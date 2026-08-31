package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

func cmdChat(ctx context.Context, cfg *config.Config, sessionID string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	if sessionID == "" {
		if sessionID, err = c.createSession(ctx, "chat"); err != nil {
			return err
		}
	}
	fmt.Printf("session %s — ctrl-d to exit\n", sessionID)
	fmt.Printf("web UI: http://%s/#%s\n", cfg.Daemon.Addr, sessionID)

	ap := terminalApprover{lines: stdinLines, out: os.Stdout}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// turnDone carries one signal per finished turn. The prompt loop waits on
	// it, which is also what keeps the approval prompt and the input prompt
	// from reading stdin at the same time: while a turn is running, the loop
	// is blocked here and the stream goroutine is the only reader.
	turnDone := make(chan struct{}, 1)
	connected := make(chan struct{})
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- c.streamFrom(streamCtx, sessionID, connected, func(ev daemon.WireEvent) error {
			printEvent(ev, cfg.ShowCost)
			switch ev.Type {
			case daemon.WireApproval:
				approve(streamCtx, c, ap, sessionID, ev)
			case daemon.WireTurnDone, daemon.WireError:
				select {
				case turnDone <- struct{}{}:
				default:
				}
			}
			return nil
		})
	}()
	select {
	case <-connected:
	case err := <-streamErr:
		return fmt.Errorf("attach to the session: %w", err)
	}

	sc := stdinLines
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := c.send(ctx, sessionID, line); err != nil {
			fmt.Fprintln(os.Stderr, "send failed:", err)
			continue
		}
		select {
		case <-turnDone:
		case err := <-streamErr:
			return fmt.Errorf("lost the event stream: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
