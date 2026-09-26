package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// cdpHandshakeTimeout limits how long RoundTrip waits for Lightpanda to complete
// the HTTP upgrade. Attach opens the backend connection before this timer starts.
const cdpHandshakeTimeout = 30 * time.Second

var (
	// ErrCDPAttachmentClosed means an operation could not proceed because the
	// CDPAttachment was closed.
	ErrCDPAttachmentClosed = errors.New("CDP attachment closed")
	// ErrCDPHandshakeTimeout means Lightpanda did not complete the HTTP upgrade before
	// the handshake timeout expired.
	ErrCDPHandshakeTimeout = errors.New("CDP handshake timeout")
	// ErrCDPIdleTimeout means the upgraded backend connection exceeded its idle limit.
	ErrCDPIdleTimeout = errors.New("CDP idle timeout")
)

// CDPAttachment is a Gateway handle associated with one CDPBrowser. It owns the
// backend connection opened by Attach and sends one WebSocket upgrade through it.
// Close the attachment when finished. CDPBrowser shutdown may close it first. Done
// returns a channel that closes after the backend connection closes and CDPBrowser
// removes the attachment. Err returns the cause that closed the attachment and stays
// nil after a normal Close.
//
// Closing a CDPAttachment removes it from its CDPBrowser so another Attach can open
// a backend connection. It does not stop the CDPBrowser or return the CDPBrowser's
// Gateway slot.
type CDPAttachment struct {
	// browser is the CDPBrowser that owns the attachment. CDPBrowser removes it when
	// the attachment closes. If shutdown has started, close uses CDPBrowser's stop
	// cause.
	browser     *CDPBrowser
	backendAddr string
	idleTimeout time.Duration
	// done is closed by CDPBrowser.removeAttachment while browser.mu is held.
	done chan struct{}

	// mu protects closed, err, backend, handoffDone, and lastActivity. Code holding mu
	// must not acquire browser.mu. close acquires browser.mu before mu.
	mu      sync.Mutex
	closed  bool
	err     error
	backend io.Closer
	// RoundTrip closes handoffDone after storing a 101 response body or closing a failed
	// response. When it returns a non-101 response, the caller owns its body. close waits
	// for handoffDone before removing the attachment.
	handoffDone  chan struct{}
	lastActivity time.Time
}

var (
	_ http.RoundTripper = (*CDPAttachment)(nil)
	_ io.Closer         = (*CDPAttachment)(nil)
)

// Done returns a channel that closes after the backend connection closes and the
// CDPBrowser removes the attachment.
func (a *CDPAttachment) Done() <-chan struct{} { return a.done }

// Err returns the cause recorded when the attachment starts closing. It is nil while
// the attachment is open or after a normal Close. Its result is final before Done
// closes.
func (a *CDPAttachment) Err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.err
}

// Close starts normal closure and waits for Done. If CDPBrowser shutdown or another
// error starts closure first, Close returns the recorded cause. It may be called more
// than once or concurrently. It does not stop the CDPBrowser or return its Gateway
// slot. Errors from closing the backend connection are ignored.
func (a *CDPAttachment) Close() error {
	return a.close(nil)
}

// RoundTrip sends one WebSocket upgrade through the backend connection that Attach
// opened. Call it only once. If the backend request returns an error, RoundTrip
// closes the attachment so its CDPBrowser can open another backend connection.
//
// After a 101 response, reads and writes through the upgraded response body reset the
// backend connection's idle timer.
func (a *CDPAttachment) RoundTrip(req *http.Request) (*http.Response, error) {
	a.mu.Lock()
	if a.closed {
		terminalErr := a.err
		a.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		if terminalErr == nil {
			terminalErr = ErrCDPAttachmentClosed
		}
		return nil, terminalErr
	}
	if a.handoffDone != nil {
		a.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errors.New("CDP attachment RoundTrip called more than once")
	}
	backendConn, ok := a.backend.(*http.ClientConn)
	if !ok {
		a.mu.Unlock()
		if req.Body != nil {
			_ = req.Body.Close()
		}
		invariantErr := errors.New("CDP attachment backend is not an HTTP client connection")
		if err := a.close(invariantErr); err != nil {
			return nil, err
		}
		return nil, ErrCDPAttachmentClosed
	}
	// Set handoffDone before network I/O so a concurrent close waits for RoundTrip to
	// store or close the upgraded response body.
	handoffDone := make(chan struct{})
	a.handoffDone = handoffDone
	a.mu.Unlock()

	handshakeCtx, cancelHandshake := context.WithTimeoutCause(req.Context(), cdpHandshakeTimeout, ErrCDPHandshakeTimeout)
	defer cancelHandshake()

	upgradeReq := req.Clone(handshakeCtx)
	if upgradeReq.Body != nil {
		// Make deferred Close safe if ClientConn already closed the request body.
		upgradeReq.Body = &onceReadCloser{ReadCloser: upgradeReq.Body}
		defer func() { _ = upgradeReq.Body.Close() }()
	}
	upgradeReq.URL.Scheme = "http"
	upgradeReq.URL.Host = a.backendAddr
	upgradeReq.URL.Path = "/"
	upgradeReq.URL.RawQuery = ""
	upgradeReq.RequestURI = ""
	upgradeReq.Host = a.backendAddr

	resp, transportErr := backendConn.RoundTrip(upgradeReq)
	cause := context.Cause(handshakeCtx)
	cancelHandshake()
	if cause == nil {
		cause = transportErr
	}

	// The response body must be closed before handoffDone is signaled; otherwise
	// CDPAttachment.Close could return while it remains open.
	if cause != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		close(handoffDone)
		if err := a.close(cause); err != nil {
			return nil, err
		}
		return nil, ErrCDPAttachmentClosed
	}

	// A non-101 response leaves ownership of the backend connection with the attachment.
	if resp.StatusCode != http.StatusSwitchingProtocols {
		a.mu.Lock()
		closureWon := a.closed
		terminalErr := a.err
		a.mu.Unlock()
		if closureWon {
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
			close(handoffDone)
			<-a.done
			if terminalErr != nil {
				return nil, terminalErr
			}
			return nil, ErrCDPAttachmentClosed
		}
		close(handoffDone)
		return resp, nil
	}

	// Wrap the response body so Close is safe to call more than once. If it supports
	// reads and writes, also record activity for the idle timer.
	trackActivity := false
	if stream, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &activityReadWriteCloser{ReadWriteCloser: stream, recordActivity: a.recordActivity}
		trackActivity = true
	} else {
		resp.Body = &onceReadCloser{ReadCloser: resp.Body}
	}

	upgradedBackend := resp.Body

	// The attachment owns the upgraded response body. If the attachment closed first,
	// close the body before signaling handoffDone.
	a.mu.Lock()
	closureWon := a.closed
	terminalErr := a.err
	if !closureWon {
		a.backend = upgradedBackend
	}
	a.mu.Unlock()
	if closureWon {
		_ = upgradedBackend.Close()
		close(handoffDone)
		<-a.done
		if terminalErr != nil {
			return nil, terminalErr
		}
		return nil, ErrCDPAttachmentClosed
	}

	close(handoffDone)
	if trackActivity {
		a.recordActivity()
		a.startIdleWatch()
	}
	return resp, nil
}

