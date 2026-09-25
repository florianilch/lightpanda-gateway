// Package browser prepares and runs managed Lightpanda child processes.
//
// Managed `serve` and `run` children use a temporary browser working directory and a
// restricted environment. This isolates each child's files and keeps ambient parent
// credentials and configuration out of client-controlled code.
//
// Cmd owns the browser working directory until Start succeeds. If Start is not called
// or fails, call Discard to try to remove it.
//
// After Start succeeds, Process owns the directory. Call Cleanup after Wait returns to
// try to remove it. Waiting ensures the child no longer uses the directory.
//
// When the command context is canceled, the child receives SIGTERM first on platforms
// that support it. If it is still running after the wait delay, os/exec kills it.
package browser
