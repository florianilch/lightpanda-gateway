package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
	"github.com/florianilch/lightpanda-gateway/internal/httpfrontend"
)

// serve runs the HTTP frontend until it stops or a shutdown signal arrives, then
// shuts down the HTTP frontend and Gateway.
func serve(opts *options, logger *slog.Logger, gw *gateway.Gateway, httpFrontend *httpfrontend.Server, shuttingDown *atomic.Bool) error {
	// SIGINT, SIGTERM, and SIGHUP request graceful shutdown.
	sigCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stopSignals()

	if err := httpFrontend.Listen(); err != nil {
		return err
	}

	// Serve may fail before shutdown starts. Keep its result so serve can return that
	// failure after cleanup finishes.
	httpServeErrCh := make(chan error, 1)
	go func() {
		err := httpFrontend.Serve()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("serve HTTP frontend: %w", err)
		}
		httpServeErrCh <- err
	}()
	logger.InfoContext(sigCtx, "listening", "addr", httpFrontend.Addr().String())

	var httpServeErr error
	httpServeDone := false
	select {
	case httpServeErr = <-httpServeErrCh:
		httpServeDone = true
	case <-sigCtx.Done():
	}
	// Stop intercepting signals. A later signal can terminate the process if shutdown hangs.
	stopSignals()

	// The HTTP frontend reads shuttingDown for /readyz. Set it before the delay so
	// readiness probes fail while the listener remains open.
	shuttingDown.Store(true)
	logger.Info("shutdown started")

	if opts.shutdownDelay > 0 && !httpServeDone {
		delayCtx, cancelDelay := context.WithTimeout(context.Background(), opts.shutdownDelay)
		select {
		case <-delayCtx.Done():
		case httpServeErr = <-httpServeErrCh:
			httpServeDone = true
		}
		cancelDelay()
	}

	if opts.shutdownGrace > 0 {
		gracefulCtx, cancelGraceful := context.WithTimeout(context.Background(), opts.shutdownGrace)
		var gatewayGracefulErr, httpGracefulErr error
		var gracefulWG sync.WaitGroup
		gracefulWG.Go(func() { gatewayGracefulErr = gw.Shutdown(gracefulCtx) })
		gracefulWG.Go(func() { httpGracefulErr = httpFrontend.Shutdown(gracefulCtx) })
		gracefulWG.Wait()
		cancelGraceful()

		if gatewayGracefulErr != nil {
			logger.Warn("graceful gateway shutdown incomplete", "err", gatewayGracefulErr)
		}
		if httpGracefulErr != nil {
			logger.Warn("graceful HTTP frontend shutdown failed", "err", httpGracefulErr)
		}
		if gatewayGracefulErr == nil && httpGracefulErr == nil {
			if !httpServeDone {
				httpServeErr = <-httpServeErrCh
			}
			logger.Info("shutdown complete")
			return httpServeErr
		}
	}

	logger.Info("forcing shutdown")
	// Stop Gateway resources before closing HTTP connections. Closing a HTTP connection
	// can make a /ws or /scripts handler finish and close its Gateway resource.
	// A Gateway resource retains its first cancellation cause. gateway.Stop records
	// server shutdown before httpFrontend.Close can record client closure.
	gw.Stop()
	httpCloseErr := httpFrontend.Close()

	// gateway.Stop has asked Gateway resources to stop. httpFrontend.Close has
	// closed connections. Allow resource cleanup and remaining HTTP client operations
	// up to 30 seconds to finish.
	const forcedShutdownTimeout = 30 * time.Second
	forcedCtx, cancelForced := context.WithTimeout(context.Background(), forcedShutdownTimeout)
	var gatewayForcedErr, httpForcedErr error
	var forcedWG sync.WaitGroup
	forcedWG.Go(func() { gatewayForcedErr = gw.Shutdown(forcedCtx) })
	forcedWG.Go(func() { httpForcedErr = httpFrontend.Shutdown(forcedCtx) })
	forcedWG.Wait()
	cancelForced()

	if !httpServeDone {
		httpServeErr = <-httpServeErrCh
	}
	if httpCloseErr != nil && httpForcedErr == nil {
		logger.Warn("forced HTTP frontend close failed but HTTP shutdown completed", "err", httpCloseErr)
	}
	if gatewayForcedErr == nil && httpForcedErr == nil {
		logger.Info("shutdown complete")
	}

	resultErr := httpServeErr
	if gatewayForcedErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("wait for gateway shutdown after forced stop: %w", gatewayForcedErr))
	}
	if httpForcedErr != nil {
		if httpCloseErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close HTTP frontend: %w", httpCloseErr))
		}
		resultErr = errors.Join(resultErr, fmt.Errorf("wait for HTTP frontend shutdown after forced close: %w", httpForcedErr))
	}
	return resultErr
}
