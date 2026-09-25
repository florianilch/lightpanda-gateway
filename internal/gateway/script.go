package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/florianilch/lightpanda-gateway/internal/browser"
)

var (
	// ErrScriptCallClosed means Close canceled the ScriptCall before Run started or
	// while Run was active. Run returns no ScriptResult with this cause.
	ErrScriptCallClosed = errors.New("script call closed")
	// ErrScriptFailed means the browser process running the script exited with a
	// non-zero status.
	// Run returns a ScriptResult and an error matching this cause.
	ErrScriptFailed = errors.New("script failed")
)

var secretKeyPattern = regexp.MustCompile(`^LP_[A-Z0-9_]{1,64}$`)

// ScriptRequest specifies one script execution.
type ScriptRequest struct {
	Script  string
	Secrets map[string]string
	// Timeout requests a shorter operation lifetime for this ScriptCall. Zero leaves
	// the limit to Gateway. If both limits are set, Gateway uses the shorter one.
	Timeout time.Duration
}

// Validate rejects blank scripts, negative timeouts, invalid secret names, and NUL bytes
// in secret values.
func (req ScriptRequest) Validate() error {
	if strings.TrimSpace(req.Script) == "" {
		return errors.New("empty script")
	}
	if req.Timeout < 0 {
		return errors.New("script timeout must not be negative")
	}
	// Sort keys so the same invalid ScriptRequest returns the same error.
	for _, key := range slices.Sorted(maps.Keys(req.Secrets)) {
		value := req.Secrets[key]
		if !secretKeyPattern.MatchString(key) {
			return fmt.Errorf("secret keys must match ^LP_[A-Z0-9_]{1,64}$, got %q", key)
		}
		if strings.ContainsRune(value, 0) {
			return fmt.Errorf("secret %s contains a NUL byte", key)
		}
	}
	return nil
}

// ScriptResult contains captured output, exit code, and truncation flags.
type ScriptResult struct {
	Stdout          string
	Stderr          string
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
}

// ScriptCall is a Gateway resource for one script execution.
type ScriptCall struct {
	launcher       *browser.Launcher
	logger         *slog.Logger
	req            ScriptRequest
	maxStdoutBytes int
	maxStderrBytes int

	// mu protects runCalled, cancelCause, and cancelRun.
	mu          sync.Mutex
	runCalled   bool
	cancelCause error
	cancelRun   context.CancelCauseFunc

	// done closes after command discard or process cleanup and final logging. If cancel
	// records a cause before Run sets runCalled, cancel closes done instead.
	done chan struct{}
}

