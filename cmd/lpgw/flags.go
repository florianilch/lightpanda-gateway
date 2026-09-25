package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// options holds command settings read from flags and environment variables.
type options struct {
	addr                 string
	browserBinary        string
	apiKeyFile           string
	allowUnauthenticated bool
	// maxConcurrency configures two independent limits with the same value:
	// admitted Gateway resources and accepted HTTP /ws and /scripts operations.
	maxConcurrency       int
	queueTimeout         time.Duration
	maxOperationLifetime time.Duration
	maxScriptBytes       int64
	// maxQueuedRequests limits how many validated requests may wait for a Gateway slot.
	// Zero disables waiting.
	maxQueuedRequests int
	maxStdoutBytes    int
	maxStderrBytes    int
	shutdownDelay     time.Duration
	shutdownGrace     time.Duration
	logLevel          slog.Level
	logFormat         string

	// browserLaunchArgs contains extra arguments for managed Lightpanda commands.
	// Each entry is passed as one command-line argument.
	browserLaunchArgs []string
}

// usageError marks an invalid command-line invocation.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }

func (e *usageError) Unwrap() error { return e.err }

// parseFlags reads command-line flags and their LPGW_* environment variables.
// Command-line flags override environment values. Valid environment values
// override built-in defaults. An invalid environment value is ignored when the
// matching command-line flag is set.
//
// parseFlags validates settings except the Lightpanda binary. validateBrowserBinary
// checks that binary later. gateway.New and httpfrontend.New validate their settings.
//
// Invalid command-line invocations return *usageError. A help request prints
// usage and returns [flag.ErrHelp].
func parseFlags(args []string) (options, bool, error) {
	var (
		opts        options
		showVersion bool
	)

	fs := flag.NewFlagSet("lpgw", flag.ContinueOnError)
	fs.BoolVar(&showVersion, "version", false, "print version and exit")
	fs.StringVar(&opts.addr, "addr", ":8080", "listen address")
	fs.StringVar(&opts.apiKeyFile, "api-key-file", "", "path to a file containing the API key")
	fs.BoolVar(&opts.allowUnauthenticated, "allow-unauthenticated", false, "disable API-key authentication")
	fs.IntVar(&opts.maxConcurrency, "max-concurrency", 1, "maximum concurrent /ws and /scripts operations")
	fs.DurationVar(&opts.queueTimeout, "queue-timeout", 10*time.Second, "maximum time a request may wait in the Gateway queue; 0 has no deadline")
	fs.DurationVar(&opts.maxOperationLifetime, "max-operation-lifetime", 30*time.Minute, "time after which an operation is asked to stop; 0 disables the limit, clients can request a shorter lifetime")
	fs.Int64Var(&opts.maxScriptBytes, "max-script-bytes", 1<<20, "max request body for /scripts")
	fs.IntVar(&opts.maxQueuedRequests, "max-queued-requests", 5,
		"maximum number of validated requests that may wait to start; 0 disables queueing")
	fs.IntVar(&opts.maxStdoutBytes, "max-stdout-bytes", 4<<20, "max stdout bytes returned by /scripts; 0 returns no stdout")
	fs.IntVar(&opts.maxStderrBytes, "max-stderr-bytes", 256<<10, "max stderr bytes returned by /scripts; 0 returns no stderr")
	fs.DurationVar(&opts.shutdownDelay, "shutdown-delay", 0,
		"on signal shutdown, delay before graceful shutdown so readiness checks can fail first; 0 disables the delay")
	fs.DurationVar(&opts.shutdownGrace, "shutdown-grace", 20*time.Second,
		"after any -shutdown-delay, grace period for existing work to finish before forced cancellation; 0 skips the grace period")
	fs.TextVar(&opts.logLevel, "log-level", slog.LevelInfo, "debug, info, warn, error; debug may include sensitive data")
	fs.StringVar(&opts.logFormat, "log-format", "text", "text or json")
	fs.StringVar(&opts.browserBinary, "browser-binary", "lightpanda", "path to the browser binary to manage")
	// browserLaunchArgsFromEnv makes the first parsed flag replace environment defaults.
	var browserLaunchArgsFromEnv bool
	fs.Func("browser-launch-arg", "one argument appended to managed serve/run invocations; may be repeated; arguments must be accepted by both commands (env LPGW_BROWSER_LAUNCH_ARGS is a JSON string array)", func(value string) error {
		if err := validateBrowserLaunchArg(value); err != nil {
			return err
		}
		if browserLaunchArgsFromEnv {
			opts.browserLaunchArgs = nil
			browserLaunchArgsFromEnv = false
		}
		opts.browserLaunchArgs = append(opts.browserLaunchArgs, value)
		return nil
	})

	// Apply valid environment values before parsing flags so command-line flags can override
	// them. Keep invalid-environment errors until after parsing, when we know whether the
	// matching flag was set.
	envErrs := applyEnvDefaults(fs)
	browserLaunchArgsFromEnv = true

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, false, flag.ErrHelp
		}
		return opts, false, &usageError{err: err}
	}
	if fs.NArg() != 0 {
		err := fmt.Errorf("unexpected arguments: %q", fs.Args())
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}
	if showVersion {
		return opts, true, nil
	}

	overridden := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { overridden[f.Name] = true })

	errs := make([]error, 0, len(envErrs))
	for _, envErr := range envErrs {
		if !overridden[envErr.flagName] {
			errs = append(errs, envErr.err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}

	if opts.addr == "" {
		err := errors.New("config: -addr must not be empty")
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}
	if opts.browserBinary == "" {
		err := errors.New("config: -browser-binary must not be empty")
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}
	if opts.logFormat != "text" && opts.logFormat != "json" {
		err := fmt.Errorf("config: invalid -log-format %q; want %q or %q", opts.logFormat, "text", "json")
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}
	if err := validateLimits(&opts); err != nil {
		fmt.Fprintln(fs.Output(), err)
		fs.Usage()
		return opts, false, &usageError{err: err}
	}

	return opts, false, nil
}

// loadAPIKey reads apiKeyFile, or LPGW_API_KEY when apiKeyFile is empty. It trims
// surrounding whitespace and rejects an empty key or a key containing a NUL byte or
// line break.
func loadAPIKey(apiKeyFile string) (string, error) {
	var key string
	if apiKeyFile != "" {
		data, err := os.ReadFile(apiKeyFile) // #nosec G304 -- operator-supplied startup path for API-key configuration
		if err != nil {
			return "", fmt.Errorf("config: read API key file %q: %w", apiKeyFile, err)
		}
		key = string(data)
	} else {
		key = os.Getenv("LPGW_API_KEY")
	}

	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("config: API key is required")
	}
	if strings.ContainsAny(key, "\x00\r\n") {
		return "", errors.New("config: API key contains a NUL byte or line break")
	}
	return key, nil
}

