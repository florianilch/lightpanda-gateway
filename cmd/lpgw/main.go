package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"

	"github.com/florianilch/lightpanda-gateway/internal/gateway"
	"github.com/florianilch/lightpanda-gateway/internal/httpfrontend"
)

func main() {
	err := run(os.Args[1:])
	if err == nil || errors.Is(err, flag.ErrHelp) {
		// parseFlags printed usage for -help.
		return
	}

	if _, ok := errors.AsType[*usageError](err); ok {
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, "lpgw:", err)
	os.Exit(1)
}

func run(args []string) error {
	opts, showVersion, err := parseFlags(args)
	if err != nil {
		return err
	}
	if showVersion {
		printVersion()
		return nil
	}

	var apiKey string
	if !opts.allowUnauthenticated {
		apiKey, err = loadAPIKey(opts.apiKeyFile)
		if err != nil {
			return err
		}
	}

	browserBinary, browserVersion, err := validateBrowserBinary(opts.browserBinary)
	if err != nil {
		return err
	}
	opts.browserBinary = browserBinary

	logOpts := &slog.HandlerOptions{Level: opts.logLevel}
	var logHandler slog.Handler = slog.NewTextHandler(os.Stdout, logOpts)
	if opts.logFormat == "json" {
		logHandler = slog.NewJSONHandler(os.Stdout, logOpts)
	}
	logger := slog.New(logHandler)
	if logger.Enabled(context.Background(), slog.LevelDebug) {
		logger.Warn("debug logging may include sensitive data")
	}

	gw, err := gateway.New(&gateway.Config{
		MaxResources:        opts.maxConcurrency,
		MaxQueueLength:      opts.maxQueuedRequests,
		QueueTimeout:        opts.queueTimeout,
		MaxResourceLifetime: opts.maxOperationLifetime,
		CDPStartupTimeout:   opts.cdpStartupTimeout,
		CDPIdleTimeout:      opts.cdpIdleTimeout,
		BrowserBinary:       opts.browserBinary,
		BrowserLaunchArgs:   opts.browserLaunchArgs,
		MaxStdoutBytes:      opts.maxStdoutBytes,
		MaxStderrBytes:      opts.maxStderrBytes,
	})
	if err != nil {
		return err
	}

	var shuttingDown atomic.Bool
	httpFrontend, err := httpfrontend.New(&httpfrontend.Config{
		Addr:                opts.addr,
		APIKey:              apiKey,
		MaxScriptBytes:      opts.maxScriptBytes,
		MaxClientOperations: opts.maxConcurrency,
	}, logger, gw, func() bool { return !shuttingDown.Load() })
	if err != nil {
		return err
	}

	buildVersion, revision := versionInfo()
	var startupAttrs []any
	if buildVersion != "" {
		startupAttrs = append(startupAttrs, "version", buildVersion)
	}
	if revision != "" {
		startupAttrs = append(startupAttrs, "revision", revision)
	}
	startupAttrs = append(startupAttrs,
		"browser_binary", opts.browserBinary,
		"browser_version", browserVersion,
		"max_concurrency", opts.maxConcurrency,
		"max_queued_requests", opts.maxQueuedRequests,
		"queue_timeout", opts.queueTimeout,
		"max_operation_lifetime", opts.maxOperationLifetime,
		"cdp_startup_timeout", opts.cdpStartupTimeout,
		"cdp_idle_timeout", opts.cdpIdleTimeout,
		"shutdown_delay", opts.shutdownDelay,
		"shutdown_grace", opts.shutdownGrace,
	)
	logger.Info("starting", startupAttrs...)

	return serve(&opts, logger, gw, httpFrontend, &shuttingDown)
}
