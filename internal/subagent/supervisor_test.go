package subagent

import (
	"context"
	"errors"
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
	reply string
	hold  <-chan struct{}
	ran   []string
	mu    sync.Mutex
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

func TestTryTrackRechecksTheCostCeiling(t *testing.T) {
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 0.05, MaxConcurrent: 4}, &stubRunner{})
	root := parentSession(t, st)

	// admit read the spend before this point, and the tree then crossed the
	// ceiling before the slot was reserved: the gap a concurrent spawn opens.
	if _, err := st.AppendMessage(context.Background(), store.Message{
		SessionID: root, Role: "assistant",
		BlocksJSON: []byte(`[{"type":"text","text":"x"}]`), CostUSD: 0.06,
	}); err != nil {
		t.Fatal(err)
	}

	err := sup.tryTrack(context.Background(), "late", root, &child{cancel: func() {}, root: root})
	if err == nil {
		t.Fatal("tryTrack reserved a slot for a tree over its cost ceiling")
	}
	if !strings.Contains(err.Error(), "max_cost_usd") {
		t.Errorf("error = %v, want it to name max_cost_usd", err)
	}
	sup.mu.Lock()
	_, tracked := sup.running["late"]
	sup.mu.Unlock()
	if tracked {
		t.Error("a refused child was left in the running set")
	}
}

// releaseRunner holds its turn open until release is closed, or until the
// child's context is cancelled. Unlike blockingRunner it honours
// cancellation, which is what the cancel tests need.
type releaseRunner struct {
	release chan struct{}
	reply   string
}

func (b *releaseRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	ch := make(chan Event, 2)
	go func() {
		defer close(ch)
		select {
		case <-b.release:
			ch <- Event{Text: b.reply}
		case <-ctx.Done():
			ch <- Event{Err: ctx.Err()}
		}
	}()
	return ch, nil
}

func waitForState(t *testing.T, sup *Supervisor, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := sup.Result(context.Background(), id)
		if err == nil && got.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := sup.Result(context.Background(), id)
	t.Fatalf("state = %q after 2s, want %q", got.State, want)
}

