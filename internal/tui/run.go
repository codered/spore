package tui

import (
	"context"
	"errors"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// Backoff bounds for reattaching to the feed. Variables so a test can make
// them fast.
var (
	minBackoff = 500 * time.Millisecond
	maxBackoff = 5 * time.Second
)

// Pump keeps the global feed attached for the life of ctx, turning it into
// messages. A closed feed -- daemon restart, or the daemon dropping a reader
// that fell behind -- is reported, retried with backoff, and followed by
// connectedMsg once it is back, which makes the model resync. That is the
// only recovery the interface needs.
func Pump(ctx context.Context, be Backend, send func(tea.Msg)) {
	delay := minBackoff
	for ctx.Err() == nil {
		ch, err := be.Events(ctx)
		if err == nil {
			send(connectedMsg{})
			delay = minBackoff
			for ev := range ch {
				send(eventMsg{ev: ev})
			}
		}
		if ctx.Err() != nil {
			return
		}
		send(streamLostMsg{err: err})
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, maxBackoff)
	}
}

// Run drives the full-screen program until the user quits, and returns what
// should be printed to ordinary scrollback afterwards.
func Run(ctx context.Context, be Backend, sessionID string, opts Options) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m := New(ctx, be, sessionID, opts)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	go Pump(ctx, be, p.Send)
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return "", err
	}
	return m.ExitSummary(), nil
}
