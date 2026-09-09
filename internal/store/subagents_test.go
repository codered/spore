package store

import (
	"context"
	"testing"
)

func newRunTree(t *testing.T, s *Store) (parent, child string) {
	t.Helper()
	ctx := context.Background()
	parent, err := s.CreateSession(ctx, "parent", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	child, err = s.CreateChildSession(ctx, "child", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	return parent, child
}

func TestSubagentRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)

	err := s.StartSubagentRun(ctx, SubagentRun{
		SessionID: child, ParentID: parent, Prompt: "audit the policy tests", Depth: 1,
	})
	if err != nil {
		t.Fatalf("StartSubagentRun: %v", err)
	}

	got, ok, err := s.SubagentRun(ctx, child)
	if err != nil || !ok {
		t.Fatalf("SubagentRun: ok=%v err=%v", ok, err)
	}
	if got.State != "running" {
		t.Errorf("State = %q, want running", got.State)
	}
	if got.Prompt != "audit the policy tests" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if !got.EndedAt.IsZero() {
		t.Errorf("EndedAt = %v, want zero while running", got.EndedAt)
	}

	if err := s.FinishSubagentRun(ctx, child, "done", "no findings", ""); err != nil {
		t.Fatalf("FinishSubagentRun: %v", err)
	}
	got, _, err = s.SubagentRun(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.Result != "no findings" {
		t.Errorf("after finish: state=%q result=%q", got.State, got.Result)
	}
	if got.EndedAt.IsZero() {
		t.Error("EndedAt is still zero after finishing")
	}
}

func TestInterruptRunningSubagentsMarksOrphans(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)

	if err := s.StartSubagentRun(ctx, SubagentRun{SessionID: child, ParentID: parent, Prompt: "p", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChildSession(ctx, "other", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartSubagentRun(ctx, SubagentRun{SessionID: other, ParentID: parent, Prompt: "q", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSubagentRun(ctx, other, "done", "ok", ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptRunningSubagents(ctx)
	if err != nil {
		t.Fatalf("InterruptRunningSubagents: %v", err)
	}
	if n != 1 {
		t.Errorf("interrupted %d rows, want 1", n)
	}
	got, _, _ := s.SubagentRun(ctx, child)
	if got.State != "interrupted" {
		t.Errorf("orphan state = %q, want interrupted", got.State)
	}
	// A finished run must not be rewritten by the sweep.
	fin, _, _ := s.SubagentRun(ctx, other)
	if fin.State != "done" {
		t.Errorf("finished run was rewritten to %q", fin.State)
	}
}

func TestTreeCostSumsDescendants(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)
	grand, err := s.CreateChildSession(ctx, "grand", "/tmp/ws", child)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := s.CreateSession(ctx, "unrelated", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		session string
		cost    float64
	}{{parent, 0.01}, {child, 0.02}, {grand, 0.04}, {outside, 9.00}} {
		if _, err := s.AppendMessage(ctx, Message{
			SessionID: tc.session, Role: "assistant",
			BlocksJSON: []byte(`[{"type":"text","text":"x"}]`), CostUSD: tc.cost,
		}); err != nil {
			t.Fatalf("AppendMessage(%s): %v", tc.session, err)
		}
	}

	got, err := s.TreeCost(ctx, parent)
	if err != nil {
		t.Fatalf("TreeCost: %v", err)
	}
	if want := 0.07; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("TreeCost = %v, want %v (an unrelated session must not count)", got, want)
	}
}
