package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	sid, err := st.CreateSession(context.Background(), "guard test", "")
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingRunner{}
	g := NewGuard(inner, engine(t, pc), ap, st)
	return g, inner, st, sid
}

func toolCall(name, id, args string) provider.Block {
	return provider.Block{Type: provider.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(args)}
}

func TestAllowRunsWithoutAsking(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Allow: []string{"fs_read"}}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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

// A human takes seconds to answer. The writes that close the suspension must
// get their own time budget once the answer is in; a budget that started
// before the wait has expired by then, which left the row pending forever
// (the session read as blocked, so every later message queued) and dropped
// the audit row that remembers a session-scope answer.
func TestAnApprovalAnsweredAfterTheBookkeepingWindowIsStillRecorded(t *testing.T) {
	old := bookkeepingTimeout
	bookkeepingTimeout = 20 * time.Millisecond
	t.Cleanup(func() { bookkeepingTimeout = old })
	release := make(chan struct{})
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}, block: release}
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})

	time.AfterFunc(100*time.Millisecond, func() { close(release) }) // the human answers late
	if got := g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/a"}`)); got.IsError {
		t.Fatalf("approved call errored: %q", got.Content)
	}
	pending, err := st.PendingCalls(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d pending calls left behind after a late answer", len(pending))
	}
	if got := g.Run(ctx, toolCall("fs_write", "c2", `{"path":"/ws/b"}`)); got.IsError {
		t.Fatalf("second call errored: %q", got.Content)
	}
	if ap.count() != 1 || len(inner.calls) != 2 {
		t.Fatalf("asked %d times, ran %d; want 1 and 2 — the session answer must be remembered", ap.count(), len(inner.calls))
	}
}

