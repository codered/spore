package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/subagent"
)

type oneReply struct{}

func (oneReply) RunSite(ctx context.Context, sessionID, input, site string) (<-chan subagent.Event, error) {
	ch := make(chan subagent.Event, 2)
	ch <- agent.Event{Type: agent.EvText, Text: "found it"}
	ch <- agent.Event{Type: agent.EvTurnDone}
	close(ch)
	return ch, nil
}

func nextEvent(t *testing.T, ch <-chan WireEvent) WireEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
		return WireEvent{}
	}
}

// Wiring the supervisor into the server must be what installs the observer:
// this runs a real child through a supervisor attached the production way
// and reads the result off the global feed.
func TestAttachedSupervisorPublishesChildrenOnTheFeed(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	parent, err := srv.CreateSession(ctx, "p", "", store.SourceChat, policy.ProfileLocal)
	if err != nil {
		t.Fatal(err)
	}
	sup := subagent.New(srv.store, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4})
	sup.AttachRunner(oneReply{})
	srv.AttachSubagents(sup)

	feed, unsubscribe := srv.hub.SubscribeAll()
	defer unsubscribe()
	pctx := policy.WithSession(ctx, policy.Session{ID: parent, Profile: policy.ProfileLocal, Workspace: t.TempDir()})
	got, err := sup.Run(pctx, parent, "find the bug")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{WireSession, WireTurnStarted, WireText, WireTurnDone, WireAgentState}
	for i, typ := range want {
		ev := nextEvent(t, feed)
		if ev.Type != typ || ev.Session != got.ID {
			t.Fatalf("event %d = %+v, want %s for %s", i, ev, typ, got.ID)
		}
		switch typ {
		case WireSession:
			if ev.ParentID != parent || ev.Source != store.SourceSubagent || ev.Title != "find the bug" {
				t.Fatalf("session event = %+v", ev)
			}
		case WireAgentState:
			if ev.State != store.RunDone {
				t.Fatalf("agent_state = %+v, want done", ev)
			}
		}
	}
}
