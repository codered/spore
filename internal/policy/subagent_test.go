package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
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
