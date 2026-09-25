package httpfrontend

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
)

const (
	readHeaderTimeout = 10 * time.Second
	httpReadTimeout   = 30 * time.Second
	httpIdleTimeout   = 2 * time.Minute
)

// Config contains HTTP frontend settings.
type Config struct {
	// Addr is the TCP address where the HTTP frontend listens. It must not be empty.
	Addr string
	// APIKey authenticates API clients. An empty value disables API-key authentication.
	APIKey string
	// MaxScriptBytes is the maximum script request-body size. It must be at least 1.
	MaxScriptBytes int64
	// MaxClientOperations limits HTTP client operations. After Gateway admission, the
	// HTTP frontend takes an HTTP frontend slot without waiting and rejects the request
	// if none is free. It must be at least 1.
	MaxClientOperations int
}

// Server serves the HTTP frontend.
type Server struct {
	addr           string
	maxScriptBytes int64
	apiKeyHash     *[sha256.Size]byte
	logger         *slog.Logger
	gateway        *gateway.Gateway
	server         *http.Server
	listener       net.Listener

	clientOperationSlots chan struct{}

	// clientOperationMu protects clientOperationsDraining.
	clientOperationMu        sync.Mutex
	clientOperationsDraining bool
	clientOperationWG        sync.WaitGroup
	clientOperationDrainOnce sync.Once
	clientOperationsDone     chan struct{}
}

// New validates cfg and returns a Server. isReady supplies the state for /readyz.
func New(cfg *Config, logger *slog.Logger, gw *gateway.Gateway, isReady func() bool) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("httpfrontend: config is nil")
	}
	if cfg.Addr == "" {
		return nil, errors.New("httpfrontend: address is required")
	}
	if cfg.MaxScriptBytes < 1 {
		return nil, errors.New("httpfrontend: max script bytes must be positive")
	}
	if cfg.MaxClientOperations < 1 {
		return nil, errors.New("httpfrontend: MaxClientOperations must be >= 1")
	}
	if logger == nil {
		return nil, errors.New("httpfrontend: logger is nil")
	}
	if gw == nil {
		return nil, errors.New("httpfrontend: gateway is nil")
	}
	if isReady == nil {
		return nil, errors.New("httpfrontend: readiness function is nil")
	}

	var apiKeyHash *[sha256.Size]byte
	if cfg.APIKey != "" {
		// The Server stores only the API-key hash. Hashing each presented key produces
		// fixed-size values for a constant-time comparison.
		hash := sha256.Sum256([]byte(cfg.APIKey))
		apiKeyHash = &hash
	}

	frontend := &Server{
		addr:                 cfg.Addr,
		maxScriptBytes:       cfg.MaxScriptBytes,
		apiKeyHash:           apiKeyHash,
		logger:               logger,
		gateway:              gw,
		clientOperationSlots: make(chan struct{}, cfg.MaxClientOperations),
		clientOperationsDone: make(chan struct{}),
	}

	httpServer := &http.Server{
		Handler: frontend.routes(isReady),
		// ReadHeaderTimeout and ReadTimeout limit time spent receiving a request.
		// Leave WriteTimeout unset because it includes time before response delivery.
		// writeJSON sets a per-response deadline when delivery begins.
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		IdleTimeout:       httpIdleTimeout,
		// net/http reports server errors through ErrorLog. Log them at Warn because they
		// may not stop the server.
		ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	frontend.server = httpServer
	return frontend, nil
}

// Listen binds the configured TCP address. It must succeed before Serve is called and
// cannot be called again after it succeeds.
func (s *Server) Listen() error {
	if s.listener != nil {
		return errors.New("httpfrontend: already listening")
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", s.addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.addr, err)
	}
	s.listener = ln
	return nil
}

