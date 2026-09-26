package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// CDPHost is the loopback address for the Lightpanda CDP server. Lightpanda does not
// authenticate CDP connections, so the server must listen only on loopback.
const CDPHost = "127.0.0.1"

// blockedCIDRs lists loopback CIDRs blocked by default for managed Lightpanda children
// running `serve` or `run`. These blocks keep client-controlled content from reaching
// host-local services.
const blockedCIDRs = "127.0.0.0/8,::1/128"

// Launcher prepares managed Lightpanda commands with reserved arguments, a separate
// browser working directory for each child, and a restricted environment. Separate
// directories prevent children from sharing files. The environment excludes parent
// credentials and configuration.
type Launcher struct {
	binaryPath string
	sharedArgs []string
}

// CDPServerOptions configures a `lightpanda serve` command.
type CDPServerOptions struct {
	// MaxConnections sets the limit for backend connections and connections waiting to
	// be accepted. NewCDPServerCmd uses this value for both limits.
	MaxConnections int
}

// NewLauncher creates a Launcher for the Lightpanda executable at binaryPath.
// binaryPath must be absolute.
//
// launchArgs provides additional arguments for each child command. NewCDPServerCmd
// reserves --host, --port, --cdp-max-connections, and --cdp-max-pending-connections so
// the CDP endpoint and connection limits stay under lpgw control. Launcher also sets
// --log-format logfmt and --block-private-networks, and rejects log filters and the
// deprecated --timeout so lpgw controls logging, network isolation, and timeouts.
//
// Launcher blocks 127.0.0.0/8 and ::1/128 by default. launchArgs can narrow these
// blocks with --block-cidrs followed by an argument that starts with "-". Lightpanda checks
// exemptions before blocks.
func NewLauncher(binaryPath string, launchArgs []string) (*Launcher, error) {
	if binaryPath == "" {
		return nil, errors.New("browser: binary must not be empty")
	}
	if !filepath.IsAbs(binaryPath) {
		return nil, fmt.Errorf("browser: binary path %q must be absolute", binaryPath)
	}
	if err := validateLaunchArgs(launchArgs); err != nil {
		return nil, err
	}
	return &Launcher{binaryPath: binaryPath, sharedArgs: buildSharedArgs(launchArgs)}, nil
}

// NewCDPServerCmd prepares a `lightpanda serve` command that listens on loopback. The OS
// chooses the port.
//
// On success, the returned Cmd owns the browser working directory. Call Discard if the
// command is not started or if Start fails.
//
// extraEnv adds environment variables to the child process. It cannot override variables
// managed by this package.
func (l *Launcher) NewCDPServerCmd(ctx context.Context, opts CDPServerOptions, extraEnv map[string]string, stdout, stderr io.Writer) (*Cmd, error) {
	if opts.MaxConnections < 1 || opts.MaxConnections > 65535 {
		return nil, errors.New("browser: MaxConnections must be between 1 and 65535")
	}
	if err := validateExtraEnv(extraEnv); err != nil {
		return nil, err
	}
	dir, err := createProcessDir()
	if err != nil {
		return nil, err
	}
	maxConnections := strconv.Itoa(opts.MaxConnections)
	args := append([]string{
		"serve",
		"--host", CDPHost,
		"--port", "0",
		"--cdp-max-connections", maxConnections,
		"--cdp-max-pending-connections", maxConnections,
	}, l.sharedArgs...)
	return l.newCmd(ctx, dir, args, extraEnv, stdout, stderr), nil
}

// NewScriptCmd writes the script source to script.js in a browser working directory and
// prepares a `lightpanda run` command with the file's absolute path.
//
// On success, the returned Cmd owns the browser working directory. Call Discard if the
// command is not started or if Start fails.
//
// extraEnv adds environment variables to the child process. It cannot override variables
// managed by this package.
func (l *Launcher) NewScriptCmd(ctx context.Context, script string, extraEnv map[string]string, stdout, stderr io.Writer) (*Cmd, error) {
	if err := validateExtraEnv(extraEnv); err != nil {
		return nil, err
	}
	dir, err := createProcessDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "script.js")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		err = fmt.Errorf("write script: %w", err)
		if rmErr := removeProcessDir(dir); rmErr != nil {
			err = errors.Join(err, fmt.Errorf("remove process directory after script write failure: %w", rmErr))
		}
		return nil, err
	}
	args := append([]string{"run", path}, l.sharedArgs...)
	return l.newCmd(ctx, dir, args, extraEnv, stdout, stderr), nil
}

// browserCmdWaitDelay limits how long os/exec waits for a child and its I/O pipes after
// cancellation. After a normal exit, it limits the wait for I/O pipes.
const browserCmdWaitDelay = 20 * time.Second

func (l *Launcher) newCmd(ctx context.Context, dir string, args []string, extraEnv map[string]string, stdout, stderr io.Writer) *Cmd {
	cmd := exec.CommandContext(ctx, l.binaryPath, args...) // #nosec G204 -- operator-supplied startup configuration; absolute path and no shell
	cmd.Dir = dir
	cmd.Env = browserEnv(dir, extraEnv)
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// When ctx is canceled, Cancel asks the child to stop. It sends SIGTERM on platforms
	// that support it. WaitDelay force-kills the child if it does not exit before the delay.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = browserCmdWaitDelay
	return &Cmd{cmd: cmd, processDir: dir}
}

func validateLaunchArgs(launchArgs []string) error {
	for _, arg := range launchArgs {
		if arg == "" {
			return errors.New("browser: launch args may not contain an empty argument")
		}
		if strings.ContainsRune(arg, 0) {
			return errors.New("browser: launch args may not contain a NUL byte")
		}
		name, _, _ := strings.Cut(arg, "=")
		switch name {
		case "--host", "--port", "--cdp-max-connections", "--cdp-max-pending-connections", "--block-private-networks", "--log-format", "--log-filter", "--log-filter-scopes", "--timeout":
			return fmt.Errorf("browser: launch args may not set %s", name)
		}
	}
	return nil
}

func buildSharedArgs(launchArgs []string) []string {
	managedArgs := make([]string, 0, 5+len(launchArgs))
	managedArgs = append(managedArgs,
		"--log-format", "logfmt",
		"--block-private-networks", "--block-cidrs", blockedCIDRs,
	)
	return append(managedArgs, launchArgs...)
}

func validateExtraEnv(extraEnv map[string]string) error {
	var errs []error
	for key, value := range extraEnv {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			errs = append(errs, fmt.Errorf("browser: invalid environment variable %q", key))
		}
		if strings.ContainsRune(value, 0) {
			errs = append(errs, fmt.Errorf("browser: environment variable %s contains a NUL byte", key))
		}
		switch key {
		case "PATH", "HOME", "TMPDIR", "PWD", "LANG":
			errs = append(errs, fmt.Errorf("browser: environment variable %s is managed by browser", key))
		}
	}
	return errors.Join(errs...)
}

// browserEnv builds a restricted environment for a managed child. It excludes ambient
// parent credentials and application configuration but keeps PATH so the child can find programs.
func browserEnv(dir string, extraEnv map[string]string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	env := make([]string, 0, 5+len(extraEnv))
	for key, value := range extraEnv {
		env = append(env, key+"="+value)
	}
	env = append(env,
		"PATH="+path,
		"HOME="+dir,
		"TMPDIR="+dir,
		"PWD="+dir,
		"LANG=C.UTF-8",
	)
	return env
}
