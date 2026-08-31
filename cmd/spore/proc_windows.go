//go:build windows

package main

import "os/exec"

// detach is a no-op on Windows; the daemon is not a supported deployment
// target there (spec section 9), and the CLI still works against one started
// by hand.
func detach(cmd *exec.Cmd) {}
