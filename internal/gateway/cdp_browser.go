package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/browser"
)

var (
	// ErrCDPBrowserClosed means Close began stopping the CDPBrowser. Start or Attach
	// may return it.
	ErrCDPBrowserClosed = errors.New("CDP browser closed")
	// ErrCDPStartupTimeout means CDPBrowser.Start exceeded its startup timeout.
	ErrCDPStartupTimeout = errors.New("CDP startup timeout")
	// ErrCDPConnectionLimit means a CDPBrowser reached its CDP connection limit. Attach
	// returns it without waiting.
	ErrCDPConnectionLimit = errors.New("CDP connection limit reached")
	// ErrUnexpectedCDPBrowserExit means the browser process exited before a stop was
	// requested.
	ErrUnexpectedCDPBrowserExit = errors.New("CDP browser exited unexpectedly")
)

// CDPBrowserOptions configures a CDPBrowser.
type CDPBrowserOptions struct {
	// Lifetime requests a maximum operation lifetime for this CDPBrowser. Zero adds no
	// client-specific limit. When Lifetime and Config.MaxResourceLifetime are both
	// nonzero, the shorter duration is used. Negative values are invalid.
	Lifetime time.Duration
	// MaxCDPConnections sets the CDP connection limit for this CDPBrowser. An Attach
	// call counts while opening a backend connection. A returned CDPAttachment counts
	// until Done closes it. Attach returns ErrCDPConnectionLimit without waiting when
	// the limit is reached.
	MaxCDPConnections int
}

// CDPBrowser is a Gateway resource that starts at most one browser process and owns
// its CDPAttachment values. Start returns when the process is ready for Attach. The
// process keeps running after Start returns. Each successful Attach opens one
// backend connection.
type CDPBrowser struct {
	launcher          *browser.Launcher
	addresses         *cdpAddressCoordinator
	transport         *http.Transport
	logger            *slog.Logger
	startupTimeout    time.Duration
	idleTimeout       time.Duration
	maxCDPConnections int

	// mu protects the CDPBrowser state. Lock mu before CDPAttachment.mu when both
	// locks are needed.
	mu          sync.Mutex
	startCalled bool
	stopCause   error
	waitErr     error
	cleanupErr  error

	// cancelStartup is set while Start makes the browser ready. cancelProcess remains
	// set after Start succeeds so stopping can end the process.
	cancelStartup context.CancelFunc
	cancelProcess context.CancelFunc

	// addr is the browser's CDP address. Start sets it when the browser is ready;
	// monitorProcess clears it after the process exits.
	addr string
	// attachments tracks CDPAttachment values that count toward the connection limit
	// and are closed when the browser stops.
	attachments map[*CDPAttachment]struct{}
	// attachCalls tracks Attach calls while they open a backend connection. Each
	// call counts toward the connection limit and lets stop interrupt it.
	attachCalls map[*attachCall]struct{}

	// attachmentSetup lets finish wait for Attach calls in attachCalls. A call that
	// returns no attachment closes any opened backend connection first.
	attachmentSetup sync.WaitGroup

	// startDone is closed when Start returns. If stopping begins before Start is called,
	// stop closes it.
	startDone chan struct{}

	// processDone is closed after monitorProcess calls Process.Wait, Process.Cleanup,
	// and logs the exit. Start closes it if no process starts. stop closes it if Start
	// was never called.
	processDone chan struct{}

	// finish sets err before closing done. Wait reads err after receiving from done, so
	// err does not need mu.
	done chan struct{}
	err  error
}

type attachCall struct {
	cancel context.CancelFunc
}

