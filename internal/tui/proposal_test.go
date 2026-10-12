package tui

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
)

func zsProposal() daemon.ProposalJSON {
	return daemon.ProposalJSON{ID: 4, Text: "You've checked ZS three days running. Want me to watch it at the close on weekdays?", InterestKey: "stock:zs"}
}

// companionOpen selects the companion session with the given proposals open.
func companionOpen(t *testing.T, ps ...daemon.ProposalJSON) (*Model, *fakeBackend) {
	t.Helper()
	m, fb := companionScene(t)
	fb.proposals = ps
	runCmd(m, m.selectSession("sp"))
	return m, fb
}

func TestTheBarShowsTheNewestProposal(t *testing.T) {
	older := daemon.ProposalJSON{ID: 3, Text: "Watch Go releases?", InterestKey: "topic:go"}
	m, _ := companionOpen(t, zsProposal(), older)
	v := m.View()
	for _, want := range []string{"spore proposes", "checked ZS", "alt+y", "yes", "not now", "never", "(+1)"} {
		if !strings.Contains(v, want) {
			t.Errorf("view is missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Watch Go releases") {
		t.Error("only the newest proposal is shown")
	}
	golden(t, "proposal-bar-120", v)
}

func TestAltYAnswersTheProposal(t *testing.T) {
	for _, tc := range []struct {
		key    rune
		choice string
	}{{'y', "yes"}, {'n', "not-now"}, {'x', "never"}} {
		m, fb := companionOpen(t, zsProposal())
		run(m, altKey(tc.key)) // the model starts in INSERT
		if len(fb.answers) != 1 || fb.answers[0] != "4:"+tc.choice {
			t.Fatalf("alt+%c: answers = %v, want 4:%s", tc.key, fb.answers, tc.choice)
		}
		if len(m.proposals) != 0 || strings.Contains(m.View(), "spore proposes") {
			t.Fatalf("alt+%c: the bar is still up after the answer", tc.key)
		}
		if len(fb.sent) != 0 {
			t.Fatalf("alt+%c: sent %v, want no message", tc.key, fb.sent)
		}
	}
	// And from NORMAL.
	m, fb := companionOpen(t, zsProposal())
	press(m, "ctrl+o")
	run(m, altKey('y'))
	if len(fb.answers) != 1 {
		t.Fatalf("NORMAL alt+y: answers = %v", fb.answers)
	}
}

func TestAnApprovalAnswersBeforeAProposal(t *testing.T) {
	m, fb := companionOpen(t, zsProposal())
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireApproval, PendingID: 9, Tool: "web_fetch", Args: `{"url":"x"}`})
	if strings.Contains(m.View(), "spore proposes") {
		t.Fatal("the proposal bar must hide while an approval waits")
	}
	run(m, altKey('x'))
	if len(fb.answers) != 0 || len(fb.resolved) != 0 {
		t.Fatalf("alt+x with an approval up: answers=%v resolved=%v, want nothing", fb.answers, fb.resolved)
	}
	run(m, altKey('y')) // still in INSERT: alt+x changed nothing
	if len(fb.answers) != 0 || len(fb.resolved) != 1 {
		t.Fatalf("alt+y with an approval up: answers=%v resolved=%v, want the approval answered", fb.answers, fb.resolved)
	}
}

func TestAnApprovalFirstGolden(t *testing.T) {
	m, _ := companionOpen(t, zsProposal())
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireApproval, PendingID: 9, Tool: "web_fetch", Args: `{"url":"x"}`})
	golden(t, "proposal-approval-first-120", m.View())
}

func TestProposalKeysFallThroughWithNothingOpen(t *testing.T) {
	// In a chat session alt+n is Esc then n: a new session, as before.
	m, fb := companionScene(t)
	fb.created = "s9"
	run(m, altKey('n'))
	if len(fb.answers) != 0 {
		t.Fatalf("answers = %v, want none outside the companion", fb.answers)
	}
	if m.selected != "s9" {
		t.Fatalf("selected = %q, want the new session alt+n fell through to", m.selected)
	}
	// In the companion session with nothing open, alt+y answers nothing.
	m, fb = companionOpen(t)
	run(m, altKey('y'))
	if len(fb.answers) != 0 {
		t.Fatalf("answers = %v, want none with no open proposal", fb.answers)
	}
}

func TestAJobNoteRefetchesProposals(t *testing.T) {
	m, fb := companionOpen(t, zsProposal())
	before := fb.proposalFetches
	fb.proposals = nil // answered on Discord
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireJobNote, Text: "Not now. I'll ask again in two weeks."})
	if fb.proposalFetches != before+1 {
		t.Fatalf("fetches = %d, want one more after the note", fb.proposalFetches)
	}
	if strings.Contains(m.View(), "spore proposes") {
		t.Fatal("the bar still shows a proposal answered elsewhere")
	}
	// Text streaming in the session does not refetch.
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireText, Text: "hi"})
	if fb.proposalFetches != before+1 {
		t.Fatalf("fetches = %d after a text event, want no refetch", fb.proposalFetches)
	}
}

func TestOtherSessionsNeverFetchProposals(t *testing.T) {
	m, fb := companionScene(t)
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireJobNote, Text: "job ran"})
	if fb.proposalFetches != 0 {
		t.Fatalf("fetches = %d, want none outside the companion", fb.proposalFetches)
	}
}

func TestSlashCompanionShowsTheText(t *testing.T) {
	m, fb := companionScene(t)
	typeText(m, "/companion pause 2h")
	press(m, "enter")
	if len(fb.companionArgs) != 1 || strings.Join(fb.companionArgs[0], " ") != "pause 2h" {
		t.Fatalf("companion args = %q", fb.companionArgs)
	}
	if len(fb.sent) != 0 {
		t.Fatalf("sent %v, want no message", fb.sent)
	}
	if !strings.Contains(m.View(), "companion: pause 2h") {
		t.Fatalf("view is missing the command's text:\n%s", m.View())
	}
}

// A transcript can answer before the /seen it raced with. The session on
// screen has been seen, whatever the transcript's Unread says.
func TestATranscriptThatLandsAfterSeenKeepsTheSessionRead(t *testing.T) {
	m, _ := companionScene(t)
	// selectSession clears the unread mark in the cache at once; its
	// commands (the /seen POST and the GET) are not run yet.
	m.selectSession("sp")
	if m.cache.get("sp").info.Unread != false {
		t.Fatal("selecting the companion must clear its unread mark")
	}
	stale := daemon.TranscriptJSON{Session: daemon.SessionJSON{ID: "sp", Title: "spore", Source: "companion", Unread: true}}
	run(m, transcriptMsg{id: "sp", t: stale})
	if m.cache.get("sp").info.Unread {
		t.Fatal("a transcript that answers after the seen POST reads the session unread again")
	}
}
