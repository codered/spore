package discord

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func TestCustomIDRoundTrip(t *testing.T) {
	cases := []policy.Answer{
		{Allow: true, Scope: policy.ScopeOnce},
		{Allow: false, Scope: policy.ScopeOnce},
		{Allow: true, Scope: policy.ScopeSession},
		{Allow: true, Scope: policy.ScopePattern},
	}
	for _, want := range cases {
		id := encodeCustomID("sess-1", 42, want.Allow, want.Scope)
		if len(id) > 100 {
			t.Fatalf("custom id is %d characters; Discord's limit is 100", len(id))
		}
		sid, pid, got, err := decodeCustomID(id)
		if err != nil {
			t.Fatal(err)
		}
		if sid != "sess-1" || pid != 42 || got != want {
			t.Fatalf("round trip: (%q, %d, %+v), want (sess-1, 42, %+v)", sid, pid, got, want)
		}
	}
}

func TestDecodeCustomIDRejectsGarbage(t *testing.T) {
	// The custom id arrives from Discord and is therefore untrusted input.
	for _, bad := range []string{"", "nonsense", "a|b|c", "spore|sess|notanumber|allow|once", "other|sess|1|allow|once"} {
		if _, _, _, err := decodeCustomID(bad); err == nil {
			t.Fatalf("decodeCustomID(%q) succeeded, want an error", bad)
		}
	}
}

func TestApprovalMessageOffersThePatternOptionOnlyWhenThereIsOne(t *testing.T) {
	withPattern := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 7, Tool: "fs_write",
		Args: `{"path":"/w/a.go"}`, Rule: "fs_write",
		Pattern: "fs_write(path matches /w/**)",
	})
	if len(withPattern.Buttons) != 4 {
		t.Fatalf("got %d buttons, want 4 (once, deny, session, pattern)", len(withPattern.Buttons))
	}

	// The event the guard sends for a call with no derivable pattern. The
	// button must be absent, not merely relabelled: this is a one-tap control
	// on a phone that would otherwise write a blanket allow for the tool.
	degraded := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 8, Tool: "shell_exec",
		Args: `{"cmd":"ls"}`, Rule: "shell_exec", Pattern: "",
	})
	if len(degraded.Buttons) != 3 {
		t.Fatalf("got %d buttons, want 3 (once, deny, session)", len(degraded.Buttons))
	}
	for _, b := range degraded.Buttons {
		if _, _, ans, err := decodeCustomID(b.CustomID); err == nil && ans.Scope == policy.ScopePattern {
			t.Fatalf("a pattern button was offered for a call with no pattern: %+v", b)
		}
	}
}

func TestApprovalMessageLabelsTheSessionScopeHonestly(t *testing.T) {
	// "session" approves the TOOL for the rest of the session, not these
	// arguments. A vaguer label would understate what the tap does.
	m := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 7, Tool: "shell_exec", Rule: "shell_exec",
	})
	found := false
	for _, b := range m.Buttons {
		if strings.Contains(b.Label, "shell_exec") && strings.Contains(b.Label, "session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no button names the tool and the session scope: %+v", m.Buttons)
	}
}

func TestApprovalMessageMarksDenyAsDanger(t *testing.T) {
	m := approvalMessage("s1", daemon.WireEvent{Type: daemon.WireApproval, PendingID: 7, Tool: "fs_write"})
	for _, b := range m.Buttons {
		if _, _, ans, err := decodeCustomID(b.CustomID); err == nil && !ans.Allow {
			if !b.Danger {
				t.Fatal("the deny button is not marked as danger")
			}
			return
		}
	}
	t.Fatal("no deny button")
}

func TestAnswererRefusesAnotherSessionsApproval(t *testing.T) {
	ctx := context.Background()
	// config.Load, never config.Default: Load is what appends the baseline
	// deny rules, and a guard without them proves nothing.
	st, guard := newLoadedGuard(t)
	victim, _ := st.CreateSession(ctx, "victim")
	attacker, _ := st.CreateSession(ctx, "attacker")

	pendingID, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: victim, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "local", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	a := newAnswerer(daemon.NewBroker(daemon.NewHub()), guard)

	// The attacker's session forges the victim's pending id.
	if _, err := a.answer(ctx, attacker, pendingID, policy.Answer{Allow: true, Scope: policy.ScopeOnce}); err == nil {
		t.Fatal("one session answered another session's approval")
	}
	// And the suspension is still open for its real owner.
	pending, err := guard.Pending(ctx, victim)
	if err != nil || len(pending) != 1 {
		t.Fatalf("the victim's suspension was consumed: %d pending, err %v", len(pending), err)
	}
}
