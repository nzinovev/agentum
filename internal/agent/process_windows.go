//go:build windows

package agent

import "os/exec"

// setProcessGroup is a no-op on Windows: there is no Setpgid equivalent, and
// exec.CommandContext's process-tree kill on cancellation is the supported
// path. Kept as a named function so the call site in opencode.go stays
// platform-agnostic.
func setProcessGroup(cmd *exec.Cmd) {
	// Intentionally empty: Windows has no process-group concept equivalent to
	// Unix Setpgid; cancellation relies on exec.CommandContext killing the
	// child. The real implementation lives in process_unix.go.
}

// terminateProcessGroup is a no-op on Windows. Cancellation relies on
// exec.CommandContext killing the child when its context is cancelled; the
// Unix-specific negative-pgid signal has no Windows analogue.
func terminateProcessGroup(cmd *exec.Cmd) {
	// Intentionally empty: the Unix negative-pgid SIGTERM has no Windows
	// analogue; exec.CommandContext's own reaping handles cancellation. The
	// real implementation lives in process_unix.go.
}
