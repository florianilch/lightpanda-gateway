package httpfrontend

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
)

func (s *Server) handleCDP(w http.ResponseWriter, r *http.Request) {
	logger := s.logger.With("request_id", requestID(r.Context()), "operation", "cdp")

	if !isWebSocketUpgrade(r) {
		writeErrorJSON(r.Context(), logger, w, http.StatusBadRequest, "expected WebSocket upgrade")
		return
	}

	// Reject requests that declare a body before the WebSocket upgrade. Lightpanda may
	// upgrade before ReverseProxy forwards the body; remaining bytes could corrupt the
	// CDP tunnel.
	if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		writeErrorJSON(r.Context(), logger, w, http.StatusBadRequest, "WebSocket upgrade request must not include a body")
		return
	}

	requestedLifetime, err := parseOptionalTimeout(r.URL.Query().Get("timeout"))
	if err != nil {
		writeErrorJSON(r.Context(), logger, w, http.StatusBadRequest, err.Error())
		return
	}

	// Start timing before Gateway admission so the duration includes Gateway queue wait.
	start := time.Now()

	browser, err := s.gateway.BeginCDPBrowser(r.Context(), logger, gateway.CDPBrowserOptions{
		Lifetime: requestedLifetime,
		// One backend connection is enough for this CDP tunnel.
		MaxCDPConnections: 1,
	})
	if err != nil {
		status := httpStatusForGatewayError(err, http.StatusInternalServerError)
		if shouldLogGatewayError(r.Context(), err) {
			logger.ErrorContext(r.Context(), "Gateway admission failed", "err", err)
		}
		writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		return
	}
	if !s.tryAcquireClientOperationSlot() {
		// Start is not called. Close the CDPBrowser before responding. This lets Gateway
		// observe completion and return its Gateway slot.
		_ = browser.Close()
		writeErrorJSON(r.Context(), logger, w, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}
	// Keep the HTTP frontend slot until the CDPAttachment closes and the established CDP
	// tunnel's closure is logged. CDPBrowser.Close may wait for the browser process to
	// exit. Release the slot before calling it.
	defer func() { _ = browser.Close() }()
	defer s.releaseClientOperationSlot()

	if err := browser.Start(r.Context()); err != nil {
		status := httpStatusForGatewayError(err, http.StatusBadGateway)
		if shouldLogGatewayError(r.Context(), err) {
			logger.ErrorContext(r.Context(), "CDP browser startup failed", "err", err)
		}
		writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		return
	}

	attachment, err := browser.Attach(r.Context())
	if err != nil {
		status := httpStatusForGatewayError(err, http.StatusBadGateway)
		if shouldLogGatewayError(r.Context(), err) {
			logger.ErrorContext(r.Context(), "CDP attachment setup failed", "err", err)
		}
		writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		return
	}

	// Close the attachment before logging tunnel closure so the log follows closure of
	// the backend connection.
	tracker := &hijackTracker{ResponseWriter: w}
	logCtx := context.WithoutCancel(r.Context())
	defer func() {
		attachmentErr := attachment.Close()

		// Log closure only for an established CDP tunnel. Before hijacking, ErrorHandler
		// writes or logs the proxy failure.
		if !tracker.hijacked() {
			return
		}
		attrs := []any{
			"duration", time.Since(start).Round(time.Millisecond),
		}
		if attachmentErr != nil {
			attrs = append(attrs, "err", attachmentErr)
		}
		logger.InfoContext(logCtx, "CDP tunnel closed", attrs...)
	}()

	// If the backend connection closes, ReverseProxy may remain blocked reading from the
	// client. Close the hijacked client connection when attachment.Done closes.
	go func() {
		<-attachment.Done()
		_ = tracker.closeHijacked()
	}()

	newCDPProxy(attachment, logger, tracker).ServeHTTP(tracker, r)
}

// isWebSocketUpgrade reports whether r is an HTTP/1.1 WebSocket upgrade with version 13
// and a base64-encoded 16-byte client key. HTTP/1.1 is required because the CDP tunnel
// uses connection hijacking.
func isWebSocketUpgrade(r *http.Request) bool {
	if r.ProtoMajor != 1 || r.ProtoMinor != 1 {
		return false
	}
	if !headerHasToken(r.Header, "Upgrade", "websocket") {
		return false
	}
	versions := r.Header.Values("Sec-WebSocket-Version")
	if len(versions) != 1 || strings.TrimSpace(versions[0]) != "13" {
		return false
	}
	keys := r.Header.Values("Sec-WebSocket-Key")
	if len(keys) != 1 {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keys[0]))
	if err != nil || len(key) != 16 {
		return false
	}
	return headerHasToken(r.Header, "Connection", "upgrade")
}

