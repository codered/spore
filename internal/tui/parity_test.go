package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// clockModel is a test model whose clock the test moves by hand. It records
// how often the one-second tick is armed instead of sleeping.
func clockModel(t *testing.T) (*Model, *time.Time, *int) {
	t.Helper()
	m := newTestModel(t, &fakeBackend{}, "s1")
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	armed := 0
	m.secondTick = func() tea.Cmd { armed++; return nil }
	return m, &now, &armed
}

func thinking(m *Model) string { return ansi.Strip(m.thinkingView()) }

func TestThinkingRowFollowsTheTurn(t *testing.T) {
	m, now, _ := clockModel(t)
	if got := thinking(m); got != "" {
		t.Fatalf("idle session shows %q", got)
	}
	run(m, keyMsg("h"))
	run(m, keyMsg("i"))
	run(m, keyMsg("enter"))
	if got := thinking(m); got != "● spore is thinking…" {
		t.Fatalf("after send: %q", got)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(12 * time.Second)
	run(m, secondTickMsg{})
	if got := thinking(m); got != "● spore is thinking… 12s" {
		t.Fatalf("after 12s: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "hel"})
	if got := thinking(m); !strings.HasPrefix(got, "● spore is writing…") {
		t.Fatalf("while text streams: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	if got := thinking(m); !strings.HasPrefix(got, "● spore is running bash…") {
		t.Fatalf("while a tool waits: %q", got)
	}
	*now = now.Add(63 * time.Second)
	if got := thinking(m); !strings.HasSuffix(got, " 1m15s") {
		t.Fatalf("elapsed past a minute: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "bash"})
	if got := thinking(m); got != "" {
		t.Fatalf("while blocked: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireResolved, PendingID: 1, Decision: "allow"})
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolResult, ToolUseID: "t1", Content: "ok"})
	if got := thinking(m); !strings.HasPrefix(got, "● spore is thinking…") {
		t.Fatalf("after the tool returns: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.View()), "spore is thinking…") {
		t.Fatal("the row is not on screen")
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	if got := thinking(m); got != "" {
		t.Fatalf("after the turn: %q", got)
	}
}

func TestANewSendDoesNotShowTheLastTurnsTime(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(30 * time.Second)
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, keyMsg("i"))
	run(m, keyMsg("x"))
	run(m, keyMsg("enter"))
	if got := thinking(m); got != "● spore is thinking…" {
		t.Fatalf("second send: %q", got)
	}
}

func approvalText(m *Model) string { return ansi.Strip(m.overlayView()) }

func TestApprovalShowsProfileAndCountdown(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell",
		Rule: "shell.ask", Profile: "local", ExpiresAt: t0.Add(2*time.Minute + 5*time.Second).Format(time.RFC3339)})
	got := approvalText(m)
	for _, want := range []string{`profile local · matched policy rule "shell.ask"`, "auto-denies in 2:05"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	*now = now.Add(2 * time.Minute)
	run(m, secondTickMsg{})
	if got := approvalText(m); !strings.Contains(got, "auto-denies in 0:05") {
		t.Fatalf("after 2m:\n%s", got)
	}
	*now = now.Add(10 * time.Second)
	if got := approvalText(m); !strings.Contains(got, "auto-denying…") {
		t.Fatalf("past the deadline:\n%s", got)
	}
}

func TestApprovalWithoutADeadlineSaysSo(t *testing.T) {
	m, _, _ := clockModel(t)
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell", Rule: "shell.ask"})
	got := approvalText(m)
	if !strings.Contains(got, "waiting (no timeout)") {
		t.Fatalf("no deadline:\n%s", got)
	}
	if strings.Contains(got, "profile") {
		t.Fatalf("empty profile shown:\n%s", got)
	}
}

func TestTheSecondTickRunsOnlyWhileSomethingCounts(t *testing.T) {
	m, _, armed := clockModel(t)
	if *armed != 0 {
		t.Fatalf("idle model armed the tick %d times", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "x"})
	if *armed != 1 {
		t.Fatalf("armed %d times while one tick is pending, want 1", *armed)
	}
	run(m, secondTickMsg{})
	if *armed != 2 {
		t.Fatalf("tick did not re-arm while working: %d", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, secondTickMsg{})
	if *armed != 2 {
		t.Fatalf("tick re-armed after the turn: %d", *armed)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell"})
	if *armed != 3 {
		t.Fatalf("an approval on screen did not arm the tick: %d", *armed)
	}
}
