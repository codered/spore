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

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// lines: one goroutine pulls user input from stdin and sends it here,
	// ensuring exactly one reader of stdinLines.
	lines := make(chan string)
	go func() {
		for stdinLines.Scan() {
			lines <- stdinLines.Text()
		}
		close(lines)
	}()

	// turnDone carries one signal per finished turn. The main loop waits on
	// it when a message is pending, which is also when it handles approvals:
	// so the main goroutine is the only consumer of `lines`.
	turnDone := make(chan struct{}, 1)
	connected := make(chan struct{})
	streamErr := make(chan error, 1)

	// approvals: events that need answering are sent here by the stream
	// goroutine, which does not call approve() itself (that runs on the main
	// loop, the only reader of input).
	approvals := make(chan daemon.WireEvent, 4)

	go func() {
		streamErr <- c.streamFrom(streamCtx, sessionID, connected, func(ev daemon.WireEvent) error {
			printEvent(ev, cfg.ShowCost)
			switch ev.Type {
			case daemon.WireApproval:
				select {
				case approvals <- ev:
				default:
					// Approval queue full; drop it. The guard will time out
					// and deny. Not ideal, but bounds memory and prevents
					// a backlog in an unusual multi-client scenario.
					fmt.Fprintf(os.Stderr, "approval for %s arrived but the queue is full; answer at http://%s/#%s\n",
						ev.Tool, cfg.Daemon.Addr, sessionID)
				}
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

	// ap: approver that reads from `lines` via chanLines. This runs on the
	// main goroutine, which is the only consumer, preventing races.
	ap := terminalApprover{lines: chanLines{ch: lines}, out: os.Stdout}

	handleApproval := func(ev daemon.WireEvent) {
		// Runs on the MAIN goroutine, which is the only reader of `lines`.
		// That is what makes answering an approval safe while a prompt is
		// pending — there is no second reader to race with.
		approve(streamCtx, c, ap, sessionID, ev)
	}

	for {
		fmt.Print("\n> ")
		select {
		case ev := <-approvals:
			handleApproval(ev)
			continue
		case line, ok := <-lines:
			if !ok {
				return nil
			}
			text := strings.TrimSpace(line)
			if text == "" {
				continue
			}
			if err := c.send(ctx, sessionID, text); err != nil {
				fmt.Fprintln(os.Stderr, "send failed:", err)
				continue
			}
			// Wait for the turn, still servicing approvals — a turn that
			// suspends is answered from here, not from the stream goroutine.
			for waiting := true; waiting; {
				select {
				case ev := <-approvals:
					handleApproval(ev)
				case <-turnDone:
					waiting = false
				case err := <-streamErr:
					return fmt.Errorf("lost the event stream: %w", err)
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case err := <-streamErr:
			return fmt.Errorf("lost the event stream: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