// close starts closing the attachment with cause. It closes the backend connection and
// waits for any RoundTrip handoff before removing the attachment. This prevents the
// CDPBrowser from removing the attachment while RoundTrip is still running. If another
// call starts closure first, it waits for Done and returns the recorded cause. A nil
// cause means normal closure.
func (a *CDPAttachment) close(cause error) error {
	// Always lock browser.mu before a.mu. This order prevents deadlock. A stop cause
	// recorded by CDPBrowser takes precedence over cause.
	a.browser.mu.Lock()
	a.mu.Lock()
	if a.closed {
		err := a.err
		a.mu.Unlock()
		a.browser.mu.Unlock()

		<-a.done
		return err
	}
	if a.browser.stopCause != nil {
		cause = a.browser.stopCause
	}

	a.err = cause
	a.closed = true

	backendToClose := a.backend
	a.backend = nil
	handoffDone := a.handoffDone

	a.mu.Unlock()
	a.browser.mu.Unlock()

	if backendToClose != nil {
		_ = backendToClose.Close()
	}
	if handoffDone != nil {
		<-handoffDone
	}

	a.browser.removeAttachment(a)
	return cause
}

func (a *CDPAttachment) recordActivity() {
	a.mu.Lock()
	a.lastActivity = time.Now()
	a.mu.Unlock()
}

// startIdleWatch starts an idle timer when idleTimeout is positive. It closes the
// attachment when the upgraded backend connection is idle for that duration.
func (a *CDPAttachment) startIdleWatch() {
	if a.idleTimeout <= 0 {
		return
	}
	go func() {
		timer := time.NewTimer(a.idleTimeout)
		defer timer.Stop()
		for {
			select {
			case <-a.Done():
				return
			case <-timer.C:
			}
			// recordActivity and this check use the same mutex. Once this check sees an
			// expired idle deadline, later activity cannot prevent closure.
			a.mu.Lock()
			remaining := a.idleTimeout - time.Since(a.lastActivity)
			a.mu.Unlock()
			if remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			_ = a.close(fmt.Errorf("%w: exceeded %s", ErrCDPIdleTimeout, a.idleTimeout))
			return
		}
	}()
}

// activityReadWriteCloser records activity when reads or writes transfer bytes on the
// upgraded backend connection. This resets the idle timer. Close closes the underlying
// stream at most once.
type activityReadWriteCloser struct {
	io.ReadWriteCloser
	recordActivity func()
	closeOnce      sync.Once
	closeErr       error
}

func (c *activityReadWriteCloser) Read(b []byte) (int, error) {
	n, err := c.ReadWriteCloser.Read(b)
	if n > 0 {
		c.recordActivity()
	}
	return n, err
}

func (c *activityReadWriteCloser) Write(b []byte) (int, error) {
	n, err := c.ReadWriteCloser.Write(b)
	if n > 0 {
		c.recordActivity()
	}
	return n, err
}

// Close closes the wrapped stream once.
func (c *activityReadWriteCloser) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.ReadWriteCloser.Close() })
	return c.closeErr
}

// onceReadCloser makes Close safe to call more than once.
type onceReadCloser struct {
	io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

// Close closes the wrapped reader once.
func (c *onceReadCloser) Close() error {
	c.closeOnce.Do(func() { c.closeErr = c.ReadCloser.Close() })
	return c.closeErr
}