// Addr returns the listener's address, or nil before Listen succeeds.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Serve accepts client connections from the listener established by Listen and serves
// their requests. It returns [http.ErrServerClosed] after Shutdown or Close.
func (s *Server) Serve() error {
	if s.listener == nil {
		return errors.New("httpfrontend: server is not listening")
	}
	return s.server.Serve(s.listener)
}

// Shutdown prevents new HTTP client operations and shuts down the HTTP server. It waits
// for existing HTTP client operations to finish or ctx to expire.
func (s *Server) Shutdown(ctx context.Context) error {
	clientOperationsDone := s.beginClientOperationDrain()
	serverErr := s.server.Shutdown(ctx)
	// Check clientOperationsDone before waiting on ctx. If it is already closed,
	// return the result from [http.Server.Shutdown] even if ctx is also done.
	select {
	case <-clientOperationsDone:
		return serverErr
	default:
	}
	select {
	case <-clientOperationsDone:
		return serverErr
	case <-ctx.Done():
		return errors.Join(serverErr, ctx.Err())
	}
}

// Close prevents new HTTP client operations. It closes the listener even if Serve was
// never called and force-closes client connections managed by net/http.
func (s *Server) Close() error {
	s.beginClientOperationDrain()
	serverErr := s.server.Close()
	if s.listener == nil {
		return serverErr
	}

	listenerErr := s.listener.Close()
	if errors.Is(listenerErr, net.ErrClosed) {
		listenerErr = nil
	}
	return errors.Join(serverErr, listenerErr)
}

func (s *Server) routes(isReady func() bool) http.Handler {
	mux := http.NewServeMux()
	middlewares := []func(http.Handler) http.Handler{
		withRequestID,
	}
	if s.apiKeyHash != nil {
		middlewares = append(middlewares, s.requireAPIKey)
	}

	scriptHandler := http.MaxBytesHandler(http.HandlerFunc(s.handleScript), s.maxScriptBytes)
	mux.Handle("POST /scripts", chain(scriptHandler, middlewares...))

	mux.HandleFunc("GET /healthz", healthHandler)
	mux.Handle("GET /readyz", readyHandler(isReady))
	mux.Handle("GET /metrics", s.metricsHandler())
	return mux
}

// tryAcquireClientOperationSlot takes an HTTP frontend slot without waiting. It returns
// false if no slot is free or shutdown has begun. A successful call must be paired with
// releaseClientOperationSlot.
func (s *Server) tryAcquireClientOperationSlot() bool {
	// Hold the mutex until clientOperationWG.Add returns. Otherwise beginClientOperationDrain
	// could call clientOperationWG.Wait after taking the slot but before adding the HTTP client
	// operation to the WaitGroup.
	s.clientOperationMu.Lock()
	defer s.clientOperationMu.Unlock()

	if s.clientOperationsDraining {
		return false
	}
	select {
	case s.clientOperationSlots <- struct{}{}:
		s.clientOperationWG.Add(1)
		return true
	default:
		return false
	}
}

// releaseClientOperationSlot returns an HTTP frontend slot and marks the HTTP client
// operation done for Shutdown.
func (s *Server) releaseClientOperationSlot() {
	<-s.clientOperationSlots
	s.clientOperationWG.Done()
}

// beginClientOperationDrain prevents new HTTP client operations. It returns a channel
// that closes after all current HTTP client operations return their HTTP frontend slots.
func (s *Server) beginClientOperationDrain() <-chan struct{} {
	s.clientOperationDrainOnce.Do(func() {
		s.clientOperationMu.Lock()
		s.clientOperationsDraining = true
		s.clientOperationMu.Unlock()

		go func() {
			s.clientOperationWG.Wait()
			close(s.clientOperationsDone)
		}()
	})
	return s.clientOperationsDone
}

// parseOptionalTimeout parses an optional non-negative duration. A zero result leaves
// the operation lifetime to Gateway.
func parseOptionalTimeout(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout < 0 {
		return 0, errors.New("timeout must be a non-negative duration, e.g. 90s or 0")
	}
	return timeout, nil
}