// BeginCDPBrowser validates opts and tries to get one Gateway slot. logger must not
// be nil. ctx controls only getting the slot. After Gateway admission, canceling ctx
// does not stop the returned CDPBrowser. The caller must call Close. Gateway may stop
// the CDPBrowser first.
//
// Gateway admission may fail with ErrQueueFull, ErrQueueTimeout, or ErrDraining.
// If ctx is canceled first, its cause is returned.
func (g *Gateway) BeginCDPBrowser(ctx context.Context, logger *slog.Logger, opts CDPBrowserOptions) (*CDPBrowser, error) {
	if logger == nil {
		return nil, errors.New("begin CDP browser: logger must not be nil")
	}
	if opts.Lifetime < 0 {
		return nil, errors.New("begin CDP browser: lifetime must not be negative")
	}
	if opts.MaxCDPConnections < 1 || opts.MaxCDPConnections > 65535 {
		return nil, errors.New("begin CDP browser: MaxCDPConnections must be between 1 and 65535")
	}

	lifetime := effectiveResourceLifetime(g.maxResourceLifetime, opts.Lifetime)
	b := &CDPBrowser{
		launcher:          g.launcher,
		addresses:         g.cdpAddresses,
		transport:         g.cdpTransport,
		logger:            logger,
		startupTimeout:    g.cdpStartupTimeout,
		idleTimeout:       g.cdpIdleTimeout,
		maxCDPConnections: opts.MaxCDPConnections,
		attachments:       make(map[*CDPAttachment]struct{}),
		attachCalls:       make(map[*attachCall]struct{}),
		startDone:         make(chan struct{}),
		processDone:       make(chan struct{}),
		done:              make(chan struct{}),
	}
	if err := g.admit(ctx, b, lifetime); err != nil {
		return nil, err
	}
	return b, nil
}

// Start starts the browser process and waits until it is ready for Attach. Its context
// controls startup only. If startup fails, Start begins stopping the browser and may
// return before the process exits. After Start succeeds, canceling ctx does not stop
// the browser.
//
// If startup times out, [errors.Is](err, ErrCDPStartupTimeout) is true. If ctx is
// canceled or the browser stops during startup, the returned error includes the
// first stop reason.
func (b *CDPBrowser) Start(ctx context.Context) (err error) {
	startupCtx, cancelStartup := context.WithTimeoutCause(
		ctx,
		b.startupTimeout,
		ErrCDPStartupTimeout,
	)
	processCtx, cancelProcess := context.WithCancel(context.Background())

	b.mu.Lock()
	if b.startCalled {
		b.mu.Unlock()
		cancelStartup()
		cancelProcess()
		return errors.New("CDP browser Start called more than once")
	}
	b.startCalled = true
	if cause := b.stopCause; cause != nil {
		b.mu.Unlock()
		cancelStartup()
		cancelProcess()
		return cause
	}
	// Publish both cancellation functions before unlocking so stop cannot miss them.
	b.cancelStartup = cancelStartup
	b.cancelProcess = cancelProcess
	b.mu.Unlock()

	// Register this defer before the defer that calls Discard. This makes startDone
	// close after Discard records any error.
	monitorStarted := false
	defer func() {
		if !monitorStarted {
			cancelProcess()
			close(b.processDone)
		}
		cancelStartup()
		close(b.startDone)
	}()

	// Keep request values for process logs after ctx is canceled.
	logCtx := context.WithoutCancel(ctx)

	// failStart stops the CDPBrowser and returns the first stop cause.
	failStart := func(cause error) error {
		b.stop(cause)
		b.mu.Lock()
		stopCause := b.stopCause
		b.mu.Unlock()
		return stopCause
	}

	if cause := context.Cause(startupCtx); cause != nil {
		return failStart(cause)
	}

	stdoutTail := &trailingCapture{limit: maxProcessTailBytes}
	stderrTail := &trailingCapture{limit: maxProcessTailBytes}
	addressWatcher := &startupAddressWatcher{
		expectedHost: browser.CDPHost,
		addr:         make(chan string, 1),
	}

	//nolint:contextcheck // The browser process must outlive a successful Start, so use processCtx, not ctx.
	cmd, prepareErr := b.launcher.NewCDPServerCmd(
		processCtx,
		browser.CDPServerOptions{MaxConnections: b.maxCDPConnections},
		lightpandaExtraEnv(nil),
		stdoutTail,
		io.MultiWriter(stderrTail, addressWatcher),
	)
	if prepareErr != nil {
		return failStart(fmt.Errorf("prepare CDP browser process: %w", prepareErr))
	}
	// Discard removes the browser working directory unless Start transfers ownership to
	// Process.
	defer func() {
		if discardErr := cmd.Discard(); discardErr != nil {
			cleanupErr := fmt.Errorf("discard browser command: %w", discardErr)
			err = errors.Join(err, cleanupErr)
			b.mu.Lock()
			b.cleanupErr = errors.Join(b.cleanupErr, cleanupErr)
			b.mu.Unlock()
			b.logger.ErrorContext(logCtx, "discard browser command", "err", discardErr)
		}
	}()

	releaseStartupGateOnce, gateErr := b.addresses.acquireStartup(startupCtx)
	if gateErr != nil {
		return failStart(gateErr)
	}

	// Start owns gate release until it launches the process monitor.
	if cause := context.Cause(startupCtx); cause != nil {
		releaseStartupGateOnce()
		return failStart(cause)
	}

	process, startErr := cmd.Start()
	if startErr != nil {
		releaseStartupGateOnce()
		return failStart(fmt.Errorf("start browser process: %w", startErr))
	}

	// Buffer one result so monitorProcess can send its exit result after Start returns.
	processResult := make(chan error, 1)
	// Start and monitorProcess may race to release the startup gate.
	go b.monitorProcess(
		logCtx,
		process,
		processResult,
		releaseStartupGateOnce,
		stdoutTail,
		stderrTail,
	)
	monitorStarted = true

	var addr string
	// Lightpanda reports its address after the CDP socket starts listening.
	select {
	case addr = <-addressWatcher.addr:
	case cause := <-processResult:
		return cause
	case <-startupCtx.Done():
		return failStart(context.Cause(startupCtx))
	}

	b.mu.Lock()
	if stopCause := b.stopCause; stopCause != nil {
		b.mu.Unlock()
		return stopCause
	}
	if startupCause := context.Cause(startupCtx); startupCause != nil {
		b.mu.Unlock()
		return failStart(startupCause)
	}

	// Register and store addr while holding b.mu. This keeps monitorProcess from
	// clearing it between the stop check and assignment.
	b.addresses.register(b, addr)
	b.addr = addr
	b.cancelStartup = nil
	b.mu.Unlock()

	releaseStartupGateOnce()
	return nil
}

