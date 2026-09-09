package subagent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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

func testSupervisor(t *testing.T, cfg config.SubagentConfig, r Runner) (*Supervisor, *store.Store) {
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

// blockingRunner holds a channel that the test controls, allowing the test to
// keep a child running while launching another one to test concurrency limits.
type blockingRunner struct {
	reply   string
	hold    <-chan struct{}
	ran     []string
	mu      sync.Mutex
}

func (b *blockingRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	b.mu.Lock()
	b.ran = append(b.ran, sessionID)
	b.mu.Unlock()
	ch := make(chan Event, 2)
	go func() {
		<-b.hold // Wait for the test to signal we can finish
		ch <- Event{Text: b.reply}
		close(ch)
	}()
	return ch, nil
}

func TestDifferentRootsGetOwnConcurrencyAllowance(t *testing.T) {
	hold := make(chan struct{})
	r := &blockingRunner{reply: "ok", hold: hold}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 3, MaxCostUSD: 100, MaxConcurrent: 1}, r)
	rootA := parentSession(t, st)
	rootB := parentSession(t, st)

	// Start one child under rootA and keep it running in a separate goroutine.
	childADone := make(chan struct{})
	go func() {
		_, err := sup.Run(ctxFor(rootA), rootA, "under A")
		if err != nil {
			t.Errorf("Run under rootA failed: %v", err)
		}
		close(childADone)
	}()
	// Give the goroutine time to start and enter the blocking runner.
	time.Sleep(50 * time.Millisecond)

	// Try to start a child under rootB while rootA's child is still running,
	// also in a goroutine since it will block on the runner.
	childBDone := make(chan struct{})
	var childBErr error
	var childB Status
	go func() {
		var err error
		childB, err = sup.Run(ctxFor(rootB), rootB, "under B")
		childBErr = err
		close(childBDone)
	}()

	// Give rootB's child time to get to the runner.
	time.Sleep(50 * time.Millisecond)

	// Release the blocking runner so both children can finish.
	close(hold)
	<-childADone
	<-childBDone

	if childBErr != nil {
		t.Errorf("Run under rootB while rootA child running: %v", childBErr)
	}
	if len(r.ran) != 2 {
		t.Errorf("ran %d children, want 2", len(r.ran))
	}
	if childB.State != store.RunDone {
		t.Errorf("rootB child state = %q, want done", childB.State)
	}
}

func TestConcurrencyCapEnforcedPerRoot(t *testing.T) {
	hold := make(chan struct{})
	r := &blockingRunner{reply: "ok", hold: hold}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 3, MaxCostUSD: 100, MaxConcurrent: 1}, r)
	root := parentSession(t, st)

	// Start one child under root and keep it running in a separate goroutine.
	childOneDone := make(chan struct{})
	go func() {
		_, err := sup.Run(ctxFor(root), root, "first")
		if err != nil {
			t.Errorf("first Run failed: %v", err)
		}
		close(childOneDone)
	}()
	// Give the goroutine time to start and enter the blocking runner.
	time.Sleep(50 * time.Millisecond)

	// Try to start another child under the same root.
	// This should fail because MaxConcurrent is 1 and one is already running.
	_, err := sup.Run(ctxFor(root), root, "second")
	if err == nil {
		t.Fatal("Run allowed second child under same root with MaxConcurrent=1")
	}
	if !strings.Contains(err.Error(), "max_concurrent") {
		t.Errorf("error = %v, want it to name max_concurrent", err)
	}

	// Release the blocking runner so the first child can finish.
	close(hold)
	<-childOneDone
}

func TestRefusalAfterStartLeavesNoRunningEntry(t *testing.T) {
	hold := make(chan struct{})
	r := &blockingRunner{reply: "ok", hold: hold}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 3, MaxCostUSD: 100, MaxConcurrent: 1}, r)
	root := parentSession(t, st)

	// Start one child and keep it running in a separate goroutine.
	childOneDone := make(chan struct{})
	go func() {
		_, err := sup.Run(ctxFor(root), root, "first")
		if err != nil {
			t.Errorf("first Run failed: %v", err)
		}
		close(childOneDone)
	}()
	time.Sleep(50 * time.Millisecond)

	// Try to start another child; it will be refused after creating the session.
	_, err := sup.Run(ctxFor(root), root, "second")
	if err == nil {
		t.Fatal("expected refusal")
	}

	// Release the first child so it can finish.
	close(hold)
	<-childOneDone

	// The refused child's run row should be terminal (not running).
	children, err := sup.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Count how many are in terminal state (should be 2, one done and one failed).
	var terminal int
	for _, ch := range children {
		if ch.State == store.RunFailed || ch.State == store.RunDone {
			terminal++
		}
	}
	if terminal != 2 {
		t.Errorf("terminal children = %d, want 2 (one done, one failed); got %+v", terminal, children)
	}

	// Make sure no child shows as running.
	for _, ch := range children {
		if ch.State == store.RunRunning {
			t.Errorf("child %s is stuck in running state", ch.ID)
		}
	}
}
