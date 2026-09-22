package tui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/provider"
)

var t0 = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)

func fixedNow() time.Time { return t0 }

func wev(session, typ string) daemon.WireEvent { return daemon.WireEvent{Session: session, Type: typ} }

func kinds(sv *sessionView) []blockKind {
	var out []blockKind
	for _, b := range sv.blocks {
		out = append(out, b.kind)
	}
	return out
}

func sameKinds(a, b []blockKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAFullTurnBecomesBlocksInOrder(t *testing.T) {
	c := newCache(fixedNow)
	for _, ev := range []daemon.WireEvent{
		wev("s", daemon.WireTurnStarted),
		{Session: "s", Type: daemon.WireText, Text: "Hel"},
		{Session: "s", Type: daemon.WireText, Text: "lo"},
		{Session: "s", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash", Args: `{"cmd":"ls"}`},
		{Session: "s", Type: daemon.WireToolResult, ToolUseID: "t1", Content: "ok"},
		{Session: "s", Type: daemon.WireText, Text: "done"},
		{Session: "s", Type: daemon.WireTurnDone, Model: "m", TokensIn: 5, TokensCacheRead: 100, TokensCacheWrite: 20, TokensOut: 3},
	} {
		c.Apply(ev)
	}
	sv := c.get("s")
	if want := []blockKind{kindText, kindTool, kindText, kindFooter}; !sameKinds(kinds(sv), want) {
		t.Fatalf("kinds = %v, want %v", kinds(sv), want)
	}
	if sv.blocks[0].text != "Hello" || sv.blocks[0].streaming {
		t.Errorf("first text = %+v, want Hello, no longer streaming", sv.blocks[0])
	}
	if !sv.blocks[1].done || sv.blocks[1].result != "ok" {
		t.Errorf("tool = %+v, want done with result ok", sv.blocks[1])
	}
	if sv.working || c.State("s") != daemon.SessionIdle {
		t.Errorf("after turn_done working=%v state=%s, want idle", sv.working, c.State("s"))
	}
	if sv.ctxTokens != 125 {
		t.Errorf("ctxTokens = %d, want 125 (uncached + cache read + cache write)", sv.ctxTokens)
	}
}

func TestStoppedEndsTheTurnAndDropsOnlyItsOwnApprovals(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(wev("s", daemon.WireTurnStarted))
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireText, Text: "par"})
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireApproval, PendingID: 1, Tool: "shell"})
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireApproval, PendingID: 2, Tool: "shell", Origin: "kid"})
	c.Apply(wev("s", daemon.WireStopped))

	sv := c.get("s")
	if sv.working {
		t.Error("still working after stopped")
	}
	if len(sv.approvals) != 1 || sv.approvals[0].PendingID != 2 {
		t.Errorf("approvals = %+v, want only the spawned child's", sv.approvals)
	}
	if sv.blocks[0].streaming {
		t.Error("the partial text is still streaming")
	}
	if last := sv.last(); last.kind != kindNotice || last.text != "stopped" {
		t.Errorf("last block = %+v, want a stopped notice", last)
	}
}

func TestResolvedElsewhereIsReportedButNotWhenAnsweredHere(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireApproval, PendingID: 1, Tool: "shell"})
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireApproval, PendingID: 2, Tool: "fs.write"})
	c.markAnswered("s", 1)
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireResolved, PendingID: 1, Tool: "shell", Decision: "allow"})
	if n := len(c.get("s").blocks); n != 0 {
		t.Fatalf("an approval answered here produced %d blocks", n)
	}
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireResolved, PendingID: 2, Tool: "fs.write", Decision: "deny"})
	sv := c.get("s")
	if len(sv.approvals) != 0 {
		t.Errorf("approvals = %+v, want none", sv.approvals)
	}
	if last := sv.last(); last == nil || !strings.Contains(last.text, "answered elsewhere") {
		t.Errorf("last block = %+v, want an answered-elsewhere notice", last)
	}
}

func TestAChildsApprovalBlocksItAndIsAnsweredThroughTheRoot(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "kid", Type: daemon.WireSession, ParentID: "root", Source: "subagent"})
	c.Apply(daemon.WireEvent{Session: "root", Type: daemon.WireApproval, PendingID: 7, Tool: "shell", Origin: "kid"})
	if c.State("kid") != daemon.SessionBlocked || c.State("root") != daemon.SessionBlocked {
		t.Fatalf("states kid=%s root=%s, want both blocked", c.State("kid"), c.State("root"))
	}
	root, ev, ok := c.approvalFor("kid")
	if !ok || root != "root" || ev.PendingID != 7 {
		t.Fatalf("approvalFor(kid) = %s %+v %v, want root/7", root, ev, ok)
	}
	c.Apply(daemon.WireEvent{Session: "kid", Type: daemon.WireAgentState, State: "interrupted"})
	if c.State("kid") != daemon.SessionIdle || c.BlockedCount() != 0 {
		t.Fatalf("after the child settled: state=%s blocked=%d, want idle and 0", c.State("kid"), c.BlockedCount())
	}
}

func TestEventsForAnotherSessionStayThere(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "a", Type: daemon.WireText, Text: "for a"})
	c.Apply(daemon.WireEvent{Session: "b", Type: daemon.WireText, Text: "for b"})
	if c.get("a").blocks[0].text != "for a" || c.get("b").blocks[0].text != "for b" {
		t.Fatal("events crossed sessions")
	}
}

func TestResyncReplacesTheTranscript(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireText, Text: "live"})
	c.SetTranscript(daemon.TranscriptJSON{
		Session: daemon.SessionJSON{ID: "s", Title: "t"},
		Messages: []daemon.MessageJSON{
			{Role: "user", Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}},
			{Role: "assistant", Model: "m", TokensIn: 10, TokensCacheRead: 90, Blocks: []provider.Block{
				{Type: provider.BlockText, Text: "persisted"},
				{Type: provider.BlockToolUse, ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"a"}`)},
			}},
			{Role: "tool", Blocks: []provider.Block{{Type: provider.BlockToolResult, ID: "t1", Content: "file"}}},
		},
	})
	sv := c.get("s")
	if want := []blockKind{kindUser, kindText, kindTool}; !sameKinds(kinds(sv), want) {
		t.Fatalf("kinds = %v, want %v", kinds(sv), want)
	}
	if !sv.loaded || !sv.blocks[2].done || sv.blocks[2].result != "file" {
		t.Fatalf("loaded=%v tool=%+v", sv.loaded, sv.blocks[2])
	}
	if sv.model != "m" || sv.ctxTokens != 100 {
		t.Fatalf("model=%q ctx=%d, want m and 100", sv.model, sv.ctxTokens)
	}
}

func TestASessionEventCreatesTheRow(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "n", Type: daemon.WireSession, Title: "new", Source: "chat", Workspace: "/w"})
	info := c.get("n").info
	if info.Title != "new" || info.Source != "chat" || info.Workspace != "/w" || info.ID != "n" {
		t.Fatalf("info = %+v", info)
	}
}

func TestClearApprovalsEmptiesEverySession(t *testing.T) {
	c := newCache(fixedNow)
	c.Apply(daemon.WireEvent{Session: "a", Type: daemon.WireApproval, PendingID: 1})
	c.Apply(daemon.WireEvent{Session: "b", Type: daemon.WireApproval, PendingID: 2})
	c.ClearApprovals()
	if c.BlockedCount() != 0 {
		t.Fatalf("BlockedCount = %d after ClearApprovals", c.BlockedCount())
	}
}