// Attach opens a backend connection and returns a CDPAttachment for it. Its context
// controls only opening the backend connection. After Attach returns, canceling the
// context does not stop the CDPAttachment. On success, the caller must call
// CDPAttachment.Close. Stopping its CDPBrowser may close the attachment first.
//
// Attach returns ErrCDPConnectionLimit without waiting when
// MaxCDPConnections is reached. The call counts while opening the connection. A
// returned CDPAttachment counts until Done closes it.
func (b *CDPBrowser) Attach(ctx context.Context) (*CDPAttachment, error) {
	attachCtx, cancelAttach := context.WithCancel(ctx)
	defer cancelAttach()
	call := &attachCall{cancel: cancelAttach}

	b.mu.Lock()
	if cause := context.Cause(attachCtx); cause != nil {
		b.mu.Unlock()
		return nil, cause
	}
	if b.stopCause != nil {
		cause := b.stopCause
		b.mu.Unlock()
		return nil, cause
	}
	if b.addr == "" {
		b.mu.Unlock()
		return nil, errors.New("CDP browser is not ready")
	}
	if len(b.attachments)+len(b.attachCalls) >= b.maxCDPConnections {
		b.mu.Unlock()
		return nil, ErrCDPConnectionLimit
	}
	// Register and count the call before unlocking so stop and finish cannot miss it.
	b.attachCalls[call] = struct{}{}
	b.attachmentSetup.Add(1)
	b.mu.Unlock()
	defer b.attachmentSetup.Done()

	backendConn, dialErr := b.dialBackend(attachCtx)

	b.mu.Lock()
	stopCause := b.stopCause
	attachCause := context.Cause(attachCtx)
	// Keep the connection counted while replacing the pending call with the attachment.
	// Holding b.mu makes stop see one or the other.
	if dialErr == nil && attachCause == nil && stopCause == nil {
		attachment := &CDPAttachment{
			browser:     b,
			backendAddr: b.addr,
			done:        make(chan struct{}),
			backend:     backendConn,
			idleTimeout: b.idleTimeout,
		}
		delete(b.attachCalls, call)
		b.attachments[attachment] = struct{}{}
		b.mu.Unlock()
		return attachment, nil
	}
	var failure error
	switch {
	case stopCause != nil:
		failure = stopCause
	case attachCause != nil:
		failure = attachCause
	default:
		failure = dialErr
	}
	b.mu.Unlock()

	// Keep the call counted until any opened backend connection closes.
	if backendConn != nil {
		_ = backendConn.Close()
	}

	b.mu.Lock()
	delete(b.attachCalls, call)
	b.mu.Unlock()
	return nil, failure
}

