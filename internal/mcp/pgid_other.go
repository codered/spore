//go:build !unix

package mcp

import "os/exec"

// setPgid is a no-op off unix: process groups are not portable, and the SDK's
// own termination sequence is what stops the child there.
func setPgid(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