func validateBrowserLaunchArg(value string) error {
	if value == "" {
		return errors.New("argument must not be empty")
	}
	if strings.ContainsRune(value, 0) {
		return errors.New("argument must not contain a NUL byte")
	}
	name, _, _ := strings.Cut(value, "=")
	switch name {
	case "--host", "--port", "--block-private-networks", "--log-format", "--log-filter", "--log-filter-scopes", "--timeout":
		return fmt.Errorf("argument must not set %s", name)
	}
	return nil
}

// validateLimits checks settings whose zero and negative values have special meanings.
func validateLimits(opts *options) error {
	if opts.maxConcurrency < 1 {
		return errors.New("config: -max-concurrency must be at least 1")
	}
	if opts.maxQueuedRequests < 0 {
		return errors.New("config: -max-queued-requests must not be negative")
	}
	if opts.queueTimeout < 0 {
		return errors.New("config: -queue-timeout must not be negative")
	}
	if opts.maxOperationLifetime < 0 {
		return errors.New("config: -max-operation-lifetime must not be negative; zero means no maximum")
	}
	if opts.maxScriptBytes < 1 {
		return errors.New("config: -max-script-bytes must be at least 1")
	}
	if opts.maxStdoutBytes < 0 {
		return errors.New("config: -max-stdout-bytes must not be negative")
	}
	if opts.maxStderrBytes < 0 {
		return errors.New("config: -max-stderr-bytes must not be negative")
	}
	if opts.shutdownDelay < 0 {
		return errors.New("config: -shutdown-delay must not be negative")
	}
	if opts.shutdownGrace < 0 {
		return errors.New("config: -shutdown-grace must not be negative")
	}
	return nil
}