// dialBackend opens one backend connection to the CDPBrowser's current address.
// acquireDial rechecks ownership after waiting for a dial permit. The permit blocks
// browser startup during the attempt, so address reuse cannot redirect it.
func (b *CDPBrowser) dialBackend(ctx context.Context) (*http.ClientConn, error) {
	b.mu.Lock()
	addr := b.addr
	b.mu.Unlock()
	if addr == "" {
		return nil, errors.New("CDP browser is not ready")
	}

	releaseDial, err := b.addresses.acquireDial(ctx, b, addr)
	if err != nil {
		return nil, err
	}
	defer releaseDial()

	backendConn, err := b.transport.NewClientConn(ctx, "http", addr)
	if err != nil {
		return nil, fmt.Errorf("connect browser backend: %w", err)
	}
	return backendConn, nil
}

// Done returns a channel that closes after shutdown finishes. Start and Attach calls
// in progress have returned, attachments are closed, and process cleanup and final
// logging are finished.
func (b *CDPBrowser) Done() <-chan struct{} { return b.done }

// Wait blocks until Done closes. It does not stop the CDPBrowser; call Close to stop
// it. It returns the first stop reason and any process wait or process cleanup errors.
// A normal Close returns nil.
func (b *CDPBrowser) Wait() error {
	<-b.Done()
	return b.err
}

// Close stops the CDPBrowser and waits for it to finish. It may be called before
// Start, more than once, or concurrently. A normal Close returns nil. If stopping
// began for another reason, Close includes it. Close also returns process wait and
// process cleanup errors.
func (b *CDPBrowser) Close() error {
	b.stop(ErrCDPBrowserClosed)
	return b.Wait()
}

func (b *CDPBrowser) requestStop(cause error) { b.stop(cause) }

// stop records the first cause. It starts finish in a goroutine, asks the process to
// exit, and cancels startup and Attach calls still running. Later calls do nothing.
// The cause must not be nil. stop does not wait.
func (b *CDPBrowser) stop(cause error) {
	var cancels []context.CancelFunc

	b.mu.Lock()
	if b.stopCause != nil {
		b.mu.Unlock()
		return
	}
	b.stopCause = cause
	if b.cancelStartup != nil {
		cancels = append(cancels, b.cancelStartup)
		b.cancelStartup = nil
	}
	if b.cancelProcess != nil {
		cancels = append(cancels, b.cancelProcess)
		b.cancelProcess = nil
	}
	for call := range b.attachCalls {
		cancels = append(cancels, call.cancel)
		delete(b.attachCalls, call)
	}
	attachments := make([]*CDPAttachment, 0, len(b.attachments))
	for attachment := range b.attachments {
		attachments = append(attachments, attachment)
	}

	// If Start was never called, stop must close its completion channels.
	if !b.startCalled {
		close(b.startDone)
		close(b.processDone)
	}
	b.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}
	go b.finish(cause, attachments)
}

