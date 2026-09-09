package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/spore.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// stubRunner answers every child turn with a fixed reply, and records the
// sessions it was asked to run so the tests can assert on parentage.
type stubRunner struct {
	reply string
	ran   []string
}

func (s *stubRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	s.ran = append(s.ran, sessionID)
	ch := make(chan Event, 2)
	ch <- Event{Text: s.reply}
	close(ch)
	return ch, nil
}

func testSupervisor(t *testing.T, cfg config.SubagentConfig, r *stubRunner) (*Supervisor, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	sup := New(st, cfg)
	sup.AttachRunner(r)
	return sup, st
}

func parentSession(t *testing.T, st *store.Store) string {
	t.Helper()
	id, err := st.CreateSession(context.Background(), "parent", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ctxFor(sessionID string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{
		ID: sessionID, Profile: policy.ProfileLocal, Workspace: "/tmp/ws",
	})
}

func TestRunReturnsTheChildsAnswer(t *testing.T) {
	r := &stubRunner{reply: "no findings"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	got, err := sup.Run(ctxFor(parent), parent, "audit the policy tests")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Result != "no findings" {
		t.Errorf("Result = %q, want the child's reply", got.Result)
	}
	if got.State != store.RunDone {
		t.Errorf("State = %q, want done", got.State)
	}

	sess, ok, err := st.Session(context.Background(), got.ID)
	if err != nil || !ok {
		t.Fatalf("child session missing: ok=%v err=%v", ok, err)
	}
	if sess.ParentID != parent {
		t.Errorf("child ParentID = %q, want %q", sess.ParentID, parent)
	}
	if sess.Workspace != "/tmp/ws" {
		t.Errorf("child Workspace = %q, want the parent's", sess.Workspace)
	}
}

func TestRunRefusesBeyondMaxDepth(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	child, err := sup.Run(ctxFor(parent), parent, "first level")
	if err != nil {
		t.Fatalf("depth 1 must be allowed: %v", err)
	}
	if _, err := sup.Run(ctxFor(child.ID), child.ID, "second level"); err == nil {
		t.Fatal("Run at depth 2 was allowed, want a refusal")
	} else if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error = %v, want it to name the depth limit", err)
	}
}

func TestRunRefusesOverTheTreeCostCeiling(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 0.05, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	// Spend the ceiling in the parent before any child exists.
	if _, err := st.AppendMessage(context.Background(), store.Message{
		SessionID: parent, Role: "assistant",
		BlocksJSON: []byte(`[{"type":"text","text":"x"}]`), CostUSD: 0.06,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := sup.Run(ctxFor(parent), parent, "too expensive"); err == nil {
		t.Fatal("Run over the ceiling was allowed, want a refusal")
	} else if !strings.Contains(err.Error(), "cost") {
		t.Errorf("error = %v, want it to name the cost ceiling", err)
	}
}

func TestListReportsChildrenOfOneParent(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)
	other := parentSession(t, st)

	if _, err := sup.Run(ctxFor(parent), parent, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Run(ctxFor(other), other, "theirs"); err != nil {
		t.Fatal(err)
	}

	got, err := sup.List(context.Background(), parent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Prompt != "mine" {
		t.Errorf("List = %+v, want only this parent's child", got)
	}
}
