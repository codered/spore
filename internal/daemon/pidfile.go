package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WritePidFile records this process's id so a later CLI invocation can find
// the daemon it started.
func WritePidFile(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create pidfile dir: %w", err)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func ReadPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("pidfile %s does not contain a pid: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pidfile %s contains an impossible pid %d", path, pid)
	}
	return pid, nil
}

// PidAlive reports whether a process with that id exists. Signal 0 performs
// the permission and existence checks without delivering anything, which is
// the portable way to ask. A daemon that crashed leaves its pidfile behind,
// and treating that as "already running" would wedge the CLI permanently.
func PidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists and belongs to someone else.
	return os.IsPermission(err)
}

// RemovePidFile deletes the file, tolerating its absence: shutdown paths run
// more than once.
func RemovePidFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
