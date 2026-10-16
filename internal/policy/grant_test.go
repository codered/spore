package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// grantFixture is guardFixture with the baseline deny rules in place. Bare
// config.Default() lacks them, and a deny test built on it proves nothing.
func grantFixture(t *testing.T, pc config.PolicyConfig, ap Approver) (*Guard, *recordingRunner, *store.Store, string, time.Time) {
	t.Helper()
	pc.Deny = append(config.BaselineDeny(), pc.Deny...)
	g, inner, st, sid := guardFixture(t, pc, ap)
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return clock }
	return g, inner, st, sid, clock
}

func grantCtx(sid string) context.Context {
	return WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
}

func lastApproval(t *testing.T, st *store.Store, sid string) store.Approval {
	t.Helper()
	rows, err := st.Approvals(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatalf("no approval rows for session %s", sid)
	}
	return rows[len(rows)-1]
}

func TestGrantRunsAskCall(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, st, sid, clock := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	g.StartGrant(sid, clock.Add(5*time.Minute))

	got := g.Run(grantCtx(sid), toolCall("shell_exec", "c1", `{"command":"ls"}`))
	if got.IsError {
		t.Fatalf("granted call errored: %q", got.Content)
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want 1", len(inner.calls))
	}
	if ap.count() != 0 {
		t.Errorf("a granted call prompted %d times", ap.count())
	}
	pending, err := st.PendingCalls(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d pending rows written for a granted call", len(pending))
	}
	if row := lastApproval(t, st, sid); row.Decision != "allow" || row.Scope != "grant" {
		t.Errorf("audit row = %s/%s, want allow/grant", row.Decision, row.Scope)
	}
}

func TestGrantKeepsDeny(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, sid, clock := grantFixture(t, config.PolicyConfig{
		Ask:  []string{"shell_exec", "fs_write"},
		Deny: []string{"shell_exec(matches curl)"},
	}, ap)
	g.StartGrant(sid, clock.Add(5*time.Minute))

	// The baseline deny: a write outside the workspace.
	base := g.Run(grantCtx(sid), toolCall("fs_write", "c1", `{"path":"/etc/passwd"}`))
	// A rule from the operator's own config.
	cfg := g.Run(grantCtx(sid), toolCall("shell_exec", "c2", `{"command":"curl example.test"}`))

	for name, got := range map[string]string{"baseline": base.Content, "config": cfg.Content} {
		if !strings.Contains(got, "denied by policy rule") {
			t.Errorf("%s deny content = %q, want the deny wording", name, got)
		}
	}
	if !base.IsError || !cfg.IsError {
		t.Error("a deny under a grant must return a tool error")
	}
	if len(inner.calls) != 0 {
		t.Errorf("a denied call reached the tool: %v", inner.calls)
	}
	if ap.count() != 0 {
		t.Error("a deny under a grant must never prompt")
	}
}

func TestGrantCoversChild(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, st, sid, clock := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	child, err := st.CreateChildSession(context.Background(), "child", "/ws", sid)
	if err != nil {
		t.Fatal(err)
	}
	g.StartGrant(sid, clock.Add(5*time.Minute))

	got := g.Run(grantCtx(child), toolCall("shell_exec", "c1", `{"command":"ls"}`))
	if got.IsError {
		t.Fatalf("child's granted call errored: %q", got.Content)
	}
	if len(inner.calls) != 1 || ap.count() != 0 {
		t.Errorf("child ran %d times and asked %d times; want 1 and 0", len(inner.calls), ap.count())
	}
}

func TestGrantOverridesSessionDecline(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, st, sid, clock := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	if err := st.RecordApproval(context.Background(), sid, "shell_exec", json.RawMessage(`{"command":"ls"}`), "deny", "session"); err != nil {
		t.Fatal(err)
	}
	g.StartGrant(sid, clock.Add(5*time.Minute))

	got := g.Run(grantCtx(sid), toolCall("shell_exec", "c1", `{"command":"ls"}`))
	if got.IsError {
		t.Fatalf("granted call was refused by an earlier decline: %q", got.Content)
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want 1", len(inner.calls))
	}
}

func TestGrantExpiredAsksAgain(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, _, _, sid, clock := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	g.StartGrant(sid, clock.Add(time.Minute))
	clock = clock.Add(2 * time.Minute)
	g.now = func() time.Time { return clock }

	g.Run(grantCtx(sid), toolCall("shell_exec", "c1", `{"command":"ls"}`))
	if ap.count() != 1 {
		t.Errorf("an expired grant asked %d times, want 1", ap.count())
	}
	if _, live := g.GrantUntil(sid); live {
		t.Error("GrantUntil reports an expired grant as live")
	}
}

func TestEndGrant(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, _, _, sid, clock := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	g.StartGrant(sid, clock.Add(5*time.Minute))
	g.EndGrant(sid)

	g.Run(grantCtx(sid), toolCall("shell_exec", "c1", `{"command":"ls"}`))
	if ap.count() != 1 {
		t.Errorf("after EndGrant the call asked %d times, want 1", ap.count())
	}
}

func TestGrantTableIsSafeForConcurrentUse(t *testing.T) {
	g, _, _, sid, clock := grantFixture(t, config.PolicyConfig{}, &scriptedApprover{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				switch (i + j) % 3 {
				case 0:
					g.StartGrant(sid, clock.Add(time.Minute))
				case 1:
					g.GrantUntil(sid)
				default:
					g.EndGrant(sid)
				}
			}
		}(i)
	}
	wg.Wait()
}