func TestAskDeniedReportsBackToTheModel(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"}), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
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
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
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

func proposals(t *testing.T, st *store.Store) []store.Refinement {
	t.Helper()
	rows, err := st.Refinements(context.Background(), store.RefineProposed, 100)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// "p" answers this call once and queues the rule for review. Nothing is
// written to the config from the ask window.
func TestPatternAnswerProposesARule(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
	g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	if len(inner.calls) != 1 {
		t.Fatalf("the call ran %d times, want once", len(inner.calls))
	}
	rows := proposals(t, st)
	if len(rows) != 1 {
		t.Fatalf("proposals = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.Kind != store.KindPolicyAllow || r.Trigger != store.RefineTriggerApproval || r.SessionID != sid {
		t.Errorf("row = %+v", r)
	}
	if r.After == nil || *r.After != "fs_write(path matches /ws/src/**)" || r.Target != *r.After {
		t.Errorf("rule: target=%q after=%v", r.Target, r.After)
	}
	if !strings.HasPrefix(r.RoundID, "approval-") {
		t.Errorf("round = %q", r.RoundID)
	}
	if !strings.Contains(r.Rationale, "/ws/src/a.go") {
		t.Errorf("rationale %q should name the path", r.Rationale)
	}
	// The next call in the pattern still asks: nothing changed policy.
	g.Run(ctx, toolCall("fs_write", "c2", `{"path":"/ws/src/b.go"}`))
	if ap.count() != 2 {
		t.Errorf("asked %d times, want 2", ap.count())
	}
	// A second "p" for the same rule adds no row.
	if n := len(proposals(t, st)); n != 1 {
		t.Errorf("proposals after a repeat = %d, want 1", n)
	}
}

// The offer is withheld when the proposed rule could not decide this call.
func TestPatternIsNotOfferedWhenItCouldNotApply(t *testing.T) {
	cases := []struct {
		name string
		pc   config.PolicyConfig
		prof Profile
	}{
		{"narrow written ask", config.PolicyConfig{Ask: []string{"fs_write(path matches **/src/**)"}}, ProfileLocal},
		{"remote profile", config.PolicyConfig{Ask: []string{"fs_write"},
			Profiles: map[string]config.ProfilePolicy{"remote": {Deny: []string{"memory"}}}}, ProfileRemote},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
			g, _, st, sid := guardFixture(t, tc.pc, ap)
			ctx := WithSession(context.Background(), Session{ID: sid, Profile: tc.prof, Workspace: "/ws"})
			g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
			if ap.count() != 1 {
				t.Fatalf("asked %d times", ap.count())
			}
			if p := ap.asked[0].Pattern; p != "" {
				t.Errorf("pattern offered: %q", p)
			}
			if n := len(proposals(t, st)); n != 0 {
				t.Errorf("proposals = %d, want 0", n)
			}
		})
	}
}

func TestPatternForNarrowsToTheDirectory(t *testing.T) {
	got, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/src/a.go"}`)}, "/ws")
	if got != "fs_write(path matches /ws/src/**)" || !ok {
		t.Errorf("PatternFor = (%q, %v)", got, ok)
	}
	// With no path to generalise from, the pattern degrades.
	got, ok = PatternFor(Call{Tool: "shell_exec", Args: json.RawMessage(`{"command":"ls"}`)}, "/ws")
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
	got := g.Run(WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"}), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
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
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"}), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Error("an approver failure must deny, not allow")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran despite an approver failure")
	}
	// Same as the timeout: the suspension is answered, so it must not stay
	// pending. A row left behind here comes back as a prompt for a call that
	// was already denied.
	pending, _ := st.PendingCalls(context.Background(), sid)
	if len(pending) != 0 {
		t.Errorf("%d pending calls left behind after an approver failure", len(pending))
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
	if got := SessionFrom(WithSession(context.Background(), Session{ID: "s1"})); got.Profile != ProfileRemote {
		t.Errorf("profile = %q, want %q for a session attached with no profile", got.Profile, ProfileRemote)
	}
	if got := SessionFrom(WithSession(context.Background(), Session{ID: "s1", Profile: ProfileLocal})); got.Profile != ProfileLocal {
		t.Errorf("profile = %q, want an explicitly named profile to be honoured", got.Profile)
	}
}

func TestSessionFromCarriesWorkspace(t *testing.T) {
	ctx := WithSession(context.Background(), Session{ID: "s1", Profile: ProfileLocal, Workspace: "/ws/a"})
	got := SessionFrom(ctx)
	if got.ID != "s1" || got.Profile != ProfileLocal || got.Workspace != "/ws/a" {
		t.Fatalf("session = %+v", got)
	}
	if WorkspaceFrom(ctx) != "/ws/a" {
		t.Fatalf("WorkspaceFrom = %q", WorkspaceFrom(ctx))
	}
}

// Nothing attached still fails toward the strictest ruleset, and names no
// directory at all rather than a default one.
func TestSessionFromEmptyContext(t *testing.T) {
	got := SessionFrom(context.Background())
	if got.Profile != ProfileRemote {
		t.Fatalf("profile = %q, want %q", got.Profile, ProfileRemote)
	}
	if got.Workspace != "" {
		t.Fatalf("workspace = %q, want empty", got.Workspace)
	}
}

// The plan amendment this task adds: Engine.Evaluate keeps a fallback to the
// configured ceiling when a session names no directory, but that fallback
// exists for `spore policy check`, which has no session at all. A real
// session that reaches the guard with no workspace -- a row whose workspace
// was never backfilled, or a caller that forgot to set it -- must be refused
// outright rather than silently judged against the ceiling: the filesystem
// and shell tools grow their own workspace refusal later, but nothing stops
// an MCP tool call from reaching the wrong directory without this guard.
func TestMissingWorkspaceDeniesEvenAnAllowedTool(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Allow: []string{"fs_read"}}, ap)
	// A session with an id and a profile, but no workspace.
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal})
	got := g.Run(ctx, toolCall("fs_read", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Fatal("a call with no workspace on the session was allowed")
	}
	if !strings.Contains(got.Content, "workspace") {
		t.Errorf("refusal text = %q, want it to name the missing workspace", got.Content)
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran despite no workspace on the session")
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

	sid, err := st.CreateSession(ctx, "test session", "")
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
	guard := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{}), nil, st)
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

