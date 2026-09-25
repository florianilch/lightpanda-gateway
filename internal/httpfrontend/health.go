package httpfrontend

import "net/http"

// healthHandler reports liveness without checking dependencies.
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// readyHandler returns a handler that reports whether the HTTP frontend can receive new traffic.
func readyHandler(isReady func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !isReady() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}
