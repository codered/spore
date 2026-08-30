//go:build windows

package shell

import (
	"os"
	"os/exec"
	"strconv"
)

// setProcessGroup is a no-op on Windows. There is no process group to join:
// killProcessTree walks the parent/child tree that Windows already tracks.
func setProcessGroup(*exec.Cmd) {}

// killProcessTree terminates the process and every descendant. Windows has no
// group signal, so this shells out to taskkill, whose /T flag is the platform's
// own answer to the same problem.
func killProcessTree(pid int) {
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// wasKilled reports whether the process was terminated rather than reaching its
// own exit. Windows carries no signal in the exit status — a terminated process
// simply reports a non-zero code — so this cannot be exact the way the Unix
// implementation is. A command that exits non-zero at the very instant the
// deadline passes is reported as a timeout on Windows; it keeps its exit status
// on Unix.
func wasKilled(ps *os.ProcessState) bool {
	return ps == nil || ps.ExitCode() != 0
}
