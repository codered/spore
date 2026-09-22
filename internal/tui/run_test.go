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
	// ends closes the matching stream; each is safe to call more than once.
	ends []func()
}

// Events keeps the Backend contract: the channel closes when the stream ends
// for any reason, including ctx ending. Without that, Pump would sit in its
// read loop after the test cancels, and could never be waited for.
func (f *flakyEvents) Events(ctx context.Context) (<-chan daemon.WireEvent, error) {
	f.emu.Lock()
	defer f.emu.Unlock()
	f.calls++
	if f.calls == 2 {
		return nil, errors.New("daemon down")
	}
	ch := make(chan daemon.WireEvent, 1)
	var once sync.Once
	end := func() { once.Do(func() { close(ch) }) }
	go func() {
		<-ctx.Done()
		end()
	}()
	f.chans = append(f.chans, ch)
	f.ends = append(f.ends, end)
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
	done := make(chan struct{})
	go func() {
		defer close(done)
		Pump(ctx, f, func(m tea.Msg) {
			select {
			case msgs <- m:
			case <-ctx.Done():
			}
		})
	}()
	// Pump reads the backoff variables. It must have returned before the
	// deferred restore above writes them, or the two race.
	defer func() {
		cancel()
		<-done
	}()

	if _, ok := next(t, msgs).(connectedMsg); !ok {
		t.Fatal("first message is not connectedMsg")
	}
	f.emu.Lock()
	first, endFirst := f.chans[0], f.ends[0]
	f.emu.Unlock()
	first <- daemon.WireEvent{Session: "s", Type: daemon.WireText}
	if ev, ok := next(t, msgs).(eventMsg); !ok || ev.ev.Session != "s" {
		t.Fatal("the event was not forwarded")
	}
	endFirst()

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