func TestResolveDowngradesADegradedPatternAnswer(t *testing.T) {
	ctx := context.Background()
	g, _, st, sid := guardFixture(t, config.PolicyConfig{}, nil)

	id, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "remote", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A client asks for ScopePattern on a call that has no pattern.
	if err := g.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopePattern}); err != nil {
		t.Fatal(err)
	}

	if n := len(proposals(t, st)); n != 0 {
		t.Fatalf("a rule was proposed for a call with no pattern: %d rows", n)
	}
	// The audit row must say what actually happened, not what was asked for.
	scope, err := lastApprovalScope(t, st, sid)
	if err != nil {
		t.Fatal(err)
	}
	if scope != string(ScopeOnce) {
		t.Fatalf("audit scope = %q, want %q", scope, ScopeOnce)
	}
}

func TestPatternAnswerLeavesTheConfigUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	orig := []byte("[policy]\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	pc := config.PolicyConfig{Default: "ask", ApprovalTimeout: "5m", Workspace: "/ws", Ask: []string{"fs_write"}}
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	g, _, _, sid := guardFixture(t, pc, ap)
	_ = NewReloader(path, pc, g) // wired exactly as the daemon wires it
	g.Run(WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"}),
		toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatalf("config changed:\n%s", got)
	}
}

// lastApprovalScope reads the scope of the newest audit row for a session.
func lastApprovalScope(t *testing.T, st *store.Store, sessionID string) (string, error) {
	t.Helper()
	rows, err := st.Approvals(context.Background(), sessionID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no approval rows for session %s", sessionID)
	}
	return rows[len(rows)-1].Scope, nil
}

