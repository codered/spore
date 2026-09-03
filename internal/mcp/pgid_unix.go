//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setPgid puts the child in its own process group so killGroup can take down
// the whole tree. An MCP server launched through a wrapper — npx, uv, a shell
// script — leaves grandchildren that outlive the process the SDK signals.
func setPgid(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup SIGKILLs the child's whole process group. It is the last step of
// shutdown, after the SDK has closed stdin and sent SIGTERM, so a wedged
// server cannot outlive the daemon.
func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative pid means "the group". Errors are ignored: the usual one is
	// ESRCH, which means the thing we wanted gone is already gone.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