// BeginScriptCall validates req and tries to get one Gateway slot. logger must not
// be nil. ctx controls only getting the slot. Canceling ctx after BeginScriptCall
// succeeds does not stop the returned ScriptCall.
//
// After success, the caller must call Close and may call Run once. Close waits
// for an active Run, including its final logging, to finish. Gateway may cancel
// the ScriptCall first.
//
// BeginScriptCall may return ErrQueueFull, ErrQueueTimeout, or ErrDraining while
// getting a Gateway slot. If ctx is canceled first, its cause is returned.
// Invalid req is rejected before a slot is requested.
func (g *Gateway) BeginScriptCall(ctx context.Context, logger *slog.Logger, req ScriptRequest) (*ScriptCall, error) {
	if logger == nil {
		return nil, errors.New("begin script call: logger must not be nil")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	// ScriptCall keeps req after this method returns. Clone Secrets so later changes
	// by the caller do not affect Run.
	req.Secrets = maps.Clone(req.Secrets)

	lifetime := effectiveResourceLifetime(g.maxResourceLifetime, req.Timeout)
	call := &ScriptCall{
		launcher:       g.launcher,
		logger:         logger,
		req:            req,
		maxStdoutBytes: g.maxStdoutBytes,
		maxStderrBytes: g.maxStderrBytes,
		done:           make(chan struct{}),
	}
	if err := g.admit(ctx, call, lifetime); err != nil {
		return nil, err
	}
	return call, nil
}

// Run executes the script. A caller may call Run at most once. Before returning, Run
// waits for a browser process it started and for output copying to finish, then
// attempts command discard or process cleanup.
//
// A non-zero exit status returns a ScriptResult and an error matching
// ErrScriptFailed. Command discard and process cleanup errors are included in the
// returned error.
//
// If ctx or the ScriptCall is canceled before Run decides its result, Run returns no
// ScriptResult and an error matching the cancellation cause.
func (c *ScriptCall) Run(ctx context.Context) (result *ScriptResult, err error) {
	runCtx, cancelRun := context.WithCancelCause(ctx)

	c.mu.Lock()
	if c.runCalled {
		c.mu.Unlock()
		cancelRun(context.Canceled)
		return nil, errors.New("ScriptCall.Run called more than once")
	}
	c.runCalled = true
	if cause := c.cancelCause; cause != nil {
		c.mu.Unlock()
		cancelRun(context.Canceled)
		return nil, cause
	}
	// Store cancelRun under mu so cancel cannot observe runCalled without a way to
	// cancel the active Run.
	c.cancelRun = cancelRun
	c.mu.Unlock()
	// Register this defer first so done closes after the other defers finish.
	defer close(c.done)

	// Keep the caller's context values for final logs without its cancellation.
	logCtx := context.WithoutCancel(ctx)

	stdoutHead := &leadingCapture{limit: c.maxStdoutBytes}
	stderrHead := &leadingCapture{limit: c.maxStderrBytes}
	stdoutTail := &trailingCapture{limit: maxProcessTailBytes}
	stderrTail := &trailingCapture{limit: maxProcessTailBytes}

	var (
		runErr     error
		cleanupErr error
		pid        int
		exitCode   = -1
	)

	// Final logging runs after command discard or process cleanup so it can report
	// those errors.
	defer func() {
		if runErr != nil {
			attrs := []slog.Attr{slog.Any("err", runErr)}
			if pid > 0 {
				attrs = append(attrs,
					slog.Int("pid", pid),
					slog.Int("exit_code", exitCode),
				)
			}
			if c.logger.Enabled(logCtx, slog.LevelDebug) {
				attrs = append(attrs,
					slog.Group("stdout",
						slog.String("tail", stdoutTail.String()),
						slog.Bool("truncated", stdoutTail.truncated),
					),
					slog.Group("stderr",
						slog.String("tail", stderrTail.String()),
						slog.Bool("truncated", stderrTail.truncated),
					),
				)
			}
			c.logger.LogAttrs(logCtx, slog.LevelError, "script run failed", attrs...)
		}

		if cleanupErr != nil {
			attrs := []slog.Attr{slog.Any("err", cleanupErr)}
			if pid > 0 {
				attrs = append(attrs,
					slog.Int("pid", pid),
					slog.Int("exit_code", exitCode),
				)
			}
			c.logger.LogAttrs(logCtx, slog.LevelError, "script cleanup failed", attrs...)
		}
	}()

	// Cancel runCtx before final logging and before done closes. cancel may already
	// have called cancelRun; calling it again is safe.
	defer cancelRun(context.Canceled)

	// A failure before Start succeeds cancels the ScriptCall. cancel preserves the
	// first cause, so return the cause stored on the ScriptCall.
	failBeforeProcessStart := func(cause error) error {
		c.cancel(cause)
		c.mu.Lock()
		cause = c.cancelCause
		c.mu.Unlock()
		return cause
	}

	if cause := context.Cause(runCtx); cause != nil {
		runErr = failBeforeProcessStart(cause)
		return nil, runErr
	}

	// Use runCtx so canceling Run's context or the ScriptCall stops the browser process.
	cmd, prepareErr := c.launcher.NewScriptCmd(
		runCtx,
		c.req.Script,
		lightpandaExtraEnv(c.req.Secrets),
		io.MultiWriter(stdoutHead, stdoutTail),
		io.MultiWriter(stderrHead, stderrTail),
	)
	if prepareErr != nil {
		failure := fmt.Errorf("prepare script process: %w", prepareErr)
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(failure, cause) {
			failure = errors.Join(cause, failure)
		}
		runErr = failBeforeProcessStart(failure)
		return nil, runErr
	}
	// Defer Discard after preparation succeeds. If Start succeeds, Process owns the
	// working directory, so Discard does nothing.
	defer func() {
		if discardErr := cmd.Discard(); discardErr != nil {
			cleanupErr = fmt.Errorf("discard script command: %w", discardErr)
			// Update err because Discard runs after return values are set.
			err = errors.Join(err, cleanupErr)
		}
	}()

	if cause := context.Cause(runCtx); cause != nil {
		runErr = failBeforeProcessStart(cause)
		return nil, runErr
	}

	process, startErr := cmd.Start()
	if startErr != nil {
		failure := fmt.Errorf("start script process: %w", startErr)
		if cause := context.Cause(runCtx); cause != nil && !errors.Is(failure, cause) {
			failure = errors.Join(cause, failure)
		}
		runErr = failBeforeProcessStart(failure)
		return nil, runErr
	}

	processErr := process.Wait()

	pid = process.PID()
	exitCode = process.ExitCode()
	_, scriptFailed := errors.AsType[*exec.ExitError](processErr)

	// The process has exited. Read both cancellation causes before deciding what Run
	// returns. cancel may record cancelCause before it cancels runCtx. Clear cancelRun
	// because no process remains to stop.
	runCause := context.Cause(runCtx)
	c.mu.Lock()
	cancelCause := c.cancelCause
	c.cancelRun = nil
	c.mu.Unlock()

	returnResult := runCause == nil &&
		cancelCause == nil &&
		(processErr == nil || scriptFailed)
	if returnResult {
		result = &ScriptResult{
			Stdout:          stdoutHead.String(),
			Stderr:          stderrHead.String(),
			ExitCode:        exitCode,
			StdoutTruncated: stdoutHead.truncated,
			StderrTruncated: stderrHead.truncated,
		}
	}
	switch {
	case runCause != nil:
		runErr = runCause
	case cancelCause != nil:
		runErr = cancelCause
	case scriptFailed:
		//nolint:errorlint // Use %v because wrapping would expose *exec.ExitError and conflate an expected script failure with a process failure.
		runErr = fmt.Errorf("%w: %v", ErrScriptFailed, processErr)
	case processErr != nil:
		runErr = fmt.Errorf("wait for script process: %w", processErr)
	}

	if processCleanupErr := process.Cleanup(); processCleanupErr != nil {
		cleanupErr = fmt.Errorf("cleanup script process: %w", processCleanupErr)
		return result, errors.Join(runErr, cleanupErr)
	}
	return result, runErr
}

// Done returns a channel that closes when the ScriptCall finishes. If Run sets
// runCalled first, Run closes the channel after command discard or process cleanup
// and final logging. If cancellation is recorded first, cancel closes the channel.
func (c *ScriptCall) Done() <-chan struct{} { return c.done }

// Close cancels the ScriptCall and waits for Done to close. It always returns nil.
// Run reports script execution errors and errors from command discard or process
// cleanup.
func (c *ScriptCall) Close() error {
	c.cancel(ErrScriptCallClosed)
	<-c.Done()
	return nil
}

// cancel records the first cause and does not wait. cause must not be nil.
func (c *ScriptCall) cancel(cause error) {
	c.mu.Lock()
	if c.cancelCause != nil {
		c.mu.Unlock()
		return
	}
	c.cancelCause = cause
	runCalled := c.runCalled
	cancelRun := c.cancelRun
	c.cancelRun = nil
	if !runCalled {
		// No Run has set runCalled. Run returns cancelCause without closing done if it
		// waits for mu or starts later, so close done here.
		close(c.done)
	}
	c.mu.Unlock()

	// Run may read runCtx after mu is unlocked and before cancelRun is called. Record
	// cancelCause first so Run can still report it.
	if cancelRun != nil {
		cancelRun(cause)
	}
}
func (c *ScriptCall) requestStop(cause error) { c.cancel(cause) }