// runGrantedPending is the second check, made after the pending row is
// written and before anyone is asked. It must claim the row or refuse.
func TestGrantedPendingClaimsOrRefuses(t *testing.T) {
	ctx := context.Background()
	ap := &scriptedApprover{}
	g, inner, st, sid, _ := grantFixture(t, config.PolicyConfig{Ask: []string{"shell_exec"}}, ap)
	call := toolCall("shell_exec", "c1", `{"command":"ls"}`)

	id, err := st.AddPendingCall(ctx, store.PendingCall{SessionID: sid, ToolUseID: "c1", Tool: "shell_exec", ArgsJSON: call.Input, Profile: string(ProfileLocal)})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.runGrantedPending(grantCtx(sid), sid, call, id); got.IsError {
		t.Fatalf("claimed grant call errored: %q", got.Content)
	}
	if len(inner.calls) != 1 {
		t.Errorf("claimed call ran %d times, want 1", len(inner.calls))
	}
	if row := lastApproval(t, st, sid); row.Decision != "allow" || row.Scope != "grant" {
		t.Errorf("audit row = %s/%s, want allow/grant", row.Decision, row.Scope)
	}

	// A row another path already answered must not run the call.
	id2, err := st.AddPendingCall(ctx, store.PendingCall{SessionID: sid, ToolUseID: "c2", Tool: "shell_exec", ArgsJSON: call.Input, Profile: string(ProfileLocal)})
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := st.ResolvePendingCall(ctx, id2, "deny"); err != nil || !claimed {
		t.Fatalf("setup: claimed=%v err=%v", claimed, err)
	}
	got := g.runGrantedPending(grantCtx(sid), sid, toolCall("shell_exec", "c2", `{"command":"ls"}`), id2)
	if !got.IsError || len(inner.calls) != 1 {
		t.Errorf("a row answered elsewhere ran the call: %+v (inner ran %d)", got, len(inner.calls))
	}

	// A row a sweep allowed through Guard.Resolve: the call runs, and the
	// resolver's audit row is the only one for it.
	id3, err := st.AddPendingCall(ctx, store.PendingCall{SessionID: sid, ToolUseID: "c3", Tool: "shell_exec", ArgsJSON: call.Input, Profile: string(ProfileLocal)})
	if err != nil {
		t.Fatal(err)
	}
	before := approvalCount(t, st, sid)
	if err := g.Resolve(ctx, sid, id3, Answer{Allow: true, Scope: ScopeOnce}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if after := approvalCount(t, st, sid); after != before+1 {
		t.Fatalf("Resolve wrote %d audit rows, want 1", after-before)
	}
	got = g.runGrantedPending(grantCtx(sid), sid, toolCall("shell_exec", "c3", `{"command":"ls"}`), id3)
	if got.IsError || len(inner.calls) != 2 {
		t.Errorf("a row allowed elsewhere did not run: %+v (inner ran %d)", got, len(inner.calls))
	}
	if after := approvalCount(t, st, sid); after != before+1 {
		t.Errorf("the grant wrote a second audit row for an allow the resolver already recorded: %d rows added", after-before)
	}
}

func approvalCount(t *testing.T, st *store.Store, sid string) int {
	t.Helper()
	rows, err := st.Approvals(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// blockRecorder is a recording approver that also reports blocks.
type blockRecorder struct {
	scriptedApprover
	mu     sync.Mutex
	blocks []string
}

func (b *blockRecorder) Blocked(rootID, sessionID, tool, rule, detail string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocks = append(b.blocks, fmt.Sprintf("%s|%s|%s|%s|%s", rootID, sessionID, tool, rule, detail))
}

func TestBlockedReported(t *testing.T) {
	ctx := context.Background()
	ap := &blockRecorder{}
	g, _, st, sid, _ := grantFixture(t, config.PolicyConfig{
		Ask:      []string{"shell_exec", "mcp__*"},
		Deny:     []string{"shell_exec(matches curl)"},
		MCPPaths: map[string]config.MCPPathMode{"srv": {Checked: true, Cwd: "/ws"}},
	}, ap)
	child, err := st.CreateChildSession(ctx, "child", "/ws", sid)
	if err != nil {
		t.Fatal(err)
	}

	// A config deny from a child reports the root, the child, and no detail.
	g.Run(WithSession(ctx, Session{ID: child, Profile: ProfileLocal, Workspace: "/ws"}), toolCall("shell_exec", "c1", `{"command":"curl x"}`))

	// A path-containment deny carries a detail the notice can show.
	g.Run(grantCtx(sid), toolCall("mcp__srv__read", "c2", `{"path":"/etc/passwd"}`))

	// A missing workspace is refused before evaluation and reports nothing.
	g.Run(WithSession(ctx, Session{ID: sid, Profile: ProfileLocal}), toolCall("shell_exec", "c3", `{"command":"ls"}`))

	if len(ap.blocks) != 2 {
		t.Fatalf("blocks = %q, want exactly the two deny-rule refusals", ap.blocks)
	}
	if want := sid + "|" + child + "|shell_exec|shell_exec(matches curl)|"; ap.blocks[0] != want {
		t.Errorf("child block = %q, want %q", ap.blocks[0], want)
	}
	if !strings.HasPrefix(ap.blocks[1], sid+"|"+sid+"|mcp__srv__read|mcp__*(any path outside workspace)|") {
		t.Errorf("path block = %q", ap.blocks[1])
	}
	if strings.HasSuffix(ap.blocks[1], "|") {
		t.Errorf("path block has no detail: %q", ap.blocks[1])
	}
	if ap.count() != 0 {
		t.Error("a deny must never prompt")
	}
}
