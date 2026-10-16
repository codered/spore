package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/codered/spore/internal/daemon"
)

var grantChoices = []string{"5m", "15m", "30m", "1h"}

// newGrantModel is a model whose daemon offers grantChoices, with the list
// loaded as connect loads it.
func newGrantModel(t *testing.T, fb *fakeBackend, selected string) *Model {
	t.Helper()
	fb.grantDurations = grantChoices
	m := newTestModel(t, fb, selected)
	run(m, m.loadGrantDurations()())
	return m
}

// altRune presses alt with a letter, as a terminal sends it.
func altRune(m *Model, r rune) {
	run(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}, Alt: true})
}

func grantApprovalOn(m *Model, session string) {
	feed(m, daemon.WireEvent{Session: session, Type: daemon.WireApproval, PendingID: 3, Tool: "shell_exec"})
}

func TestCardShowsGrantRow(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	if !strings.Contains(m.View(), "allow all for…") {
		t.Fatal("the card does not offer the grant")
	}

	off := &fakeBackend{}
	m = newTestModel(t, off, "s1")
	grantApprovalOn(m, "s1")
	press(m, "alt+esc", "a")
	if strings.Contains(m.View(), "allow all for…") {
		t.Fatal("the card offers a grant with no durations configured")
	}
	if m.mode != modeNormal || m.input.Value() != "" || m.grantPick != nil {
		t.Fatalf("a with no durations: mode=%s input=%q picker=%v, want NORMAL and nothing", m.mode, m.input.Value(), m.grantPick)
	}
}

func TestGrantPickerByNumber(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	press(m, "alt+esc", "a")
	for _, d := range grantChoices {
		if !strings.Contains(m.View(), d) {
			t.Fatalf("the picker does not list %s", d)
		}
	}
	press(m, "2")
	if len(fb.grants) != 1 || fb.grants[0] != "s1:15m" {
		t.Fatalf("grants = %v, want [s1:15m]", fb.grants)
	}
	if m.grantPick != nil || m.mode != modeNormal || m.confirm != nil {
		t.Fatalf("after picking: picker=%v mode=%s confirm=%v, want closed in NORMAL with no modal", m.grantPick, m.mode, m.confirm)
	}
}

func TestGrantPickerJKEnter(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	press(m, "alt+esc", "a", "j", "j", "enter")
	if len(fb.grants) != 1 || fb.grants[0] != "s1:30m" {
		t.Fatalf("grants = %v, want [s1:30m]", fb.grants)
	}
}

func TestGrantPickerEscSendsNothing(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	press(m, "alt+esc", "a", "esc")
	if len(fb.grants) != 0 {
		t.Fatalf("grants = %v after esc, want none", fb.grants)
	}
	if _, _, ok := m.cache.approvalFor("s1"); !ok || !strings.Contains(m.View(), "allow all for…") {
		t.Fatal("the card is gone after esc")
	}
}

func TestGrantAltAInInsert(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	typeText(m, "draft")
	altRune(m, 'a')
	if m.mode != modeGrant {
		t.Fatalf("mode = %s after alt+a, want the picker", m.mode)
	}
	press(m, "2")
	if m.mode != modeInsert || m.input.Value() != "draft" {
		t.Fatalf("after picking: mode=%s draft=%q, want INSERT with the draft kept", m.mode, m.input.Value())
	}
	if len(fb.grants) != 1 || fb.grants[0] != "s1:15m" {
		t.Fatalf("grants = %v, want [s1:15m]", fb.grants)
	}
}

