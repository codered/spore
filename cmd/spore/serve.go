package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codered/spore/internal/bridge/discord"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	mcphost "github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// cmdServe runs the daemon, or reports on and stops one that is already
// running. Flags: --status, --stop, --detach.
// recallMirrorInterval is how often the vector mirror catches up. It is a
// background chore, not a turn's business: a message becomes semantically
// searchable within this window, and is keyword-searchable immediately.
const recallMirrorInterval = 30 * time.Second

func cmdServe(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	status, stop, detach := false, false, false
	for _, a := range args {
		switch a {
		case "--status", "-status":
			status = true
		case "--stop", "-stop":
			stop = true
		case "--detach", "-detach":
			detach = true
		default:
			return fmt.Errorf("unknown serve flag %q (want --status, --stop or --detach)", a)
		}
	}

	pidPath := cfg.PidPath()
	switch {
	case status:
		pid, err := daemon.ReadPidFile(pidPath)
		if err != nil || !daemon.PidAlive(pid) {
			fmt.Println("not running")
			return nil
		}
		fmt.Printf("running (pid %d) on %s\n", pid, cfg.Daemon.Addr)
		return nil
	case stop:
		pid, err := daemon.ReadPidFile(pidPath)
		if err != nil {
			fmt.Println("not running")
			return nil
		}
		if !daemon.PidAlive(pid) {
			fmt.Println("not running (removing a stale pidfile)")
			return daemon.RemovePidFile(pidPath)
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("signal %d: %w", pid, err)
		}
		fmt.Printf("stopping (pid %d)\n", pid)
		return nil
	}

	// Atomically claim the pidfile. If a daemon is already running, extract its pid
	// for the user-facing error message.
	if err := daemon.AcquirePidFile(pidPath); err != nil {
		if errors.Is(err, daemon.ErrAlreadyRunning) {
			// Try to extract the pid from the error message for a better message.
			// The error format is "daemon is already running: pid N" when a live daemon
			// exists.
			pid, readErr := daemon.ReadPidFile(pidPath)
			if readErr == nil && daemon.PidAlive(pid) {
				return fmt.Errorf("spore is already running (pid %d); use spore serve --stop first", pid)
			}
		}
		return err
	}
	defer daemon.ReleasePidFile(pidPath)

	srv, mcpHost, recallMirror, err := buildServer(cfg, st)
	if err != nil {
		return err
	}
	// Close is the guarantee, not the graceful path: Supervise's wait() below
	// is what normally tears the host down, but every return between here and
	// that join (buildBridge failing, a panic recovered upstream, and so on)
	// must not leave a dialled MCP child (an npx process, say) running after
	// this function returns. Close is idempotent — markDown no-ops without a
	// session and killGroup guards a nil pid — so the deferred call is free
	// on the graceful path where wait() has already closed everything.
	defer mcpHost.Close()

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// MCP servers are supervised like the bridge: dialled in the background,
	// retried when they fail, and joined at shutdown so no child outlives the
	// daemon.
	mcpWait := func() {}
	if mcpHost.Configured() {
		mcpWait = mcphost.Supervise(ctx, mcpHost)
		if !detach {
			fmt.Printf("%d mcp server(s) configured\n", len(cfg.MCP.Servers))
		}
	}

	sched := scheduler.New(st, srv, nil)
	go sched.Run(ctx, time.Duration(cfg.Daemon.TickSeconds)*time.Second)

	if recallMirror != nil {
		// The mirror runs for the daemon's lifetime and never inside a turn:
		// a turn must not wait on a sidecar. Catching up immediately covers
		// whatever was written while the daemon was down.
		go func() {
			if _, err := recallMirror.Once(ctx); err != nil {
				slog.Default().Warn("recall mirror could not catch up at start", "error", err)
			}
			recallMirror.Run(ctx, recallMirrorInterval)
		}()
	}

	bridge, err := buildBridge(cfg, srv)
	if err != nil {
		// A misconfigured bridge is a config error and should stop startup:
		// silently serving without the surface you asked for is worse.
		return err
	}

	bridgeDone := make(chan struct{})
	if bridge != nil {
		go func() {
			defer close(bridgeDone)
			discord.Supervise(ctx, bridge, slog.Default())
		}()
		if !detach {
			fmt.Println("discord bridge enabled")
		}
	} else {
		close(bridgeDone)
	}

	if !detach {
		fmt.Printf("spore listening on http://%s\n", cfg.Daemon.Addr)
	}

	err = srv.Run(ctx, cfg.Daemon.Addr)
	// Cancel explicitly: Run can return without ctx being done (a listen
	// error), and Supervise is waiting on exactly that signal. The deferred
	// cancel fires too late — after this function has already returned and
	// main has closed the store.
	cancel()
	select {
	case <-bridgeDone:
	case <-time.After(5 * time.Second):
		slog.Warn("discord bridge did not stop within the shutdown grace period")
	}

	mcpDone := make(chan struct{})
	go func() { mcpWait(); close(mcpDone) }()
	select {
	case <-mcpDone:
	case <-time.After(10 * time.Second):
		slog.Warn("mcp servers did not stop within the shutdown grace period")
		mcpHost.Close()
	}
	return err
}
