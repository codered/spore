package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// askEverything is a ruleset written out in full. Building it from
// config.Default() would add the baseline deny set, which silently satisfies
// the assertions these tests exist to make.
func askEverything(t *testing.T, workspace string) *Engine {
	t.Helper()
	e, err := NewEngine(config.PolicyConfig{
		Default:         "ask",
		Workspace:       workspace,
		Allow:           []string{},
		Ask:             []string{"*"},
		Deny:            []string{},
		ApprovalTimeout: "5m",
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func treeStore(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/spore.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, err := st.CreateSession(ctx, "root", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	childA, err := st.CreateChildSession(ctx, "a", "/tmp/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	childB, err := st.CreateChildSession(ctx, "b", "/tmp/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	return st, root, childA, childB
}

func pendingIn(t *testing.T, st *store.Store, sessionID string) int64 {
	t.Helper()
	id, err := st.AddPendingCall(context.Background(), store.PendingCall{
		SessionID: sessionID, ToolUseID: "tu-" + sessionID, Tool: "shell_exec",
		Profile: string(ProfileLocal), Rule: "*", ArgsJSON: []byte(`{"cmd":"ls"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPendingTreeIncludesDescendants(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	pendingIn(t, st, root)
	pendingIn(t, st, childA)

	own, err := g.Pending(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Errorf("Pending = %d rows, want only the root's own", len(own))
	}

	all, err := g.PendingTree(ctx, root)
	if err != nil {
		t.Fatalf("PendingTree: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("PendingTree = %d rows, want the root's and the child's", len(all))
	}
}

func TestParentMayAnswerAChildsApproval(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	if err := g.Resolve(ctx, root, id, Answer{Allow: true, Scope: ScopeOnce}); err != nil {
		t.Errorf("the parent could not answer its child's approval: %v", err)
	}
}

func TestChildMayNotAnswerItsOwnApproval(t *testing.T) {
	ctx := context.Background()
	st, _, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	err := g.Resolve(ctx, childA, id, Answer{Allow: true, Scope: ScopeOnce})
	if err == nil {
		t.Fatal("a sub-agent answered its own approval")
	}
	if !strings.Contains(err.Error(), "own approval") {
		t.Errorf("error = %v, want it to say a sub-agent cannot answer its own approval", err)
	}
}

func TestSiblingMayNotAnswerAnothersApproval(t *testing.T) {
	ctx := context.Background()
	st, _, childA, childB := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	if err := g.Resolve(ctx, childB, id, Answer{Allow: true, Scope: ScopeOnce}); err == nil {
		t.Fatal("a sibling answered another sub-agent's approval")
	}
}

func TestRememberedDecisionsAreRootScoped(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)

	// The human allowed shell_exec for the session earlier.
	if err := st.RecordApproval(ctx, root, "shell_exec", []byte(`{}`), string(DecisionAllow), string(ScopeSession)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := rootScopedDecision(ctx, st, childA, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != string(DecisionAllow) {
		t.Errorf("child decision = (%q, %v), want the root's allow", got, ok)
	}
	_ = root
}

// Fix 3: Root-scoped decisions are written and read correctly through Guard.Resolve
func TestRootScopedDecisionWrittenViaResolve(t *testing.T) {
	ctx := context.Background()
	st, root, childA, childB := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	// Child A raises a pending call
	id := pendingIn(t, st, childA)
	// Root resolves it with ScopeSession
	if err := g.Resolve(ctx, root, id, Answer{Allow: true, Scope: ScopeSession}); err != nil {
		t.Fatalf("root could not resolve child's approval: %v", err)
	}

	// Both the child and a sibling should see the allow decision at the root
	got, ok, err := rootScopedDecision(ctx, st, childA, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != string(DecisionAllow) {
		t.Errorf("child decision = (%q, %v), want root's allow after Resolve", got, ok)
	}

	got2, ok2, err := rootScopedDecision(ctx, st, childB, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok2 || got2 != string(DecisionAllow) {
		t.Errorf("sibling decision = (%q, %v), want root's allow after Resolve", got2, ok2)
	}
}

// Fix 3: Root-scoped decisions are written when the approval comes via Guard.Run
func TestRootScopedDecisionWrittenViaRun(t *testing.T) {
	ctx := context.Background()
	st, _, childA, childB := treeStore(t)
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}}
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), ap, st, nil)

	// A child makes a call that needs approval
	sess := WithSession(ctx, Session{ID: childA, Profile: ProfileLocal, Workspace: "/tmp/ws"})
	got := g.Run(sess, provider.Block{Type: provider.BlockToolUse, ID: "c1", Name: "shell_exec", Input: []byte(`{"cmd":"ls"}`)})

	if got.IsError {
		t.Fatalf("child call was denied: %q", got.Content)
	}

	// The approval was answered with ScopeSession, so the root should have the decision
	decision, ok, err := rootScopedDecision(ctx, st, childA, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || decision != string(DecisionAllow) {
		t.Errorf("child decision after Run = (%q, %v), want root's allow", decision, ok)
	}

	// Sibling should also see the decision
	decision2, ok2, err := rootScopedDecision(ctx, st, childB, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok2 || decision2 != string(DecisionAllow) {
		t.Errorf("sibling decision after Run = (%q, %v), want root's allow", decision2, ok2)
	}
}

// Fix 2: Broker.Answer with RootID - child cannot answer its own, root can
func TestBrokerAnswerChecksRootID(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	// Child raises a pending call
	id := pendingIn(t, st, childA)

	// Attempting to answer with the child's own session ID should fail in Broker.Answer
	// This is a synchronous fast-path authorization check
	// We'll verify via Guard.Resolve since that's what the HTTP handler calls
	err := g.Resolve(ctx, childA, id, Answer{Allow: true, Scope: ScopeOnce})
	if err == nil {
		t.Fatal("child should not be able to answer its own approval")
	}
	if !strings.Contains(err.Error(), "own approval") {
		t.Errorf("error = %v, want it to mention own approval", err)
	}

	// Now a new approval from a different child
	id2 := pendingIn(t, st, childA)
	// Root should be able to answer
	err = g.Resolve(ctx, root, id2, Answer{Allow: true, Scope: ScopeOnce})
	if err != nil {
		t.Errorf("root could not answer: %v", err)
	}
}