func TestPatternForReportsDegradation(t *testing.T) {
	cases := []struct {
		name   string
		call   Call
		want   string
		wantOK bool
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
			name:   "a file at the workspace root has no directory narrower than the workspace",
			call:   Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"notes.md"}`)},
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PatternFor(tc.call, "/w")
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("PatternFor = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}

}

// TestSkillInstallIsNeverLearnable checks that PatternFor never returns a
// pattern scope for skill_install, even when its arguments look path-shaped.
func TestSkillInstallIsNeverLearnable(t *testing.T) {
	cfg := config.PolicyConfig{
		Workspace:       "/ws",
		Default:         "ask",
		ApprovalTimeout: "5m",
		Allow:           []string{"skill_load"},
		Ask:             []string{"skill_install"},
	}
	_, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pattern, learnable := PatternFor(Call{
		Tool: "skill_install",
		Args: json.RawMessage(`{"path":"/tmp/foo"}`),
	}, "/ws")
	if learnable {
		t.Fatalf("skill_install must never offer a pattern scope, even with a path-shaped argument")
	}
	if pattern != "" {
		t.Fatalf("expected empty pattern for skill_install, got %q", pattern)
	}
}

// TestSkillInstallDeniedUnderRemote checks that the remote profile denies
// skill_install outright.
func TestSkillInstallDeniedUnderRemote(t *testing.T) {
	cfg := config.PolicyConfig{
		Workspace:       "/ws",
		Default:         "ask",
		ApprovalTimeout: "5m",
		Allow:           []string{"skill_load"},
		Ask:             []string{"skill_install"},
		Profiles: map[string]config.ProfilePolicy{
			"remote": {Deny: []string{"skill_install"}},
		},
	}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := eng.Evaluate(Session{
		ID:        "s",
		Profile:   ProfileRemote,
		Workspace: "/ws",
	}, Call{Tool: "skill_install", Args: json.RawMessage(`{"name":"x"}`)})
	if res.Decision != DecisionDeny {
		t.Fatalf("a bridge session must not install a skill, got %v", res.Decision)
	}
}

// A path deny is recoverable: the model can send the same call with a path
// inside the workspace. The message says what was wrong and does not tell it
// to stop, which every other deny does.
func TestMCPPathDenyExplainsItselfAndInvitesARetry(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{
		Workspace: "/ws",
		Ask:       []string{"mcp__*"},
		Deny:      []string{"mcp__*(any path outside workspace)"},
		MCPPaths:  map[string]config.MCPPathMode{"srv": {Checked: true, Cwd: "/ws"}},
	}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
	got := g.Run(ctx, toolCall("mcp__srv__read", "c1", `{"path":"/etc/passwd"}`))

	if !got.IsError {
		t.Fatal("a denied call must return a tool error")
	}
	if len(inner.calls) != 0 {
		t.Error("a denied call must not reach the tool")
	}
	if ap.count() != 0 {
		t.Error("a deny is never escalated to a human")
	}
	if !strings.Contains(got.Content, `mcp__*(any path outside workspace)`) {
		t.Errorf("content = %q, want the rule named", got.Content)
	}
	if !strings.Contains(got.Content, "/etc/passwd") {
		t.Errorf("content = %q, want the offending path named", got.Content)
	}
	if strings.Contains(got.Content, "Do not retry") {
		t.Errorf("content = %q, want no do-not-retry: a corrected path is the recovery", got.Content)
	}
}

// Every other deny keeps today's wording.
func TestOtherDeniesKeepTheDoNotRetryWording(t *testing.T) {
	ap := &scriptedApprover{}
	g, _, _, sid := guardFixture(t, config.PolicyConfig{
		Workspace: "/ws",
		Deny:      []string{"shell_exec(matches sudo)"},
	}, ap)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
	got := g.Run(ctx, toolCall("shell_exec", "c1", `{"command":"sudo id"}`))
	if !strings.Contains(got.Content, "Do not retry this call; choose another approach.") {
		t.Errorf("content = %q, want the unchanged wording", got.Content)
	}
}

// TestAgentNoteIsNeverLearnable checks that PatternFor never returns a
// pattern scope for agent_note, even when its arguments look path-shaped. A
// file of standing instructions shapes every later turn in this workspace,
// exactly as a skill shapes every later turn everywhere, so each write is
// approved on its own. The path-shaped argument is the point: agent_note
// takes no path today, and resting the property on that accident is how it
// gets lost to a later argument rename.
func TestAgentNoteIsNeverLearnable(t *testing.T) {
	pattern, learnable := PatternFor(Call{
		Tool: "agent_note",
		Args: json.RawMessage(`{"path":"/tmp/foo"}`),
	}, "/ws")
	if learnable {
		t.Fatal("agent_note must never offer a pattern scope, even with a path-shaped argument")
	}
	if pattern != "" {
		t.Fatalf("expected empty pattern for agent_note, got %q", pattern)
	}
}

// TestAgentNoteDeniedUnderRemote checks that the remote profile denies
// agent_note outright: a Discord user must not be able to rewrite the
// operator's standing instructions.
//
// The config is built through config.Load rather than config.Default so the
// baseline deny is in force. A policy test built on Default silently loses
// the assertion it exists to make.
func TestAgentNoteDeniedUnderRemote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Deliberately minimal: everything policy-shaped comes from the shipped
	// defaults, so this asserts what spore actually ships rather than what
	// the test just wrote into a file.
	if err := os.WriteFile(path, []byte(`
default_model = "anthropic/claude-sonnet-5"
[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	eng, err := NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	res := eng.Evaluate(
		Session{ID: "s", Profile: ProfileRemote, Workspace: dir},
		Call{Tool: "agent_note", Args: json.RawMessage(`{"text":"x"}`)},
	)
	if res.Decision != DecisionDeny {
		t.Fatalf("remote profile decision for agent_note = %q, want deny", res.Decision)
	}
}

// realTempDir is a temp directory with symlinks resolved, because Resolve
// follows them: on macOS t.TempDir sits under a symlinked /var, and a
// pattern compared against the unresolved name would never match.
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The bug: a pattern kept the path as the model wrote it, so a rule learned
// from "src/a.go" never matched "<ws>/src/a.go", the same file.
func TestPatternForIsAbsoluteWhateverTheArgumentForm(t *testing.T) {
	ws := realTempDir(t)
	rel, okRel := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"src/a.go"}`)}, ws)
	abs, okAbs := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"` + ws + `/src/a.go"}`)}, ws)
	want := "fs_write(path matches " + ws + "/src/**)"
	if rel != want || abs != want || !okRel || !okAbs {
		t.Fatalf("relative = (%q, %v), absolute = (%q, %v), want both %q", rel, okRel, abs, okAbs, want)
	}
	e := engine(t, config.PolicyConfig{Workspace: ws, Learned: config.LearnedPolicy{Allow: []string{rel}}})
	for _, arg := range []string{`{"path":"src/b.go"}`, `{"path":"` + ws + `/src/b.go"}`, `{"path":"src/../src/c.go"}`} {
		if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: ws}, Call{Tool: "fs_write", Args: json.RawMessage(arg)}); got.Decision != DecisionAllow {
			t.Errorf("%s: decision = %s by %q, want allow by the learned rule", arg, got.Decision, got.Rule)
		}
	}
}

