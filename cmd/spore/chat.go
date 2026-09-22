package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/tui"
)

// cmdChat opens an interactive session. On a terminal it runs the full
// interface; with input or output redirected it falls back to the plain
// line-at-a-time loop, which is what scripts and tests drive.
func cmdChat(ctx context.Context, cfg *config.Config, sessionID, workspaceFlag string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	if sessionID == "" {
		ws, err := sessionWorkspace(workspaceFlag)
		if err != nil {
			return err
		}
		if sessionID, err = c.createSession(ctx, "chat", ws); err != nil {
			return err
		}
	} else if workspaceFlag != "" {
		// Resuming does not move a session -- a transcript, its recall hits
		// and its file references stay coherent. --workspace is the one way
		// to say "move it anyway", and it rewrites the row.
		ws, err := sessionWorkspace(workspaceFlag)
		if err != nil {
			return err
		}
		if err := c.setWorkspace(ctx, sessionID, ws); err != nil {
			return err
		}
	}
	if interactiveTerminal() {
		return chatTUI(ctx, cfg, c, sessionID)
	}
	return chatPlain(ctx, cfg, c, sessionID)
}

// interactiveTerminal reports whether both ends of the CLI are a terminal.
// The full interface needs to read keys and to repaint, so it must not be
// started when either end is a pipe.
func interactiveTerminal() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}

// chatTUI runs the full-screen interface on the session. Once it has left
// the alternate screen it prints what the interface hands back -- the last
// exchange and how to resume -- to ordinary scrollback, so quitting does not
// make the conversation vanish from view.
func chatTUI(ctx context.Context, cfg *config.Config, c *client, sessionID string) error {
	summary, err := tui.Run(ctx, tuiBackend{c: c, showCost: cfg.ShowCost}, sessionID, tui.Options{ShowCost: cfg.ShowCost})
	if err != nil {
		return err
	}
	fmt.Print(summary)
	return nil
}

// runPlainSlash handles commands that have a direct daemon API in the
// line-oriented loop. It reports whether text was a command; true means the
// caller must not send it as a model turn.
func runPlainSlash(ctx context.Context, c *client, sessionID, text string, out io.Writer) (bool, error) {
	switch text {
	case "/skills":
		list, err := c.listSkills(ctx, sessionID)
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprint(out, formatSkills(list))
		return true, err
	case "/agents":
		list, err := c.listAgents(ctx, sessionID)
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprint(out, formatAgents(list))
		return true, err
	}
	return false, nil
}

// chatPlain is the line-oriented loop used when stdin or stdout is not a
// terminal. It has no line editing and no styling on purpose: it must behave
// exactly like a pipe consumer.
func chatPlain(ctx context.Context, cfg *config.Config, c *client, sessionID string) error {
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
			case daemon.WireTurnDone, daemon.WireError, daemon.WireStopped:
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
			if handled, err := runPlainSlash(ctx, c, sessionID, text, os.Stdout); handled {
				if err != nil {
					fmt.Fprintln(os.Stderr, "command failed:", err)
				}
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
