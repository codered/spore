package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

func twoSessions(t *testing.T) (*fakeBackend, *Model) {
	t.Helper()
	fb := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "aaaa", Title: "first", Source: "chat", Workspace: "/w", UpdatedAt: t0},
		{ID: "bbbb", Title: "second", Source: "chat", Workspace: "/w", UpdatedAt: t0.Add(-1)},
	}}
	m := newTestModel(t, fb, "aaaa")
	run(m, sessionsMsg{list: fb.sessions})
	return fb, m
}

// bodyTop is the first row under the header: the top borders of the panes.
func bodyTop(m *Model) string { return ansi.Strip(strings.Split(m.View(), "\n")[1]) }

func TestTabMovesFocusBetweenTheSidebarAndTheChat(t *testing.T) {
	_, m := twoSessions(t)
	if m.focused() != paneChat {
		t.Fatal("spore chat opens to type, so the chat pane starts focused")
	}
	press(m, "esc")
	if m.focused() != paneChat {
		t.Fatal("leaving INSERT moved the focus off the chat")
	}
	press(m, "tab")
	if m.focused() != paneSidebar {
		t.Fatal("tab did not focus the sidebar")
	}
	run(m, tea.KeyMsg{Type: tea.KeyShiftTab})
	if m.focused() != paneChat {
		t.Fatal("shift+tab did not focus the chat")
	}
	press(m, "tab", "i")
	if m.focused() != paneChat || m.mode != modeInsert {
		t.Fatal("i from the sidebar must type into the chat")
	}
}

func TestTabDoesNothingWithoutASidebar(t *testing.T) {
	_, m := twoSessions(t)
	press(m, "esc", "tab", "ctrl+b")
	if m.focused() != paneChat {
		t.Fatal("hiding the sidebar left the focus on it")
	}
	press(m, "tab")
	if m.focused() != paneChat {
		t.Fatal("tab focused a hidden sidebar")
	}
}

func TestJScrollsAFocusedChatAndMovesAFocusedSidebar(t *testing.T) {
	_, m := twoSessions(t)
	sv := m.cache.get("aaaa")
	for i := range 80 {
		sv.add(kindNotice, fmt.Sprintf("line %d", i))
	}
	run(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	press(m, "esc", "g", "j")
	if m.selected != "aaaa" {
		t.Fatalf("j in the chat changed the session to %q", m.selected)
	}
	if m.vp.YOffset != 1 {
		t.Fatalf("j in the chat scrolled to %d, want 1", m.vp.YOffset)
	}
	press(m, "k")
	if m.vp.YOffset != 0 {
		t.Fatalf("k in the chat scrolled to %d, want 0", m.vp.YOffset)
	}
	press(m, "tab", "j")
	if m.selected != "bbbb" {
		t.Fatalf("j in the sidebar selected %q, want bbbb", m.selected)
	}
}

func TestTheFocusedPaneHasTheHeavyBorder(t *testing.T) {
	_, m := twoSessions(t)
	top := bodyTop(m)
	if !strings.HasPrefix(top, "╭") || !strings.Contains(top, "┏") {
		t.Fatalf("chat focused, but the borders are %q", top)
	}
	press(m, "esc", "tab")
	top = bodyTop(m)
	if !strings.HasPrefix(top, "┏") || strings.Count(top, "┏") != 1 {
		t.Fatalf("sidebar focused, but the borders are %q", top)
	}
}

func TestTheHeaderIsOneRowOfTabs(t *testing.T) {
	fb := &fakeBackend{jobs: []daemon.JobJSON{{ID: 3}}}
	m := newTestModel(t, fb, "s1")
	head := ansi.Strip(strings.Split(m.View(), "\n")[0])
	for _, want := range []string{"spore", "chat", "skills", "agents", "usage", "jobs"} {
		if !strings.Contains(head, want) {
			t.Errorf("header %q is missing %q", head, want)
		}
	}
	if m.bodyHeight() != m.height-2 {
		t.Fatalf("body is %d rows of %d; header and status bar should take one each", m.bodyHeight(), m.height)
	}
	if m.activeTab() != "chat" {
		t.Fatalf("active tab = %q, want chat", m.activeTab())
	}
	press(m, "esc", "S")
	if m.activeTab() != "skills" {
		t.Fatalf("active tab = %q, want skills", m.activeTab())
	}
	m.openView(jobRunsRes{job: 3})
	if m.activeTab() != "jobs" {
		t.Fatalf("a job's runs lit %q, want jobs", m.activeTab())
	}
}

func TestTheChatPaneNamesTheSessionAndItsFacts(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1abcd")
	run(m, sessionsMsg{list: []daemon.SessionJSON{{ID: "s1abcd", Title: "fix it", Source: "discord", Workspace: "/work/x"}}})
	feed(m, daemon.WireEvent{Session: "s1abcd", Type: daemon.WireTurnDone, Model: "sonnet-5", TokensIn: 1000})
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	if strings.Contains(lines[0], "sonnet-5") || strings.Contains(lines[0], "fix it") {
		t.Fatalf("the top nav still carries the session: %q", lines[0])
	}
	if !strings.Contains(lines[1], "s1ab") || !strings.Contains(lines[1], "fix it") {
		t.Fatalf("the chat pane's border does not name the session: %q", lines[1])
	}
	for _, want := range []string{"discord", "/work/x", "sonnet-5", "ctx 1.0k"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("the chat pane's first line %q is missing %q", lines[2], want)
		}
	}
}

