//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own process group so a ctrl-c in the
// terminal that started it does not take it down with the CLI.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
