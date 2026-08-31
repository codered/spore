package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ErrAlreadyRunning is returned by AcquirePidFile when a daemon is already
// running. Callers can use errors.As to extract the pid.
var ErrAlreadyRunning = errors.New("daemon is already running")

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
//
// Note: the kernel can reuse pids after a process exits. Between a daemon
// crash and the next --status or --stop, the pid can be reassigned to an
// unrelated process. This is inherent to pidfiles without start-time
// verification. We accept this for a single-user local tool.
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

// AcquirePidFile claims the pidfile for this process, atomically. O_EXCL means
// the create either wins outright or fails — there is no window between
// checking and writing for a second daemon to slip into. A pidfile left by a
// crashed daemon is detected here and replaced; one naming a live process is
// reported as ErrAlreadyRunning.
func AcquirePidFile(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create pidfile dir: %w", err)
		}
	}

	pidStr := strconv.Itoa(os.Getpid()) + "\n"
	pidBytes := []byte(pidStr)

	// Try to create the file exclusively. If it exists, we need to check if
	// the daemon it names is still running.
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// Successfully created the file. Write our PID and we're done.
			_, writeErr := f.Write(pidBytes)
			closeErr := f.Close()
			if writeErr != nil {
				return writeErr
			}
			if closeErr != nil {
				return closeErr
			}
			return nil
		}

		// File already exists. Check if the daemon it names is still running.
		if !os.IsExist(err) {
			// Some other error (permission denied, etc.)
			return fmt.Errorf("open pidfile: %w", err)
		}

		// File exists. Read the pid.
		pid, readErr := ReadPidFile(path)
		if readErr != nil {
			// File is garbage or unreadable. On first attempt, try to remove
			// and retry. On second attempt, give up and report already-running.
			if attempt == 0 {
				_ = os.Remove(path)
				continue
			}
			// Still can't read it on second attempt. Report it as already running
			// to be safe (the stale file may be protecting a crashed daemon).
			return fmt.Errorf("%w (stale pidfile unreadable)", ErrAlreadyRunning)
		}

		// We have a pid. Is it alive?
		if PidAlive(pid) {
			// The daemon is running. This is a hard error.
			return fmt.Errorf("%w: pid %d", ErrAlreadyRunning, pid)
		}

		// The daemon is dead. Try to remove the stale file and retry once.
		if attempt == 0 {
			_ = os.Remove(path)
			continue
		}

		// Second attempt and the file still exists. Give up and report
		// already-running to avoid looping forever in a race condition.
		return fmt.Errorf("%w: pid %d (stale pidfile could not be removed)", ErrAlreadyRunning, pid)
	}

	// Should not reach here, but if we do, something went very wrong.
	return fmt.Errorf("internal error: AcquirePidFile retry loop exhausted")
}

// ReleasePidFile removes the pidfile only if it still names this process, so a
// daemon that lost a startup race cannot delete the winner's pidfile.
func ReleasePidFile(path string) error {
	pid, err := ReadPidFile(path)
	if err != nil {
		// File doesn't exist or is unreadable. Nothing to do.
		return nil
	}

	// Only remove if it names this process.
	if pid != os.Getpid() {
		return nil
	}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
