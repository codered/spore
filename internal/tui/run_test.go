package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codered/spore/internal/daemon"
)

// flakyEvents opens a stream, fails the second attempt, and opens again.
type flakyEvents struct {
	fakeBackend
	emu   sync.Mutex
	calls int
	chans []chan daemon.WireEvent
}

func (f *flakyEvents) Events(context.Context) (<-chan daemon.WireEvent, error) {
	f.emu.Lock()
	defer f.emu.Unlock()
	f.calls++
	if f.calls == 2 {
		return nil, errors.New("daemon down")
	}
	ch := make(chan daemon.WireEvent, 1)
	f.chans = append(f.chans, ch)
	return ch, nil
}

func next(t *testing.T, msgs <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case m := <-msgs:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a message from the pump")
		return nil
	}
}

func TestPumpReconnectsAfterTheFeedCloses(t *testing.T) {
	oldMin, oldMax := minBackoff, maxBackoff
	minBackoff, maxBackoff = time.Millisecond, 2*time.Millisecond
	defer func() { minBackoff, maxBackoff = oldMin, oldMax }()

	f := &flakyEvents{}
	msgs := make(chan tea.Msg, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Pump(ctx, f, func(m tea.Msg) { msgs <- m })

	if _, ok := next(t, msgs).(connectedMsg); !ok {
		t.Fatal("first message is not connectedMsg")
	}
	f.emu.Lock()
	first := f.chans[0]
	f.emu.Unlock()
	first <- daemon.WireEvent{Session: "s", Type: daemon.WireText}
	if ev, ok := next(t, msgs).(eventMsg); !ok || ev.ev.Session != "s" {
		t.Fatal("the event was not forwarded")
	}
	close(first)

	if _, ok := next(t, msgs).(streamLostMsg); !ok {
		t.Fatal("a closed feed was not reported")
	}
	if lost, ok := next(t, msgs).(streamLostMsg); !ok || lost.err == nil {
		t.Fatal("the failed reattach was not reported with its error")
	}
	if _, ok := next(t, msgs).(connectedMsg); !ok {
		t.Fatal("the pump did not reconnect")
	}
}
