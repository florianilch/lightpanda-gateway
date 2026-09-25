package httpfrontend

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"uuid"
)

// chain wraps h with middlewares. The first middleware is outermost.
func chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for _, m := range slices.Backward(middlewares) {
		h = m(h)
	}
	return h
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

type requestIDContextKey struct{}

var requestIDKey requestIDContextKey

// withRequestID validates X-Request-ID and generates an ID if it is absent or invalid.
// It adds the ID to the response header and request context before calling next.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" || !requestIDPattern.MatchString(id) {
			id = uuid.NewV4().String()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// requireAPIKey authenticates requests with a Bearer token, X-Api-Key header, or token
// query parameter. Missing or invalid credentials return 401 Unauthorized.
func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const bearerPrefix = "Bearer "

		token := ""
		if h := r.Header.Get("Authorization"); len(h) > len(bearerPrefix) && strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
			token = strings.TrimSpace(h[len(bearerPrefix):])
		}
		if token == "" {
			token = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}

		tokenHash := sha256.Sum256([]byte(token))
		if s.apiKeyHash == nil || subtle.ConstantTimeCompare(s.apiKeyHash[:], tokenHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
			logger := s.logger.With("request_id", requestID(r.Context()))
			writeErrorJSON(r.Context(), logger, w, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
			return
		}
		next.ServeHTTP(w, r)
	})
}
