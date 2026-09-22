package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tui"
)

// tuiBackend adapts the daemon client to the interface internal/tui drives.
type tuiBackend struct {
	c        *client
	showCost bool
}

var _ tui.Backend = tuiBackend{}

func (b tuiBackend) Sessions(ctx context.Context) ([]daemon.SessionJSON, error) {
	return b.c.sessions(ctx)
}

func (b tuiBackend) Transcript(ctx context.Context, id string) (daemon.TranscriptJSON, error) {
	return b.c.transcript(ctx, id)
}

// Events opens the global feed and returns once the daemon has accepted it,
// so the caller's connectedMsg is true when it is sent. The channel closes
// when the stream ends for any reason.
func (b tuiBackend) Events(ctx context.Context) (<-chan daemon.WireEvent, error) {
	ch := make(chan daemon.WireEvent, 256)
	connected := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		defer close(ch)
		errc <- b.c.streamAll(ctx, connected, func(ev daemon.WireEvent) error {
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-connected:
		return ch, nil
	case err := <-errc:
		if err == nil {
			err = errors.New("the event feed closed before it opened")
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b tuiBackend) Send(ctx context.Context, id, text string) error { return b.c.send(ctx, id, text) }

func (b tuiBackend) Stop(ctx context.Context, id string) error { return b.c.stop(ctx, id) }

func (b tuiBackend) Resolve(ctx context.Context, id string, pendingID int64, ans policy.Answer) error {
	return b.c.resolve(ctx, id, pendingID, ans)
}

func (b tuiBackend) CancelAgent(ctx context.Context, parent, child string) error {
	return b.c.cancelAgent(ctx, parent, child)
}

// NewSession opens a chat session. An empty workspace means the current
// directory, as `spore chat` itself does; a relative one is resolved from it.
func (b tuiBackend) NewSession(ctx context.Context, workspace string) (string, error) {
	dir, err := sessionWorkspace(workspace)
	if err != nil {
		return "", err
	}
	return b.c.createSession(ctx, "chat", dir)
}

func (b tuiBackend) Slash(ctx context.Context, id, cmd string) (string, error) {
	switch cmd {
	case "clear":
		if err := b.c.clear(ctx, id); err != nil {
			return "", err
		}
		return "cleared", nil
	case "compact":
		res, err := b.c.compact(ctx, id)
		if err != nil {
			return "", err
		}
		return compactSummary(res), nil
	case "context", "usage":
		data, err := b.c.getTranscript(ctx, id)
		if err != nil {
			return "", err
		}
		if cmd == "context" {
			return formatContext(data), nil
		}
		return formatUsage(data, b.showCost), nil
	case "skills":
		list, err := b.c.listSkills(ctx, id)
		if err != nil {
			return "", err
		}
		return formatSkills(list), nil
	case "agents":
		list, err := b.c.listAgents(ctx, id)
		if err != nil {
			return "", err
		}
		return formatAgents(list), nil
	}
	return "", fmt.Errorf("unknown command: %s", cmd)
}
