//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so cancellation can
// kill the whole tree (opencode may spawn subagents, formatters, LSP servers).
// Unix uses Setpgid; the process-group kill in terminateProcessGroup targets
// -pgid to reach descendants the parent spawned.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup sends SIGTERM to the negative process-group id, hitting
// every process in the child's group. SIGKILL after a grace period is left to
// exec.CommandContext's own reaping.
func terminateProcessGroup(cmd *exec.Cmd) {
	pgid := cmd.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
}
