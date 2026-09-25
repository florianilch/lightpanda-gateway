package browser

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Cmd is a prepared command for starting a Lightpanda child process. It owns the
// browser working directory until Discard removes it or Start transfers it to Process.
type Cmd struct {
	cmd        *exec.Cmd
	processDir string
}

// Discard removes Cmd's browser working directory. If removal fails, Cmd keeps
// ownership so the caller can try again. After Start succeeds, Process owns the
// directory and Discard has no effect.
func (c *Cmd) Discard() error {
	if c.processDir == "" {
		return nil
	}
	if err := removeProcessDir(c.processDir); err != nil {
		return err
	}
	c.processDir = ""
	return nil
}

// Start starts the prepared Lightpanda child process.
// On success, the returned Process owns the browser working directory.
// On failure, Cmd keeps the directory; the caller must call Discard.
func (c *Cmd) Start() (*Process, error) {
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	processDir := c.processDir
	c.processDir = ""
	return &Process{
		cmd:        c.cmd,
		processDir: processDir,
	}, nil
}

// Process represents a started Lightpanda child process. It owns the browser working
// directory until Cleanup removes it.
type Process struct {
	cmd        *exec.Cmd
	processDir string
}

// Wait waits for the Lightpanda child process to exit and for its I/O copying to finish.
// It returns any error from waiting or I/O copying. Call it exactly once after Start succeeds.
func (p *Process) Wait() error {
	return p.cmd.Wait()
}

// Cleanup removes Process's browser working directory. Call it after Wait returns.
// If removal fails, Process retains ownership and Cleanup returns the error; the caller can retry.
// Cleanup does not stop or wait for the child process.
func (p *Process) Cleanup() error {
	if err := removeProcessDir(p.processDir); err != nil {
		return err
	}
	p.processDir = ""
	return nil
}

// PID returns the child process ID after Start succeeds.
func (p *Process) PID() int {
	osProcess := p.cmd.Process
	if osProcess != nil {
		return osProcess.Pid
	}
	return 0
}

// ExitCode returns the child process exit code after Wait returns, or -1 when unavailable.
func (p *Process) ExitCode() int {
	if p.cmd.ProcessState == nil {
		return -1
	}
	return p.cmd.ProcessState.ExitCode()
}

// createProcessDir creates a temporary browser working directory under [os.TempDir].
// It returns an absolute path.
func createProcessDir() (string, error) {
	tempDir, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	return os.MkdirTemp(tempDir, "lpgw-process-")
}

// removeProcessDir removes the browser working directory at dir and its contents.
// It does nothing when dir is empty.
func removeProcessDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}