// headerHasToken checks comma-separated header values case-insensitively.
func headerHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for part := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// newCDPProxy creates a reverse proxy for one CDP tunnel. It rejects backend responses
// other than 101 Switching Protocols before ReverseProxy hijacks the client connection.
func newCDPProxy(attachment *gateway.CDPAttachment, logger *slog.Logger, tracker *hijackTracker) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: attachment,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("X-Api-Key")
			pr.Out.Header.Del("Cookie")
		},
		ModifyResponse: func(resp *http.Response) error {
			// Do not hijack after attachment closure fails or completes. Err is nil after a
			// normal Close, so check Done too.
			if err := attachment.Err(); err != nil {
				return err
			}
			select {
			case <-attachment.Done():
				return gateway.ErrCDPAttachmentClosed
			default:
			}
			// Reject non-101 responses before ReverseProxy hijacks the client connection.
			if resp.StatusCode != http.StatusSwitchingProtocols {
				return fmt.Errorf("CDP backend returned %s instead of 101 Switching Protocols", resp.Status)
			}
			return nil
		},
		// ErrorHandler handles errors returned by the proxy. ErrorLog sends proxy errors
		// logged internally through slog.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelError),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			// A hijacked client connection cannot receive an HTTP error response. handleCDP's
			// defers close the CDPAttachment and CDPBrowser.
			if tracker.hijacked() {
				logger.DebugContext(r.Context(), "CDP proxy stopped after hijack", "err", proxyErr)
				return
			}

			if r.Context().Err() != nil {
				// Do not write an HTTP error after request cancellation; the API client may no
				// longer receive the response.
				logger.DebugContext(r.Context(), "CDP proxy stopped after request cancellation", "err", proxyErr)
				return
			}

			// Prefer the attachment error. proxyErr may only report a closed backend
			// connection.
			err := attachment.Err()
			if err == nil {
				err = proxyErr
			}
			if shouldLogGatewayError(r.Context(), err) {
				logger.ErrorContext(r.Context(), "CDP proxy failed", "err", err)
			} else {
				logger.DebugContext(r.Context(), "CDP proxy stopped", "err", err)
			}

			status := httpStatusForGatewayError(err, http.StatusBadGateway)
			writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		},
	}
}

// hijackTracker records a successful hijack and retains the client connection.
// ErrorHandler uses the record to decide whether it can still write an HTTP error
// response.
type hijackTracker struct {
	http.ResponseWriter

	mu             sync.Mutex
	conn           net.Conn
	didHijack      bool
	closeRequested bool
}

var _ http.Hijacker = (*hijackTracker)(nil)

// Hijack records the client connection so closeHijacked can close it later.
func (h *hijackTracker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, buf, err := http.NewResponseController(h.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}

	h.mu.Lock()
	h.didHijack = true
	h.conn = conn
	closeRequested := h.closeRequested
	h.mu.Unlock()

	// The CDPAttachment can close while ReverseProxy upgrades the client connection.
	// closeHijacked may run before Hijack stores conn. Close conn here if so. This
	// prevents a closed CDPAttachment from leaving the client connection open.
	if closeRequested {
		_ = conn.Close()
	}
	return connWithoutCloseWrite{Conn: conn}, buf, nil
}

func (h *hijackTracker) hijacked() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.didHijack
}

// closeHijacked requests closure. If Hijack has stored the connection, it closes it now.
// Otherwise, Hijack closes it after storing it.
func (h *hijackTracker) closeHijacked() error {
	h.mu.Lock()
	h.closeRequested = true
	conn := h.conn
	h.mu.Unlock()

	if conn == nil {
		return nil
	}
	return conn.Close()
}

func (h *hijackTracker) Unwrap() http.ResponseWriter { return h.ResponseWriter }

// Hide CloseWrite so ReverseProxy closes the client connection when the backend
// connection reaches EOF. Otherwise, the API client could remain connected.
type connWithoutCloseWrite struct{ net.Conn }
