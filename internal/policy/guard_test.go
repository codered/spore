package policy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type recordingRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingRunner) Specs() []provider.ToolSpec { return nil }
func (r *recordingRunner) ReadOnly(string) bool       { return true }
func (r *recordingRunner) Run(_ context.Context, c provider.Block) provider.Block {
	r.mu.Lock()
	r.calls = append(r.calls, c.Name)
	r.mu.Unlock()
	return provider.Block{Type: provider.BlockToolResult, ID: c.ID, Content: "ran " + c.Name}
}

type scriptedApprover struct {
	mu      sync.Mutex
	asked   []Ask
	answer  Answer
	err     error
	block   chan struct{} // when non-nil, Ask waits on it (and on ctx)
}

func (a *scriptedApprover) Ask(ctx context.Context, ask Ask) (Answer, error) {
	a.mu.Lock()
	a.asked = append(a.asked, ask)
	blocker := a.block
	a.mu.Unlock()
	if blocker != nil {
		select {
		case <-blocker:
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		}
	}
	return a.answer, a.err
}

func (a *scriptedApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

func guardFixture(t *testing.T, pc config.PolicyConfig, ap Approver) (*Guard, *recordingRunner, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, err := st.CreateSession(context.Background(), "guard test")
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingRunner{}
	g := NewGuard(inner, engine(t, pc), ap, st, nil)
	return g, inner, st, sid
}

func toolCall(name, id, args string) provider.Block {
	return provider.Block{Type: provider.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(args)}
}

func TestAllowRunsWithoutAsking(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Allow: []string{"fs_read"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("fs_read", "c1", `{"path":"/ws/a"}`))
	if got.IsError {
		t.Fatalf("allowed call returned an error: %q", got.Content)
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want 1", len(inner.calls))
	}
	if ap.count() != 0 {
		t.Error("an allowed call must not prompt")
	}
}

func TestDenyNeverReachesTheTool(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{
		Allow: []string{"shell_exec"},
		Deny:  []string{"shell_exec(matches sudo)"},
	}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("shell_exec", "c1", `{"command":"sudo rm -rf /tmp/x"}`))
	if !got.IsError {
		t.Fatal("a denied call must return a tool error")
	}
	if !strings.Contains(got.Content, "shell_exec(matches sudo)") {
		t.Errorf("the model must be told which rule denied it: %q", got.Content)
	}
	if len(inner.calls) != 0 {
		t.Error("a denied call reached the tool")
	}
	if ap.count() != 0 {
		t.Error("a denied call must never prompt for approval")
	}
}

func TestAskPromptsAndRunsOnApproval(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if got.IsError {
		t.Fatalf("approved call errored: %q", got.Content)
	}
	if ap.count() != 1 || len(inner.calls) != 1 {
		t.Errorf("asked %d times, ran %d times; want 1 and 1", ap.count(), len(inner.calls))
	}
	// The suspension must be resolved, not left dangling.
	pending, err := st.PendingCalls(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d pending calls left behind", len(pending))
	}
}

func TestAskDeniedReportsBackToTheModel(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError || !strings.Contains(got.Content, "declined") {
		t.Errorf("got %+v, want a tool error saying the user declined", got)
	}
	if len(inner.calls) != 0 {
		t.Error("a declined call reached the tool")
	}
}

func TestSessionScopeAnswersOnlyOnce(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	for i := 0; i < 3; i++ {
		if got := g.Run(ctx, toolCall("fs_write", "c", `{"path":"/ws/a"}`)); got.IsError {
			t.Fatalf("call %d errored: %q", i, got.Content)
		}
	}
	if ap.count() != 1 {
		t.Errorf("asked %d times, want 1 — the session answer must be remembered", ap.count())
	}
	if len(inner.calls) != 3 {
		t.Errorf("inner ran %d times, want 3", len(inner.calls))
	}
}

func TestSessionScopeDenialIsAlsoRemembered(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	for i := 0; i < 2; i++ {
		if got := g.Run(ctx, toolCall("fs_write", "c", `{"path":"/ws/a"}`)); !got.IsError {
			t.Fatalf("call %d was allowed after a session denial", i)
		}
	}
	if ap.count() != 1 {
		t.Errorf("asked %d times, want 1", ap.count())
	}
	if len(inner.calls) != 0 {
		t.Error("a session-denied tool ran anyway")
	}
}

func TestRememberedSessionAllowStillCannotBeatDeny(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{
		Ask:  []string{"shell_exec"},
		Deny: []string{"shell_exec(matches sudo)"},
	}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	if got := g.Run(ctx, toolCall("shell_exec", "c1", `{"command":"ls"}`)); got.IsError {
		t.Fatalf("benign call errored: %q", got.Content)
	}
	// The session now remembers "allow shell_exec". A denied command must
	// still be refused: deny is checked before the remembered answer.
	got := g.Run(ctx, toolCall("shell_exec", "c2", `{"command":"sudo ls"}`))
	if !got.IsError {
		t.Fatal("a remembered session approval overrode a deny rule")
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want only the benign call", len(inner.calls))
	}
}

func TestPatternScopeLearnsARule(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, _ := st.CreateSession(context.Background(), "t")
	var learned []string
	g := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), ap, st,
		func(d Decision, rule string) error {
			learned = append(learned, string(d)+" "+rule)
			return nil
		})
	g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	if len(learned) != 1 {
		t.Fatalf("learned = %v, want one rule written back", learned)
	}
	if !strings.HasPrefix(learned[0], "allow fs_write") {
		t.Errorf("learned %q, want an allow rule for fs_write", learned[0])
	}
}

func TestPatternForNarrowsToTheDirectory(t *testing.T) {
	got := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/src/a.go"}`)})
	if got != "fs_write(path matches /ws/src/**)" {
		t.Errorf("PatternFor = %q", got)
	}
	// With no path to generalise from, the pattern is the bare tool name.
	if got := PatternFor(Call{Tool: "shell_exec", Args: json.RawMessage(`{"command":"ls"}`)}); got != "shell_exec" {
		t.Errorf("PatternFor(shell) = %q, want the bare tool name", got)
	}
}

func TestUnansweredApprovalDeniesAtTheTimeout(t *testing.T) {
	ap := &scriptedApprover{block: make(chan struct{})} // never answered
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{
		Ask:             []string{"fs_write"},
		ApprovalTimeout: "50ms",
	}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError || !strings.Contains(got.Content, "timed out") {
		t.Errorf("got %+v, want a timeout denial", got)
	}
	if len(inner.calls) != 0 {
		t.Error("a timed-out call ran anyway")
	}
	pending, _ := st.PendingCalls(context.Background(), sid)
	if len(pending) != 0 {
		t.Errorf("%d pending calls left behind after a timeout", len(pending))
	}
}

func TestApproverErrorDenies(t *testing.T) {
	ap := &scriptedApprover{err: errors.New("no tty")}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Error("an approver failure must deny, not allow")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran despite an approver failure")
	}
}

func TestMissingSessionContextDenies(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, _, _ := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	// No WithSession: the guard cannot persist a suspension, so it refuses
	// rather than running unaudited.
	got := g.Run(context.Background(), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Error("a call with no session context was allowed")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran without a session")
	}
}

func TestGuardDelegatesSpecsAndReadOnly(t *testing.T) {
	ap := &scriptedApprover{}
	g, _, _, _ := guardFixture(t, config.PolicyConfig{}, ap)
	if !g.ReadOnly("anything") {
		t.Error("ReadOnly must delegate to the wrapped runner")
	}
	if g.Specs() != nil {
		t.Error("Specs must delegate to the wrapped runner")
	}
}