func TestSpawnReturnsBeforeTheChildFinishes(t *testing.T) {
	release := make(chan struct{})
	r := &releaseRunner{release: release, reply: "eventually"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	id, err := sup.Spawn(ctxFor(parent), parent, "background work")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, err := sup.Result(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.RunRunning {
		t.Errorf("State = %q immediately after Spawn, want running", got.State)
	}

	close(release)
	waitForState(t, sup, id, store.RunDone)
}

func TestSpawnSurvivesTheParentTurnEnding(t *testing.T) {
	release := make(chan struct{})
	r := &releaseRunner{release: release, reply: "done later"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	// The parent's turn context is cancelled the moment Spawn returns, which
	// is what happens when the turn that called agent_spawn ends.
	turnCtx, cancelTurn := context.WithCancel(ctxFor(parent))
	id, err := sup.Spawn(turnCtx, parent, "background work")
	if err != nil {
		t.Fatal(err)
	}
	cancelTurn()

	close(release)
	waitForState(t, sup, id, store.RunDone)
	got, _ := sup.Result(context.Background(), id)
	if got.Result != "done later" {
		t.Errorf("Result = %q, want the child's reply -- the child died with its parent's turn", got.Result)
	}
}

func TestSpawnRefusedWhenDetachedIsNotAllowed(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)
	// AllowDetached defaults to false: a one-shot CLI process has nothing to
	// collect a detached result.
	if _, err := sup.Spawn(ctxFor(parent), parent, "background"); err == nil {
		t.Fatal("Spawn was allowed outside the daemon")
	} else if !strings.Contains(err.Error(), "agent_run") {
		t.Errorf("error = %v, want it to point at agent_run", err)
	}
}

func TestCancelStopsAChildAndRecordsIt(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := &releaseRunner{release: release, reply: "never"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	id, err := sup.Spawn(ctxFor(parent), parent, "long job")
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, err := sup.Result(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel records the stop itself, before it returns.
	if got.State != store.RunInterrupted {
		t.Errorf("State = %q straight after Cancel, want interrupted", got.State)
	}
}

func TestCancelOfAFinishedChildIsNotRunning(t *testing.T) {
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, &stubRunner{reply: "ok"})
	parent := parentSession(t, st)

	done, err := sup.Run(ctxFor(parent), parent, "quick")
	if err != nil {
		t.Fatal(err)
	}
	err = sup.Cancel(context.Background(), done.ID)
	if !errors.Is(err, ErrNotRunning) {
		t.Errorf("Cancel of a finished child = %v, want ErrNotRunning", err)
	}
	got, _ := sup.Result(context.Background(), done.ID)
	if got.State != store.RunDone {
		t.Errorf("State = %q, want the finished run left as done", got.State)
	}
}

func TestSweepOrphansMarksRunsFromAPreviousProcess(t *testing.T) {
	ctx := context.Background()
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, &stubRunner{})
	parent := parentSession(t, st)
	orphan, err := st.CreateChildSession(ctx, "orphan", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartSubagentRun(ctx, store.SubagentRun{
		SessionID: orphan, ParentID: parent, Prompt: "from a dead process", Depth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := sup.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	got, _ := sup.Result(ctx, orphan)
	if got.State != store.RunInterrupted {
		t.Errorf("orphan state = %q, want interrupted", got.State)
	}
}

// recordingObserver keeps what the supervisor reported, in order.
type recordingObserver struct {
	mu      sync.Mutex
	started []string
	events  int
	settled []string
}

func (o *recordingObserver) ChildStarted(parentID, childID, prompt string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.started = append(o.started, parentID+">"+childID+":"+prompt)
}
func (o *recordingObserver) ChildEvent(childID string, ev Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.events++
}
func (o *recordingObserver) ChildSettled(childID, state string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.settled = append(o.settled, childID+":"+state)
}
func (o *recordingObserver) snapshot() ([]string, int, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.started...), o.events, append([]string(nil), o.settled...)
}

func TestObserverSeesAChildStartStreamAndSettle(t *testing.T) {
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, &stubRunner{reply: "ok"})
	obs := &recordingObserver{}
	sup.SetObserver(obs)
	parent := parentSession(t, st)

	got, err := sup.Run(ctxFor(parent), parent, "look")
	if err != nil {
		t.Fatal(err)
	}
	started, events, settled := obs.snapshot()
	if len(started) != 1 || started[0] != parent+">"+got.ID+":look" {
		t.Errorf("started = %v", started)
	}
	if events != 1 {
		t.Errorf("events = %d, want 1 (the stub sends one)", events)
	}
	if len(settled) != 1 || settled[0] != got.ID+":"+store.RunDone {
		t.Errorf("settled = %v, want exactly one done", settled)
	}
}

// Cancel records the stop and the child's goroutine then tries to record it
// again. Only the write that moved the row may report.
func TestObserverHearsACancelledChildSettleOnce(t *testing.T) {
	r := &releaseRunner{release: make(chan struct{}), reply: "never"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	sup.AllowDetached(true)
	obs := &recordingObserver{}
	sup.SetObserver(obs)
	parent := parentSession(t, st)

	id, err := sup.Spawn(ctxFor(parent), parent, "background")
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// Wait until the child's goroutine has untracked itself, so its own
	// finish has run.
	deadline := time.Now().Add(2 * time.Second)
	for {
		sup.mu.Lock()
		_, live := sup.running[id]
		sup.mu.Unlock()
		if !live {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cancelled child never untracked")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, _, settled := obs.snapshot(); len(settled) != 1 || settled[0] != id+":"+store.RunInterrupted {
		t.Fatalf("settled = %v, want exactly one interrupted", settled)
	}
}
