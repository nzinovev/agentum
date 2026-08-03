//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// Process-group control for POSIX hosts. The Windows implementation lives in
// process_windows.go; the adapter itself never references syscall directly, so
// `go build`/`go vet` succeed on every GOOS the toolchain supports.

// setProcessGroup puts the child in its own process group. opencode spawns
// helpers (subagents, formatters, LSP servers); without a group of its own,
// cancelling the run would leave those helpers holding the worktree.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalProcessGroup signals the child's whole process group: SIGTERM for a
// graceful stop, SIGKILL when force is set. Negating the pid addresses the
// group, which is why setProcessGroup ran at Start time. A process that has
// already been reaped yields ESRCH, which is the no-op we want.
func signalProcessGroup(cmd *exec.Cmd, force bool) {
	if cmd.Process == nil {
		return
	}
	signal := syscall.SIGTERM
	if force {
		signal = syscall.SIGKILL
	}
	_ = syscall.Kill(-cmd.Process.Pid, signal)
}
