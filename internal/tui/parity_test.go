package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

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
	run(m, tickMsg{})
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
