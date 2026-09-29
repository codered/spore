package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

func card(m *Model) string { return ansi.Strip(m.approvalCard(100, 40)) }

func approvalOn(m *Model, ev daemon.WireEvent) {
	ev.Session, ev.Type = "s1", daemon.WireApproval
	if ev.PendingID == 0 {
		ev.PendingID = 1
	}
	feed(m, ev)
}

func TestApprovalCardShowsProfileRuleCallAndCountdown(t *testing.T) {
	m, now, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Profile: "local",
		Args: `{"command":"go test ./..."}`, ExpiresAt: t0.Add(2*time.Minute + 5*time.Second).Format(time.RFC3339)})
	got := card(m)
	for _, want := range []string{"spore wants to run shell_exec", "profile local", "matched ask  shell_exec",
		"$ go test ./...", "y allow once", "n deny", "s allow shell_exec this session", "auto-deny in 2:05"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	*now = now.Add(2 * time.Minute)
	run(m, tickMsg{})
	if got := card(m); !strings.Contains(got, "auto-deny in 0:05") {
		t.Fatalf("after 2m:\n%s", got)
	}
	*now = now.Add(10 * time.Second)
	if got := card(m); !strings.Contains(got, "auto-denying…") {
		t.Fatalf("past the deadline:\n%s", got)
	}
}

func TestApprovalCardWithoutDeadlineOrProfile(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec"})
	got := card(m)
	if !strings.Contains(got, "waiting (no timeout)") {
		t.Fatalf("no deadline:\n%s", got)
	}
	if strings.Contains(got, "profile") || strings.Contains(got, "from sub-agent") {
		t.Fatalf("empty origin row drawn:\n%s", got)
	}
}

func TestApprovalCardNamesTheSubAgentAndPattern(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Origin: "7f3e99", Profile: "remote",
		Pattern: "shell_exec(command matches go test*)"})
	got := card(m)
	for _, want := range []string{"from sub-agent 7f3e · profile remote", "p always allow shell_exec(command matches go test*)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDefaultRuleReadsAsNoRuleMatched(t *testing.T) {
	if got := ansi.Strip(ruleLine("policy.default")); got != "no rule matched · default is ask" {
		t.Fatalf("ruleLine = %q", got)
	}
}

func boxText(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSpace(ansi.Strip(l))
	}
	return out
}

func TestShellCallBox(t *testing.T) {
	got := boxText(callBox("shell_exec", `{"command":"go test","timeout_seconds":30}`, 8, 40))
	if strings.Join(got, "|") != "$ go test|timeout 30s" {
		t.Fatalf("with timeout: %q", got)
	}
	got = boxText(callBox("shell_exec", `{"command":"go test"}`, 8, 40))
	if strings.Join(got, "|") != "$ go test" {
		t.Fatalf("without timeout: %q", got)
	}
	for _, l := range callBox("shell_exec", `{"command":"go test"}`, 8, 40) {
		if ansi.StringWidth(l) != 40 {
			t.Fatalf("box row is %d wide, want 40", ansi.StringWidth(l))
		}
	}
}

func TestOtherToolsShowTheirArgs(t *testing.T) {
	got := boxText(callBox("fs_write", `{"path":"a"}`, 8, 40))
	if strings.Join(got, "|") != `{|"path": "a"|}` {
		t.Fatalf("args: %q", got)
	}
}

func longCommand() string {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("echo %d", i))
	}
	return strings.Join(lines, `\n`)
}

func TestCallBoxClipsToItsRows(t *testing.T) {
	got := boxText(callBox("shell_exec", `{"command":"`+longCommand()+`"}`, 8, 40))
	if len(got) != 8 || got[7] != "… 13 more lines" {
		t.Fatalf("clipped box: %q", got)
	}
}

func TestCardShrinksItsCallBoxBeforeClipping(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Args: `{"command":"` + longCommand() + `"}`})
	full := lipgloss.Height(m.approvalCard(100, 40))
	if got := lipgloss.Height(m.approvalCard(100, full-4)); got != full-4 {
		t.Fatalf("height %d, want %d", got, full-4)
	}
	if got := lipgloss.Height(m.approvalCard(100, 5)); got != full-7 {
		t.Fatalf("squeezed height %d, want %d (box down to one row)", got, full-7)
	}
}

func TestCardFitsANarrowPane(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Profile: "local",
		Pattern: "shell_exec(command matches go test -race -count=50 ./internal/tui/...)",
		Args:    `{"command":"go test -race -count=50 -run 'TestA|TestB|TestC' ./internal/tui/... ./cmd/spore/..."}`})
	for _, l := range strings.Split(m.approvalCard(56, 20), "\n") {
		if w := ansi.StringWidth(l); w > 56 {
			t.Fatalf("card row is %d wide in a 56-column pane: %q", w, ansi.Strip(l))
		}
	}
}

func TestDraftLineUnderTheInput(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec"})
	if v := ansi.Strip(m.View()); !strings.Contains(v, "approval keys work in NORMAL — esc, then y/n/s/p") || strings.Contains(v, "draft kept") {
		t.Fatalf("empty input:\n%s", v)
	}
	run(m, keyMsg("h"))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "draft kept · approval keys work in NORMAL — esc, then y/n/s/p") {
		t.Fatalf("with a draft:\n%s", v)
	}
	press(m, "esc")
	if v := ansi.Strip(m.View()); strings.Contains(v, "approval keys work in NORMAL") {
		t.Fatalf("still shown in NORMAL:\n%s", v)
	}
}
