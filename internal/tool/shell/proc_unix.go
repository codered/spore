//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a new process group of its own, so its
// descendants can be signalled as a unit.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the whole group. The group id equals the shell's pid
// because it was made the group leader.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// wasKilled reports whether the process died from a signal rather than
// reaching its own exit. This is exact on Unix, which is what lets a command
// that exited on its own at the instant of the deadline keep its exit status.
func wasKilled(ps *os.ProcessState) bool {
	if ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}