func TestGrantCommand(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	press(m, "alt+esc", ":")
	typeText(m, "grant 1h")
	press(m, "enter")
	if len(fb.grants) != 1 || fb.grants[0] != "s1:1h" {
		t.Fatalf("grants = %v, want [s1:1h]", fb.grants)
	}

	// A sub-agent's grant goes to its root, the session that holds the approval.
	fb = &fakeBackend{}
	m = newGrantModel(t, fb, "kid")
	feed(m, daemon.WireEvent{Session: "kid", Type: daemon.WireSession, Title: "kid", Workspace: "/work", ParentID: "root", Source: "subagent"})
	press(m, "alt+esc", ":")
	typeText(m, "grant 1h")
	press(m, "enter")
	if len(fb.grants) != 1 || fb.grants[0] != "root:1h" {
		t.Fatalf("grants from a sub-agent = %v, want [root:1h]", fb.grants)
	}

	press(m, ":")
	typeText(m, "grant off")
	press(m, "enter")
	if len(fb.grantEnds) != 1 || fb.grantEnds[0] != "root" {
		t.Fatalf("ends = %v, want [root]", fb.grantEnds)
	}

	press(m, ":")
	typeText(m, "grant")
	press(m, "enter")
	if m.mode != modeGrant {
		t.Fatalf(":grant alone: mode = %s, want the picker", m.mode)
	}
}

func TestGrantCommandRejectsUnknownDuration(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	press(m, "alt+esc", ":")
	typeText(m, "grant 2h")
	press(m, "enter")
	if len(fb.grants) != 0 {
		t.Fatalf("grants = %v for a refused duration, want none", fb.grants)
	}
	if !strings.Contains(m.flash, "5m, 15m, 30m, 1h") {
		t.Fatalf("flash = %q, want the allowed durations", m.flash)
	}
}

func TestGrantCountdown(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "kid")
	feed(m,
		daemon.WireEvent{Session: "kid", Type: daemon.WireSession, Title: "kid", Workspace: "/work", ParentID: "root", Source: "subagent"},
		daemon.WireEvent{Session: "root", Type: daemon.WireGrant, ExpiresAt: fixedNow().Add(14*time.Minute + 32*time.Second).Format(time.RFC3339)},
	)
	if !strings.Contains(m.sessionFacts(), "grant 14:32") {
		t.Fatalf("header = %q, want the countdown for the sub-agent's root", m.sessionFacts())
	}

	m.opts.Now = func() time.Time { return fixedNow().Add(15 * time.Minute) }
	if strings.Contains(m.sessionFacts(), "grant") {
		t.Fatal("the countdown shows after the grant ran out")
	}

	m.opts.Now = fixedNow
	feed(m, daemon.WireEvent{Session: "root", Type: daemon.WireGrant})
	if strings.Contains(m.sessionFacts(), "grant") {
		t.Fatal("an ended grant still shows a countdown")
	}

	if got := grantCountdown(time.Hour + 2*time.Minute + 3*time.Second); got != "1:02:03" {
		t.Fatalf("an hour or more = %q, want 1:02:03", got)
	}
}

func TestBlockedNotice(t *testing.T) {
	fb := &fakeBackend{}
	m := newGrantModel(t, fb, "s1")
	feed(m,
		daemon.WireEvent{Session: "s1", Type: daemon.WireBlocked, Tool: "shell_exec", Rule: "shell_exec(rm *)"},
		daemon.WireEvent{Session: "s1", Type: daemon.WireBlocked, Tool: "shell_exec"},
		daemon.WireEvent{Session: "s1", Type: daemon.WireBlocked, Tool: "shell_exec", Rule: "shell_exec(rm *)", Content: "rm -rf /"},
	)
	var got []string
	for _, b := range m.cache.get("s1").blocks {
		if b.kind == kindNotice {
			got = append(got, b.text)
		}
	}
	want := []string{
		"blocked by config: shell_exec (rule shell_exec(rm *))",
		"blocked by config: shell_exec",
		"blocked by config: shell_exec (rule shell_exec(rm *)) · rm -rf /",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("notices:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestStartGrantFailureFlashes(t *testing.T) {
	fb := &fakeBackend{grantErr: errors.New("boom")}
	m := newGrantModel(t, fb, "s1")
	grantApprovalOn(m, "s1")
	press(m, "alt+esc", "a", "1")
	if !strings.Contains(m.flash, "boom") {
		t.Fatalf("flash = %q, want the failure", m.flash)
	}
	if _, _, ok := m.cache.approvalFor("s1"); !ok {
		t.Fatal("the card went away after a failed grant")
	}
}
