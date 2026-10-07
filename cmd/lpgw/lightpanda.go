package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
)

// supportedBrowserVersionRange limits the tested Lightpanda versions.
const supportedBrowserVersionRange = ">= 0.4.0, <= 1.0.0"

// validateBrowserBinary resolves binary to an absolute executable path. It runs
// `<path> version` and accepts only the stable Lightpanda versions supported by
// this build. On success it returns the resolved path and reported version.
func validateBrowserBinary(binary string) (resolvedPath, version string, err error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", "", fmt.Errorf("config: find lightpanda binary %q: %w", binary, err)
	}
	resolvedPath, err = filepath.Abs(path)
	if err != nil {
		return "", "", fmt.Errorf("config: resolve lightpanda binary %q: %w", path, err)
	}

	detectedBrowserVersion, err := probeBrowserVersion(resolvedPath)
	if err != nil {
		return "", "", fmt.Errorf("config: validate lightpanda binary %q: %w", resolvedPath, err)
	}
	if detectedBrowserVersion.Prerelease() != "" {
		return "", "", fmt.Errorf(
			"config: unsupported lightpanda prerelease version %s; supported stable releases %s",
			detectedBrowserVersion,
			supportedBrowserVersionRange,
		)
	}
	supportedBrowserVersions, err := semver.NewConstraint(supportedBrowserVersionRange)
	if err != nil {
		panic("cannot parse supportedBrowserVersionRange")
	}

	if !supportedBrowserVersions.Check(detectedBrowserVersion) {
		return "", "", fmt.Errorf(
			"config: unsupported lightpanda version %s; supported %s",
			detectedBrowserVersion,
			supportedBrowserVersionRange,
		)
	}
	return resolvedPath, detectedBrowserVersion.String(), nil
}

const (
	// `lightpanda version` does not start a browser and should return quickly. This
	// timeout bounds the command after Start succeeds. Go cannot interrupt Start itself.
	browserVersionCommandTimeout = 5 * time.Second
	// WaitDelay bounds how long Wait may remain blocked after cancellation or process
	// exit, including when a descendant still has stdout or stderr open.
	browserVersionWaitDelay = 1 * time.Second
	// A valid version is usually one SemVer line.
	browserVersionOutputLimit = 4 << 10
)

// versionOutput keeps at most browserVersionOutputLimit bytes. Write still accepts
// the whole stream so os/exec can continue draining the pipe.
type versionOutput struct {
	data      []byte
	truncated bool
}

func (out *versionOutput) Write(p []byte) (int, error) {
	if room := browserVersionOutputLimit - len(out.data); room > 0 {
		out.data = append(out.data, p[:min(len(p), room)]...)
		out.truncated = out.truncated || len(p) > room
	} else if len(p) > 0 {
		out.truncated = true
	}
	return len(p), nil
}

// browserVersionEnv builds the environment for `lightpanda version`.
// It uses LANG=C.UTF-8 and the parent's PATH or a fallback when PATH is unset.
// It also preserves LIGHTPANDA_DISABLE_TELEMETRY when set.
func browserVersionEnv() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	env := []string{
		"PATH=" + path,
		"LANG=C.UTF-8",
	}
	if value, ok := os.LookupEnv("LIGHTPANDA_DISABLE_TELEMETRY"); ok {
		env = append(env, "LIGHTPANDA_DISABLE_TELEMETRY="+value)
	}
	return env
}

// probeBrowserVersion runs `<binary> version` and parses stdout as a semantic version.
// stderr is kept only to explain failures.
func probeBrowserVersion(binary string) (*semver.Version, error) {
	ctx, cancel := context.WithTimeout(context.Background(), browserVersionCommandTimeout)
	defer cancel()

	var stdout, stderr versionOutput
	cmd := exec.CommandContext(ctx, binary, "version")
	cmd.Env = browserVersionEnv()
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = browserVersionWaitDelay

	if err := cmd.Run(); err != nil {
		details := fmt.Sprintf(" stdout=%q stderr=%q", stdout.data, stderr.data)
		if stdout.truncated {
			details += " stdout_truncated=true"
		}
		if stderr.truncated {
			details += " stderr_truncated=true"
		}

		switch {
		case ctx.Err() != nil:
			return nil, fmt.Errorf("version command timed out after %s:%s %w", browserVersionCommandTimeout, details, ctx.Err())
		case errors.Is(err, exec.ErrWaitDelay):
			return nil, fmt.Errorf("version command output did not close within %s:%s %w", browserVersionWaitDelay, details, err)
		default:
			return nil, fmt.Errorf("version command failed:%s %w", details, err)
		}
	}
	if stdout.truncated {
		return nil, fmt.Errorf("version stdout exceeded %d bytes: stdout=%q stderr=%q", browserVersionOutputLimit, stdout.data, stderr.data)
	}

	versionText := strings.TrimSpace(string(stdout.data))
	parsed, err := semver.StrictNewVersion(versionText)
	if err != nil {
		return nil, fmt.Errorf("cannot parse version stdout %q as SemVer: stderr=%q: %w", versionText, stderr.data, err)
	}
	return parsed, nil
}
