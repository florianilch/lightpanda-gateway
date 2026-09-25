package httpfrontend

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
)

// responseWriteTimeout limits the time spent writing and flushing a response.
// The HTTP server leaves WriteTimeout unset because it includes time before
// response delivery.
const responseWriteTimeout = 30 * time.Second

type errorResponse struct {
	Error string `json:"error"`
}

// httpStatusForGatewayError maps known Gateway and context errors to HTTP statuses.
// It returns defaultStatus when err has no mapping.
func httpStatusForGatewayError(err error, defaultStatus int) int {
	switch {
	case errors.Is(err, gateway.ErrQueueFull),
		errors.Is(err, gateway.ErrQueueTimeout),
		errors.Is(err, gateway.ErrDraining),
		errors.Is(err, gateway.ErrShutdown):
		return http.StatusServiceUnavailable

	case errors.Is(err, gateway.ErrResourceTimeout):
		return http.StatusGatewayTimeout

		// Request cancellation is handled when writing the response. A context error here
		// means the Gateway operation did not complete, so return 503.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable

	default:
		return defaultStatus
	}
}

// shouldLogGatewayError reports whether err needs an error-level log. It suppresses
// expected Gateway errors and context errors from request cancellation.
func shouldLogGatewayError(ctx context.Context, err error) bool {
	switch {
	// These errors are expected when capacity is full, a timeout expires, or shutdown
	// begins. Logging each at error level adds noise.
	case errors.Is(err, gateway.ErrQueueFull),
		errors.Is(err, gateway.ErrQueueTimeout),
		errors.Is(err, gateway.ErrDraining),
		errors.Is(err, gateway.ErrShutdown),
		errors.Is(err, gateway.ErrResourceTimeout):
		return false

		// A context error is expected when the request context is done. If the request
		// context is still active, another component canceled or timed out the Gateway
		// operation. Log the error.
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return ctx.Err() == nil

	default:
		return true
	}
}

// writeJSON writes v as JSON and flushes the response. It returns without starting
// a response if ctx is already canceled.
//
// writeJSON panics with [http.ErrAbortHandler] when response preparation or delivery fails.
// net/http aborts the response without logging a stack trace.
// Deferred functions in the handler run before net/http handles the panic.
func writeJSON(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, code int, v any) {
	if ctx.Err() != nil {
		return
	}

	body, err := json.Marshal(v, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		logger.ErrorContext(ctx, "failed to marshal HTTP response", "err", err)
		panic(http.ErrAbortHandler)
	}

	controller := http.NewResponseController(w)
	// Use the earlier of the responseWriteTimeout and context deadline for response writes and
	// flushes.
	deadline := time.Now().Add(responseWriteTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	// SetWriteDeadline needs the network connection. Test response writers and some
	// middleware wrappers do not expose one. Continue without a deadline when it
	// returns http.ErrNotSupported.
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		logger.ErrorContext(ctx, "failed to set HTTP response write deadline", "err", err)
		panic(http.ErrAbortHandler)
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(code)
	if _, err := w.Write(body); err != nil {
		logger.DebugContext(ctx, "failed to write HTTP response", "err", err)
		panic(http.ErrAbortHandler)
	}
	if err := controller.Flush(); err != nil {
		logger.DebugContext(ctx, "failed to flush HTTP response", "err", err)
		panic(http.ErrAbortHandler)
	}
}

// writeErrorJSON writes a JSON response with message in its error field.
func writeErrorJSON(ctx context.Context, logger *slog.Logger, w http.ResponseWriter, code int, message string) {
	writeJSON(ctx, logger, w, code, errorResponse{Error: message})
}
