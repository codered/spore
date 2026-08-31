package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// cmdServe runs the daemon, or reports on and stops one that is already
// running. Flags: --status, --stop, --detach.
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

	// A second daemon on the same database and port helps nobody.
	if pid, err := daemon.ReadPidFile(pidPath); err == nil && daemon.PidAlive(pid) {
		return fmt.Errorf("spore is already running (pid %d); use spore serve --stop first", pid)
	}
	if err := daemon.WritePidFile(pidPath); err != nil {
		return err
	}
	defer daemon.RemovePidFile(pidPath)

	srv, err := buildServer(cfg, st)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sched := scheduler.New(st, srv, nil)
	go sched.Run(ctx, time.Duration(cfg.Daemon.TickSeconds)*time.Second)

	if !detach {
		fmt.Printf("spore listening on http://%s\n", cfg.Daemon.Addr)
	}
	return srv.Run(ctx, cfg.Daemon.Addr)
}