// finish closes the attachments captured by stop. It waits for Attach and Start calls
// in progress, process exit, process cleanup, and final logging before setting b.err
// and closing done.
func (b *CDPBrowser) finish(cause error, attachments []*CDPAttachment) {
	// Close attachments concurrently because one may wait for upgrade handoff.
	// Attachment close errors do not change the browser error.
	var closeWG sync.WaitGroup
	for _, attachment := range attachments {
		closeWG.Go(func() { _ = attachment.close(cause) })
	}
	closeWG.Wait()

	// Wait for every Attach call registered before stop began to return. A call with no
	// attachment closes any opened backend connection first.
	b.attachmentSetup.Wait()

	<-b.startDone
	<-b.processDone

	b.mu.Lock()
	waitErr := b.waitErr
	cleanupErr := b.cleanupErr
	b.mu.Unlock()

	//nolint:errorlint // Exact first-stop-cause identity distinguishes a normal Close from a cause that merely wraps it.
	if cause == ErrCDPBrowserClosed {
		cause = nil
	}
	b.err = errors.Join(cause, waitErr, cleanupErr)
	close(b.done)
}

// monitorProcess owns Process.Wait and Process.Cleanup. It calls stop before clearing
// addr and releasing the startup gate, so Start cannot publish an address after
// process exit.
func (b *CDPBrowser) monitorProcess(
	logCtx context.Context,
	process *browser.Process,
	processResult chan<- error,
	releaseStartupGateOnce func(),
	stdoutTail, stderrTail *trailingCapture,
) {
	defer close(b.processDone)

	waitErr := process.Wait()

	b.stop(ErrUnexpectedCDPBrowserExit)

	b.mu.Lock()
	addr := b.addr
	b.addr = ""
	stopCause := b.stopCause
	b.mu.Unlock()
	b.addresses.unregister(b, addr)
	// Release the gate if Start has not already released it after publishing addr.
	releaseStartupGateOnce()

	// Ignore the context cancellation caused by a requested stop.
	//nolint:errorlint // Exact first-stop-cause identity records whether the process monitor, rather than another stopper, won.
	if stopCause != ErrUnexpectedCDPBrowserExit && waitErr == context.Canceled {
		waitErr = nil
	}
	if waitErr != nil {
		waitErr = fmt.Errorf("wait for browser process: %w", waitErr)
	}

	// Store wait and process cleanup errors before processDone closes.
	cleanupErr := process.Cleanup()
	if cleanupErr != nil {
		cleanupErr = fmt.Errorf("cleanup browser process: %w", cleanupErr)
	}
	b.mu.Lock()
	b.waitErr = waitErr
	if cleanupErr != nil {
		b.cleanupErr = errors.Join(b.cleanupErr, cleanupErr)
	}
	b.mu.Unlock()

	processResult <- errors.Join(stopCause, waitErr, cleanupErr)
	b.logProcessExit(logCtx, process, stopCause, waitErr, cleanupErr, stdoutTail, stderrTail)
}

func (b *CDPBrowser) logProcessExit(
	logCtx context.Context,
	process *browser.Process,
	stopCause, waitErr, cleanupErr error,
	stdoutTail, stderrTail *trailingCapture,
) {
	//nolint:errorlint // Exact first-stop-cause identity records an exit detected by the process monitor.
	exitWasUnexpected := stopCause == ErrUnexpectedCDPBrowserExit
	eventErr := errors.Join(waitErr, cleanupErr)
	if exitWasUnexpected {
		eventErr = errors.Join(ErrUnexpectedCDPBrowserExit, eventErr)
	}

	attrs := []slog.Attr{slog.Int("pid", process.PID())}
	if exitCode := process.ExitCode(); exitCode >= 0 {
		attrs = append(attrs, slog.Int("exit_code", exitCode))
	}
	if eventErr != nil {
		attrs = append(attrs, slog.Any("err", eventErr))
	}
	if b.logger.Enabled(logCtx, slog.LevelDebug) {
		attrs = append(attrs,
			slog.Group("stdout",
				slog.String("tail", stdoutTail.String()),
				slog.Bool("truncated", stdoutTail.truncated)),
			slog.Group("stderr",
				slog.String("tail", stderrTail.String()),
				slog.Bool("truncated", stderrTail.truncated)))
	}

	switch {
	case exitWasUnexpected:
		b.logger.LogAttrs(logCtx, slog.LevelError, "browser process exited unexpectedly", attrs...)
	case eventErr != nil:
		b.logger.LogAttrs(logCtx, slog.LevelError, "browser process exited", attrs...)
	default:
		b.logger.LogAttrs(logCtx, slog.LevelDebug, "browser process exited", attrs...)
	}
}

