package main

import (
	"context"
	"fmt"
	"os"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// printEvent renders one wire event on the terminal. It is the CLI's whole
// display layer, shared by once and chat.
func printEvent(ev daemon.WireEvent, showCost bool) {
	switch ev.Type {
	case daemon.WireText:
		fmt.Print(ev.Text)
	case daemon.WireToolCall:
		fmt.Printf("\n  → %s %s\n", ev.Tool, ev.Args)
	case daemon.WireToolResult:
		mark := "←"
		if ev.IsError {
			mark = "✗"
		}
		fmt.Printf("  %s %d bytes\n", mark, len(ev.Content))
	case daemon.WireTurnDone:
		cost := ""
		if showCost {
			cost = fmt.Sprintf(" · $%.4f", ev.CostUSD)
		}
		fmt.Printf("\n\n[%s · %d in / %d out%s]\n", ev.Model, ev.TokensIn, ev.TokensOut, cost)
	case daemon.WireError:
		fmt.Fprintf(os.Stderr, "\nturn failed: %s\n", ev.Error)
	}
}

// errTurnFinished ends a stream cleanly once the turn it was watching is
// over. stream returns it, and callers treat it as success.
var errTurnFinished = fmt.Errorf("turn finished")

// approve renders an approval on the terminal and posts the answer back.
// Errors are reported and swallowed: the guard denies on its own timeout, so
// a failure to answer degrades to a denial rather than a hung turn.
func approve(ctx context.Context, c *client, ap terminalApprover, sessionID string, ev daemon.WireEvent) {
	ans, err := ap.Ask(ctx, policy.Ask{
		SessionID: sessionID, Tool: ev.Tool, Args: []byte(ev.Args),
		Rule: ev.Rule, PendingID: ev.PendingID, Pattern: ev.Pattern,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\napproval not answered: %v\n", err)
		return
	}
	if err := c.resolve(ctx, sessionID, ev.PendingID, ans); err != nil {
		fmt.Fprintf(os.Stderr, "\ncould not send the answer: %v\n", err)
	}
}

func cmdOnce(ctx context.Context, cfg *config.Config, prompt, workspaceFlag string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	ws, err := sessionWorkspace(workspaceFlag)
	if err != nil {
		return err
	}
	sessionID, err := c.createSession(ctx, prompt, ws)
	if err != nil {
		return err
	}

	ap := terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Attach BEFORE posting the message, and wait for the connection rather
	// than for the goroutine to start: an event published between the two
	// would otherwise be missed, and for a one-shot turn that can be the
	// whole reply.
	connected := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- c.streamFrom(streamCtx, sessionID, connected, func(ev daemon.WireEvent) error {
			printEvent(ev, cfg.ShowCost)
			switch ev.Type {
			case daemon.WireApproval:
				approve(streamCtx, c, ap, sessionID, ev)
			case daemon.WireTurnDone, daemon.WireError:
				return errTurnFinished
			}
			return nil
		})
	}()
	select {
	case <-connected:
	case err := <-errc:
		return fmt.Errorf("attach to the session: %w", err)
	}

	if err := c.send(ctx, sessionID, prompt); err != nil {
		return err
	}
	if err := <-errc; err != nil && err != errTurnFinished {
		return err
	}
	return nil
}