func approvalModel(t *testing.T) (*fakeBackend, *Model) {
	t.Helper()
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "s1")
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 7, Tool: "shell", Rule: "shell.ask",
		Pattern: "shell:go *", Args: `{}`})
	press(m, "esc")
	return fb, m
}

func TestAnApprovalKeyAsksBeforeAnswering(t *testing.T) {
	cases := map[string]string{
		"y": "Allow shell once?",
		"n": "Deny shell?",
		"s": "Allow shell for this session?",
		"p": "Always allow shell:go *?",
	}
	for key, question := range cases {
		fb, m := approvalModel(t)
		press(m, key)
		if len(fb.resolved) != 0 {
			t.Fatalf("[%s] answered before confirming: %v", key, fb.resolved)
		}
		if m.mode != modeConfirm || !strings.Contains(m.View(), question) {
			t.Fatalf("[%s] no modal asking %q", key, question)
		}
		press(m, "y")
		if len(fb.resolved) != 1 {
			t.Fatalf("[%s] confirming resolved %v", key, fb.resolved)
		}
		for _, b := range m.cache.get("s1").blocks {
			if b.kind == kindNotice {
				t.Fatalf("[%s] the answer went into the chat: %q", key, b.text)
			}
		}
		if !strings.Contains(m.View(), "shell") || m.flash == "" {
			t.Fatalf("[%s] the status bar does not report the answer", key)
		}
	}
}

func TestEscInTheModalLeavesTheApprovalUnanswered(t *testing.T) {
	fb, m := approvalModel(t)
	press(m, "y", "esc")
	if len(fb.resolved) != 0 || m.mode != modeNormal {
		t.Fatalf("esc resolved %v, mode %s", fb.resolved, m.mode)
	}
	if _, _, ok := m.cache.approvalFor("s1"); !ok {
		t.Fatal("cancelling the modal dropped the approval")
	}
}

func TestConfirmingAnApprovalAnsweredElsewhereSendsNothing(t *testing.T) {
	fb, m := approvalModel(t)
	press(m, "y")
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireResolved, PendingID: 7})
	press(m, "y")
	if len(fb.resolved) != 0 {
		t.Fatalf("resolved a stale approval: %v", fb.resolved)
	}
}

func TestTheScreenFitsTheTerminalWithTheModalOpen(t *testing.T) {
	for _, w := range []int{60, 80, 100, 120, 160} {
		_, m := approvalModel(t)
		run(m, tea.WindowSizeMsg{Width: w, Height: 30})
		press(m, "p")
		lines := strings.Split(m.View(), "\n")
		if len(lines) != 30 {
			t.Errorf("[%d] %d rows, want 30", w, len(lines))
		}
		for i, l := range lines {
			if lipgloss.Width(l) > w {
				t.Errorf("[%d] row %d is %d cells: %q", w, i, lipgloss.Width(l), ansi.Strip(l))
			}
		}
		if !strings.Contains(ansi.Strip(m.View()), "Always allow") {
			t.Errorf("[%d] the modal is not on screen", w)
		}
	}
}
