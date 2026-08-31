package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

// startupTimeout bounds how long a CLI waits for a daemon it just spawned.
const startupTimeout = 15 * time.Second

// daemonPid reports the pid of a live daemon, or an error when none is
// running. A stale pidfile from a crashed daemon is an error, not a pid.
func daemonPid(cfg *config.Config) (int, error) {
	pid, err := daemon.ReadPidFile(cfg.PidPath())
	if err != nil {
		return 0, err
	}
	if !daemon.PidAlive(pid) {
		return 0, fmt.Errorf("pidfile names pid %d, which is not running", pid)
	}
	return pid, nil
}

// ensureDaemon returns a client for a running daemon, starting one if
// nothing answers. The daemon it starts is DETACHED and outlives this
// process: scheduled jobs must keep firing and an approval suspended
// mid-turn must still be answerable after the terminal closes.
func ensureDaemon(ctx context.Context, cfg *config.Config) (*client, error) {
	c := newClient(cfg.Daemon.Addr)

	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.health(probe)
	cancel()
	if err == nil {
		return c, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the spore binary to start a daemon: %w", err)
	}
	logPath := filepath.Join(cfg.DataDir, "daemon.log")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "-config", cfg.Path, "serve", "--detach")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	detach(cmd) // put it in its own process group; see proc_*.go
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the daemon: %w", err)
	}
	// Release rather than Wait: this process must be able to exit while the
	// daemon keeps running.
	if err := cmd.Process.Release(); err != nil {
		return nil, fmt.Errorf("release the daemon process: %w", err)
	}

	if err := waitForHealth(ctx, c, startupTimeout); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, tailFile(logPath, 2048))
	}
	fmt.Fprintf(os.Stderr, "spore: started a daemon on %s (log: %s)\n", cfg.Daemon.Addr, logPath)
	return c, nil
}

// waitForHealth polls until the daemon answers or the timeout passes.
func waitForHealth(ctx context.Context, c *client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		err := c.health(probe)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no daemon answered on %s within %s: %w", c.base, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tailFile returns the last n bytes of a file, for putting a failed daemon's
// own error message in front of the user instead of "it did not start".
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > n {
		if _, err := f.Seek(-n, 2); err != nil {
			return ""
		}
	}
	var b strings.Builder
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	b.Write(buf[:read])
	return b.String()
}
