package httpfrontend

import (
	"encoding/json/v2"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
)

// scriptJSONRequest is the JSON request body.
//
//	{
//	  "script": "const p = new Page(); await p.goto(\"$LP_TARGET\"); return p.extract({title: \"h1\"});",
//	  "timeout": "30s",
//	  "secrets": {"LP_TARGET": "https://example.com"}
//	}
type scriptJSONRequest struct {
	Script  string            `json:"script"`
	Timeout string            `json:"timeout"`
	Secrets map[string]string `json:"secrets"`
}

// scriptResponse is the JSON response for a completed script execution.
// JSON replaces invalid UTF-8 bytes in output, so the response can
// differ from the original bytes.
type scriptResponse struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr,omitzero"`
	// ExitCode is always present. Zero means success.
	// -1 means the browser process exited without a numeric code, such as after
	// receiving a signal.
	ExitCode        int    `json:"exit_code"`
	StdoutTruncated bool   `json:"stdout_truncated,omitzero"`
	StderrTruncated bool   `json:"stderr_truncated,omitzero"`
	Error           string `json:"error,omitzero"`
}

func (s *Server) handleScript(w http.ResponseWriter, r *http.Request) {
	logger := s.logger.With("request_id", requestID(r.Context()), "operation", "script")

	req, status, err := readScriptRequest(r)
	if err != nil {
		logger.InfoContext(r.Context(), "script request rejected", "status", status, "err", err)
		writeErrorJSON(r.Context(), logger, w, status, err.Error())
		return
	}

	// Start timing before Gateway admission to include Gateway slot wait time in
	// the logged duration.
	start := time.Now()

	call, err := s.gateway.BeginScriptCall(r.Context(), logger, req)
	if err != nil {
		status := httpStatusForGatewayError(err, http.StatusInternalServerError)
		if shouldLogGatewayError(r.Context(), err) {
			logger.ErrorContext(r.Context(), "Gateway admission failed", "err", err)
		}
		writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		return
	}
	if !s.tryAcquireClientOperationSlot() {
		// Close the ScriptCall so Gateway observes completion before responding.
		_ = call.Close()
		writeErrorJSON(r.Context(), logger, w, http.StatusServiceUnavailable, http.StatusText(http.StatusServiceUnavailable))
		return
	}
	defer func() { _ = call.Close() }()
	// Keep the HTTP frontend slot until response delivery and final logging finish.
	defer s.releaseClientOperationSlot()

	result, runErr := call.Run(r.Context())
	if result == nil {
		status := httpStatusForGatewayError(runErr, http.StatusBadGateway)
		if shouldLogGatewayError(r.Context(), runErr) {
			logger.ErrorContext(r.Context(), "script call failed", "err", runErr)
		}
		writeErrorJSON(r.Context(), logger, w, status, http.StatusText(status))
		return
	}

	// A result means the response status is 200, even for a nonzero exit status.
	// Process cleanup errors do not change the response.
	var failure string
	status = http.StatusOK
	if errors.Is(runErr, gateway.ErrScriptFailed) {
		failure = "script failed"
	}

	writeJSON(r.Context(), logger, w, status, scriptResponse{
		Stdout:          result.Stdout,
		Stderr:          result.Stderr,
		ExitCode:        result.ExitCode,
		StdoutTruncated: result.StdoutTruncated,
		StderrTruncated: result.StderrTruncated,
		Error:           failure,
	})

	logger.InfoContext(r.Context(), "script response sent",
		"exit_code", result.ExitCode,
		"duration", time.Since(start).Round(time.Millisecond),
		"status", status)
}

// readScriptRequest parses and validates a script request.
// On error, it returns the corresponding HTTP status.
func readScriptRequest(r *http.Request) (gateway.ScriptRequest, int, error) {
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return gateway.ScriptRequest{}, http.StatusBadRequest, errors.New("invalid Content-Type")
	}

	var req gateway.ScriptRequest
	var status int
	switch mediaType {
	case "application/json":
		req, status, err = readJSONScriptRequest(r)
	case "application/javascript", "text/javascript", "text/plain":
		req, status, err = readRawScriptRequest(r)
	default:
		return gateway.ScriptRequest{}, http.StatusUnsupportedMediaType, errors.New("unsupported Content-Type")
	}
	if err != nil {
		return req, status, err
	}
	if err := req.Validate(); err != nil {
		return req, http.StatusBadRequest, err
	}
	return req, http.StatusOK, nil
}

// readJSONScriptRequest decodes exactly one JSON value from r.Body.
func readJSONScriptRequest(r *http.Request) (gateway.ScriptRequest, int, error) {
	var wire scriptJSONRequest
	if err := json.UnmarshalRead(r.Body, &wire, json.RejectUnknownMembers(true)); err != nil {
		status, bodyErr := scriptBodyError(err)
		return gateway.ScriptRequest{}, status, bodyErr
	}

	requestedTimeout, err := parseOptionalTimeout(wire.Timeout)
	if err != nil {
		return gateway.ScriptRequest{}, http.StatusBadRequest, err
	}
	return gateway.ScriptRequest{
		Script:  wire.Script,
		Secrets: wire.Secrets,
		Timeout: requestedTimeout,
	}, http.StatusOK, nil
}

// readRawScriptRequest reads JavaScript source from r.Body.
// It gets an optional timeout from the timeout query parameter.
func readRawScriptRequest(r *http.Request) (gateway.ScriptRequest, int, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		status, bodyErr := scriptBodyError(err)
		return gateway.ScriptRequest{}, status, bodyErr
	}
	requestedTimeout, err := parseOptionalTimeout(r.URL.Query().Get("timeout"))
	if err != nil {
		return gateway.ScriptRequest{}, http.StatusBadRequest, err
	}
	return gateway.ScriptRequest{
		Script:  string(body),
		Timeout: requestedTimeout,
	}, http.StatusOK, nil
}

// scriptBodyError returns an HTTP status and message for a request-body error.
func scriptBodyError(err error) (int, error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge, errors.New("request body too large")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return http.StatusRequestTimeout, errors.New("request body timed out")
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return http.StatusRequestTimeout, errors.New("request body timed out")
	}
	return http.StatusBadRequest, errors.New("invalid request body")
}
