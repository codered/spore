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
	mu     sync.Mutex
	asked  []Ask
	answer Answer
	err    error
	block  chan struct{} // when non-nil, Ask waits on it (and on ctx)
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
	got, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/src/a.go"}`)})
	if got != "fs_write(path matches /ws/src/**)" || !ok {
		t.Errorf("PatternFor = (%q, %v)", got, ok)
	}
	// With no path to generalise from, the pattern degrades.
	got, ok = PatternFor(Call{Tool: "shell_exec", Args: json.RawMessage(`{"command":"ls"}`)})
	if got != "" || ok {
		t.Errorf("PatternFor(shell) = (%q, %v), want (\"\", false)", got, ok)
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

func TestMissingSessionDeniesEvenAnAllowedTool(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, _ := guardFixture(t, config.PolicyConfig{Allow: []string{"fs_read"}}, ap)
	// fs_read is allowed outright by policy, so this call never reaches the
	// ask branch. With no session on the context it must still be refused: an
	// unattributable call cannot be audited, and must not reach a tool.
	got := g.Run(context.Background(), toolCall("fs_read", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Fatal("an allowed tool ran with no session on the context")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool executed without a session")
	}
}

func TestSessionWithoutAProfileGetsTheStrictestRuleset(t *testing.T) {
	// A session attached without naming its trust level is a caller mistake,
	// and must not be quietly treated as the most trusted one.
	if _, p := SessionFrom(WithSession(context.Background(), "s1", "")); p != ProfileRemote {
		t.Errorf("profile = %q, want %q for a session attached with no profile", p, ProfileRemote)
	}
	if _, p := SessionFrom(WithSession(context.Background(), "s1", ProfileLocal)); p != ProfileLocal {
		t.Errorf("profile = %q, want an explicitly named profile to be honoured", p)
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

func TestNoDuplicateAuditRowsWhenApprovalRacesBetweenGuardAndBroker(t *testing.T) {
	// Simulate: a pending call is added, then Guard.Resolve is called out of
	// band to answer it (e.g., via HTTP), then the Guard's timeout path runs
	// and tries to write its own audit row. ResolvePendingCall should return
	// false to indicate the suspension was already claimed, preventing duplicate
	// audit rows.
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sid, err := st.CreateSession(ctx, "test session")
	if err != nil {
		t.Fatal(err)
	}

	// Add a pending call manually (simulating a suspension).
	pendingID, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid,
		ToolUseID: "c1",
		Tool:      "fs_read",
		ArgsJSON:  []byte(`{"path":"/etc/passwd"}`),
		Profile:   string(ProfileRemote),
		Rule:      "policy.deny",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Simulate Guard.Resolve being called out of band to answer the suspension
	// (e.g., via HTTP API or another client). This will claim the suspension
	// and write an approval row via ClaimPendingCall (which is atomic).
	guard := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{}), nil, st, nil)
	err = guard.Resolve(ctx, sid, pendingID, Answer{Allow: true, Scope: ScopeOnce})
	if err != nil {
		t.Fatalf("Guard.Resolve failed: %v", err)
	}

	// Now simulate ResolvePendingCall being called from the timeout path (the
	// race condition that caused duplicate rows). ResolvePendingCall should
	// return false because the suspension was already claimed by Guard.Resolve,
	// and the guard's audit write should be skipped.
	claimed, err := st.ResolvePendingCall(ctx, pendingID, "timeout")
	if err != nil {
		t.Fatalf("ResolvePendingCall: %v", err)
	}
	if claimed {
		t.Fatal("ResolvePendingCall claimed an already-resolved suspension: the race is not fixed")
	}

	// The test is successful if ResolvePendingCall correctly reported that the
	// suspension was not claimed. The guard's code is responsible for skipping
	// the audit write when claimed=false, preventing duplicate rows.
}

func TestPatternForReportsDegradation(t *testing.T) {
	cases := []struct {
		name    string
		call    Call
		want    string
		wantOK  bool
	}{
		{
			name:   "a single path generalises to its directory",
			call:   Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"/w/src/main.go"}`)},
			want:   "fs_read(path matches /w/src/**)",
			wantOK: true,
		},
		{
			name: "no path-shaped argument degrades",
			// This is the case the whole task exists for: the only pattern
			// derivable from a shell command is the bare tool name, which
			// would allow every shell_exec there is.
			call:   Call{Tool: "shell_exec", Args: json.RawMessage(`{"cmd":"ls -l"}`)},
			want:   "",
			wantOK: false,
		},
		{
			name:   "two paths are ambiguous and degrade",
			call:   Call{Tool: "fs_edit", Args: json.RawMessage(`{"from":"/w/a.go","to":"/w/b.go"}`)},
			want:   "",
			wantOK: false,
		},
		{
			name:   "a bare filename has no directory to generalise to",
			call:   Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"notes.md"}`)},
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PatternFor(tc.call)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("PatternFor = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
