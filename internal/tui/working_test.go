package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// clockModel is a test model whose clock the test moves by hand. It records
// the interval of every tick it arms instead of sleeping.
func clockModel(t *testing.T) (*Model, *time.Time, *[]time.Duration) {
	t.Helper()
	m := newTestModel(t, &fakeBackend{}, "s1")
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	var armed []time.Duration
	m.tick = func(d time.Duration) tea.Cmd { armed = append(armed, d); return nil }
	return m, &now, &armed
}

// working is the working line without its spinner frame, which turns with
// the clock.
func working(m *Model) string {
	s := []rune(ansi.Strip(m.workingLine()))
	if len(s) < 2 {
		return string(s)
	}
	return string(s[2:])
}

func TestWorkingLineFollowsTheTurn(t *testing.T) {
	m, now, _ := clockModel(t)
	if got := working(m); got != "" {
		t.Fatalf("idle session shows %q", got)
	}
	run(m, keyMsg("h"))
	run(m, keyMsg("i"))
	run(m, keyMsg("enter"))
	if got := working(m); got != "thinking" {
		t.Fatalf("after send: %q", got)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(12 * time.Second)
	run(m, tickMsg{})
	if got := working(m); got != "thinking · 12s" {
		t.Fatalf("after 12s: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "hel"})
	if got := working(m); got != "writing · 12s" {
		t.Fatalf("while text streams: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	if got := working(m); got != "running bash · 12s" {
		t.Fatalf("while a tool waits: %q", got)
	}
	*now = now.Add(63 * time.Second)
	run(m, tickMsg{})
	if got := working(m); got != "running bash · 1m15s" {
		t.Fatalf("past a minute: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.lastContent), "running bash · 1m15s") {
		t.Fatal("the working line is not in the transcript")
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "bash"})
	if got := working(m); got != "" {
		t.Fatalf("while blocked: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireResolved, PendingID: 1, Decision: "allow"})
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolResult, ToolUseID: "t1", Content: "ok"})
	if got := working(m); got != "thinking · 1m15s" {
		t.Fatalf("after the tool returns: %q", got)
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	if got := working(m); got != "" {
		t.Fatalf("after the turn: %q", got)
	}
	if strings.Contains(ansi.Strip(m.lastContent), "thinking") {
		t.Fatal("the working line outlived the turn")
	}
}

func TestANewSendDoesNotShowTheLastTurnsTime(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(30 * time.Second)
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, keyMsg("x"))
	run(m, keyMsg("enter"))
	if got := working(m); got != "thinking" {
		t.Fatalf("second send: %q", got)
	}
}

func TestStoppedMidToolClearsTheWorkingLine(t *testing.T) {
	m, _, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	feed(m, wev("s1", daemon.WireStopped))
	if got := working(m); got != "" {
		t.Fatalf("after stop: %q", got)
	}
	row := ansi.Strip(m.cache.get("s1").tool("t1").drawTool(60, false))
	if !strings.HasSuffix(row, "…") {
		t.Fatalf("orphaned tool row = %q", row)
	}
}

func TestSpinnerTurnsWithTheClock(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	a := []rune(ansi.Strip(m.workingLine()))[0]
	*now = now.Add(frameEvery)
	b := []rune(ansi.Strip(m.workingLine()))[0]
	if a == b {
		t.Fatalf("frame %q did not turn", string(a))
	}
}

func TestAFrameIsNotNewOutput(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	m.follow, m.unseen = false, false
	*now = now.Add(frameEvery)
	run(m, tickMsg{})
	if m.unseen {
		t.Fatal("a spinner frame lit ↓ new")
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "real output"})
	if !m.unseen {
		t.Fatal("real output did not light ↓ new")
	}
}

func TestTickIntervalFollowsWhatIsOnScreen(t *testing.T) {
	m, _, armed := clockModel(t)
	if len(*armed) != 0 {
		t.Fatalf("idle model armed %v", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "x"})
	if len(*armed) != 1 || (*armed)[0] != frameEvery {
		t.Fatalf("while working: %v, want one %v", *armed, frameEvery)
	}
	run(m, tickMsg{})
	if len(*armed) != 2 {
		t.Fatalf("tick did not re-arm while working: %v", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, tickMsg{})
	if len(*armed) != 2 {
		t.Fatalf("tick re-armed after the turn: %v", *armed)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell"})
	if len(*armed) != 3 || (*armed)[2] != secondEvery {
		t.Fatalf("approval only: %v, want a %v tick", *armed, secondEvery)
	}
}

func BenchmarkSyncLongTranscript(b *testing.B) {
	m := New(context.Background(), &fakeBackend{}, "s1", Options{})
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	m.tick = func(time.Duration) tea.Cmd { return nil }
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	sv := m.cache.get("s1")
	for i := 0; i < 1000; i++ {
		sv.blocks = append(sv.blocks,
			&block{kind: kindText, text: fmt.Sprintf("paragraph %d with **some** markdown and `code`", i)},
			&block{kind: kindTool, toolID: fmt.Sprint(i), tool: "bash", args: `{"command":"go test ./..."}`, result: "ok", done: true})
	}
	sv.working, sv.started = true, t0
	m.sync() // renders every block once; later frames hit each block's cache
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(frameEvery)
		m.sync()
	}
}