// removeAttachment removes the attachment and closes its Done channel while holding
// b.mu. A concurrent stop then either includes it in its list or sees it removed.
func (b *CDPBrowser) removeAttachment(attachment *CDPAttachment) {
	b.mu.Lock()
	delete(b.attachments, attachment)
	close(attachment.done)
	b.mu.Unlock()
}

func newCDPBackendTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Lightpanda listens on loopback. Connect directly instead of using proxy settings
	// from the environment.
	transport.Proxy = nil
	// The backend WebSocket handshake uses HTTP/1.1.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return transport
}

// startupAddressPattern matches an address field in Lightpanda's logfmt output:
//
//	$msg="server running" ... address=127.0.0.1:46455
var startupAddressPattern = regexp.MustCompile(`address\s*=\s*"?([^\s"]+)`)

// maxStartupLineBytes limits the partial line kept by startupAddressWatcher.
// It does not truncate stderr passed to the other writers.
const maxStartupLineBytes = 8 << 10

// startupAddressWatcher scans browser process stderr and reports the first valid CDP
// address.
type startupAddressWatcher struct {
	// expectedHost is the host allowed for backend connections. Other hosts are ignored
	// so process output cannot redirect the connection.
	expectedHost string

	lineBuf []byte

	// discardUntilNewline ignores the rest of an oversized line instead of scanning it
	// as a separate startup record.
	discardUntilNewline bool

	addr chan string

	// Write is called by one stderr-copy goroutine, so reported needs no mutex. Once
	// true, it ignores later addresses.
	reported bool
}

func (w *startupAddressWatcher) Write(p []byte) (int, error) {
	n := len(p)
	if w.reported {
		return n, nil
	}

	for len(p) > 0 {
		if w.discardUntilNewline {
			i := bytes.IndexByte(p, '\n')
			if i < 0 {
				return n, nil
			}
			w.discardUntilNewline = false
			p = p[i+1:]
			continue
		}

		i := bytes.IndexByte(p, '\n')
		fragment := p
		if i >= 0 {
			fragment = p[:i]
		}
		if len(fragment) > maxStartupLineBytes-len(w.lineBuf) {
			w.lineBuf = w.lineBuf[:0]
			if i < 0 {
				w.discardUntilNewline = true
				return n, nil
			}
			p = p[i+1:]
			continue
		}

		w.lineBuf = append(w.lineBuf, fragment...)
		if i < 0 {
			return n, nil
		}
		w.scan(string(w.lineBuf))
		w.lineBuf = w.lineBuf[:0]
		if w.reported {
			return n, nil
		}
		p = p[i+1:]
	}
	return n, nil
}

// scan reports an address when the line says "server running", its host matches
// expectedHost, and its port is valid. It builds the address from expectedHost and
// the reported port.
func (w *startupAddressWatcher) scan(line string) {
	if w.reported {
		return
	}
	if !strings.Contains(line, "server running") {
		return
	}
	m := startupAddressPattern.FindStringSubmatch(line)
	if m == nil {
		return
	}
	host, portText, err := net.SplitHostPort(m[1])
	if err != nil || host != w.expectedHost {
		return
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return
	}
	w.reported = true
	w.addr <- net.JoinHostPort(w.expectedHost, strconv.Itoa(port))
}