// A relative rule applied in every workspace; an absolute one only where it
// was learned.
func TestALearnedPatternStaysInItsWorkspace(t *testing.T) {
	a, b := realTempDir(t), realTempDir(t)
	rule, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"src/a.go"}`)}, a)
	if !ok {
		t.Fatal("no pattern")
	}
	e := engine(t, config.PolicyConfig{Workspace: a, Learned: config.LearnedPolicy{Allow: []string{rule}}})
	if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: b}, Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"src/a.go"}`)}); got.Decision == DecisionAllow {
		t.Errorf("a rule learned in %s allowed a write in %s", a, b)
	}
}

func TestPatternForResolvesHomeDotsAndSymlinks(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	ws := realTempDir(t)
	if real, err := filepath.EvalSymlinks(home); err == nil {
		home = real
	}
	if got, _ := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"~/spore-pattern-test/a.go"}`)}, ws); got != "fs_write(path matches "+filepath.Join(home, "spore-pattern-test")+"/**)" {
		t.Errorf("~ path = %q", got)
	}
	if got, _ := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"x/../src/a.go"}`)}, ws); got != "fs_write(path matches "+ws+"/src/**)" {
		t.Errorf(".. path = %q", got)
	}

	// A workspace reached through a symlink learns the real directory, which
	// is what the matcher resolves arguments to.
	real := filepath.Join(ws, "real")
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ws, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	rule, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"src/a.go"}`)}, link)
	if !ok || rule != "fs_write(path matches "+real+"/src/**)" {
		t.Fatalf("symlinked workspace = (%q, %v), want the real directory", rule, ok)
	}
	e := engine(t, config.PolicyConfig{Workspace: link, Learned: config.LearnedPolicy{Allow: []string{rule}}})
	if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: link}, Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"src/b.go"}`)}); got.Decision != DecisionAllow {
		t.Errorf("through the symlink: %s by %q, want allow", got.Decision, got.Rule)
	}
}

// A directory whose name the rule syntax would read as a wildcard or a list
// separator must not become a rule: it would allow more than was approved,
// or not parse.
func TestPatternForRefusesNamesTheRuleCannotSayLiterally(t *testing.T) {
	ws := realTempDir(t)
	for _, dir := range []string{"a*b", "a?b", "a,b", `a"b`} {
		args, _ := json.Marshal(map[string]string{"path": dir + "/f.go"})
		if got, ok := PatternFor(Call{Tool: "fs_write", Args: args}, ws); ok || got != "" {
			t.Errorf("%q: PatternFor = (%q, %v), want no pattern", dir, got, ok)
		}
	}
}

