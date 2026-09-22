package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
)

// waitFor reads events until one of type typ arrives.
func waitFor(t *testing.T, ch <-chan Event, typ EventType) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed before a %s event", typ)
			}
			if ev.Type == typ {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for a %s event", typ)
		}
	}
}

func drainAll(ch <-chan Event) []Event {
	var out []Event
	for ev := range ch {
		out = append(out, ev)
	}
	return out
}

// lastAssistantText is the text of the newest assistant message on disk.
func lastAssistantText(t *testing.T, a *Agent, sessionID string) string {
	t.Helper()
	msgs, err := a.Store.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != string(provider.RoleAssistant) {
			continue
		}
		var blocks []provider.Block
		if err := json.Unmarshal(msgs[i].BlocksJSON, &blocks); err != nil {
			t.Fatal(err)
		}
		for _, b := range blocks {
			if b.Type == provider.BlockText {
				return b.Text
			}
		}
		return ""
	}
	return ""
}

func TestStopMidReplyPersistsThePartialTextWithAMarker(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	a, st := harness(t, provider.NewScript(provider.ScriptTurn{Text: "half a thought", Hold: hold}), nil)
	sid, err := st.CreateSession(context.Background(), "t", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ch, err := a.Run(ctx, sid, "think")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, EvText)
	cancel(ErrStopped)

	evs := drainAll(ch)
	if len(evs) == 0 {
		t.Fatal("no events after the stop")
	}
	for _, ev := range evs {
		if ev.Type == EvError {
			t.Fatalf("a stop produced an error event: %v", ev.Err)
		}
	}
	last := evs[len(evs)-1]
	if last.Type != EvStopped || !errors.Is(last.Err, ErrStopped) {
		t.Fatalf("last event = %+v, want EvStopped carrying ErrStopped", last)
	}
	if got, want := lastAssistantText(t, a, sid), "half a thought"+stopMarker; got != want {
		t.Fatalf("persisted text = %q, want %q", got, want)
	}
}

// A cancel with any other cause is the daemon shutting down, and must stay
// an error: nothing may claim the user stopped it.
func TestCancelWithoutTheStopCauseIsAnError(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	a, st := harness(t, provider.NewScript(provider.ScriptTurn{Text: "half", Hold: hold}), nil)
	sid, err := st.CreateSession(context.Background(), "t", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	ch, err := a.Run(ctx, sid, "think")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, ch, EvText)
	cancel(nil) // the cause is context.Canceled, as on shutdown

	evs := drainAll(ch)
	last := evs[len(evs)-1]
	if last.Type != EvError {
		t.Fatalf("last event = %+v, want EvError", last)
	}
	if strings.Contains(lastAssistantText(t, a, sid), "[stopped by you]") {
		t.Fatal("a shutdown was recorded as a stop")
	}
}

// blockingTools holds its one call until the turn's context ends.
type blockingTools struct{ entered chan struct{} }

func (b *blockingTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "slow", Description: "waits", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (b *blockingTools) ReadOnly(string) bool { return false }
func (b *blockingTools) Run(ctx context.Context, call provider.Block) provider.Block {
	close(b.entered)
	<-ctx.Done()
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "cancelled", IsError: true}
}

// A stop during tool execution must end the turn without another provider
// call, and leave a transcript the next turn can send. If the loop made one
// more call, it would consume the second scripted turn and the follow-up
// message would fail with "script exhausted".
func TestStopDuringToolsLeavesASendableTranscript(t *testing.T) {
	tools := &blockingTools{entered: make(chan struct{})}
	script := provider.NewScript(
		provider.ScriptTurn{ToolCalls: []provider.Block{{Type: provider.BlockToolUse, ID: "t1", Name: "slow", Input: json.RawMessage(`{}`)}}},
		provider.ScriptTurn{Text: "fresh answer"},
	)
	a, st := harness(t, script, tools)
	sid, err := st.CreateSession(context.Background(), "t", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	ch, err := a.Run(ctx, sid, "do the slow thing")
	if err != nil {
		t.Fatal(err)
	}
	<-tools.entered
	cancel(ErrStopped)
	evs := drainAll(ch)
	if last := evs[len(evs)-1]; last.Type != EvStopped {
		t.Fatalf("last event = %+v, want EvStopped", last)
	}

	ch2, err := a.Run(context.Background(), sid, "again")
	if err != nil {
		t.Fatal(err)
	}
	evs2 := collect(t, ch2) // collect fails the test on any EvError
	if last := evs2[len(evs2)-1]; last.Type != EvTurnDone {
		t.Fatalf("follow-up last event = %+v, want EvTurnDone", last)
	}
	if got := lastAssistantText(t, a, sid); got != "fresh answer" {
		t.Fatalf("follow-up text = %q, want %q", got, "fresh answer")
	}
}
