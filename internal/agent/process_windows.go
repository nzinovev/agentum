//go:build windows

package agent

import (
	"os/exec"
	"strconv"
	"syscall"
)

// Process-group control for Windows hosts. Mirrors process_unix.go so the
// adapter compiles and runs on a developer's Windows machine; the enforcement
// guarantees themselves are unchanged, only the termination primitive differs.

// setProcessGroup puts the child in its own process group so terminating the
// run does not walk up into the Agentum process itself.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// signalProcessGroup terminates the child's process tree. Windows has no
// SIGTERM equivalent an arbitrary console process is guaranteed to honor, so
// both the graceful and the forced path end at the same place: `taskkill /T`
// walks the tree, and `/F` forces it. The graceful call is left without /F so
// a well-behaved child still gets the chance to exit on its own; the caller
// escalates with force after the grace period.
//
// This is the documented Windows limitation: a child that ignores the console
// close is only stopped by the forced escalation.
func signalProcessGroup(cmd *exec.Cmd, force bool) {
	if cmd.Process == nil {
		return
	}
	args := []string{"/T", "/PID", strconv.Itoa(cmd.Process.Pid)}
	if force {
		args = append([]string{"/F"}, args...)
	}
	kill := exec.Command("taskkill", args...)
	if err := kill.Run(); err != nil && force {
		// taskkill unavailable or the pid is already gone: fall back to killing
		// the leader directly. Best-effort by design — the caller has already
		// decided the run must end.
		_ = cmd.Process.Kill()
	}
}