// GlobSource quoted the glob a byte at a time through string(byte), which
// re-encodes every byte above 0x7f as a two-byte rune: any rule naming a
// non-ASCII directory was saved and never matched.
func TestPathGlobsMatchNonASCIINames(t *testing.T) {
	for _, dir := range []string{"héllo", "日本", "naïve café"} {
		re, err := compilePathGlob("/ws/" + dir + "/**")
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString("/ws/" + dir + "/f.go") {
			t.Errorf("/ws/%s/** does not match a file inside it", dir)
		}
	}
	ws := realTempDir(t)
	rule, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"héllo/a.go"}`)}, ws)
	if !ok {
		t.Fatal("no pattern for a non-ASCII directory")
	}
	e := engine(t, config.PolicyConfig{Workspace: ws, Learned: config.LearnedPolicy{Allow: []string{rule}}})
	if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: ws}, Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"héllo/b.go"}`)}); got.Decision != DecisionAllow {
		t.Errorf("learned %q: decision %s by %q, want allow", rule, got.Decision, got.Rule)
	}
}

// Without a workspace there is no root to refuse, and an absolute path
// directly in the home directory would learn all of it.
func TestPatternForNeedsAWorkspace(t *testing.T) {
	if got, ok := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/src/a.go"}`)}, ""); ok || got != "" {
		t.Errorf("PatternFor with no workspace = (%q, %v), want no pattern", got, ok)
	}
}

// The rule proposed on answer is the pattern stored when the call was
// suspended, never one re-derived from the arguments: between the ask and
// the answer a directory can become a symlink, or the session be re-rooted,
// and the human approved what they were shown.
func TestResolveProposesTheStoredPatternNotARederivedOne(t *testing.T) {
	ctx := context.Background()
	g, _, st, sid := guardFixture(t, config.PolicyConfig{}, nil)
	const shown = "fs_write(path matches /ws/notes/**)"
	// The arguments would now derive something else entirely.
	id, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "fs_write", Profile: "local", Rule: "fs_write",
		ArgsJSON: []byte(`{"path":"/elsewhere/x/a.go"}`), Pattern: shown,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopePattern}); err != nil {
		t.Fatal(err)
	}
	rows := proposals(t, st)
	if len(rows) != 1 || rows[0].After == nil || *rows[0].After != shown {
		t.Fatalf("proposals = %+v, want exactly the pattern that was shown", rows)
	}
}

// The suspension stores exactly the pattern the approver was shown, so an
// answer that arrives after a restart (through Resolve, with no live
// waiter) learns what the human saw.
func TestRunStoresThePatternItOffers(t *testing.T) {
	ctx := context.Background()
	ws := realTempDir(t)
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeOnce}}
	g, _, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}, Workspace: ws}, ap)
	g.Run(WithSession(ctx, Session{ID: sid, Profile: ProfileLocal, Workspace: ws}), toolCall("fs_write", "c1", `{"path":"notes/a.txt"}`))
	if len(ap.asked) != 1 {
		t.Fatalf("asked %d times, want 1", len(ap.asked))
	}
	ask := ap.asked[0]
	if want := "fs_write(path matches " + ws + "/notes/**)"; ask.Pattern != want {
		t.Fatalf("offered %q, want %q", ask.Pattern, want)
	}
	p, found, err := st.PendingCallByID(ctx, ask.PendingID)
	if err != nil || !found || p.Pattern != ask.Pattern {
		t.Errorf("stored pattern = %q (%v %v), want the offered %q", p.Pattern, found, err, ask.Pattern)
	}
}
