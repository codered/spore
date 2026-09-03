package main

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// harness drives the model the way Bubble Tea does, capturing everything the
// model would have written to scrollback so a test can assert on the
// transcript without a terminal.
type harness struct {
	ui  *chatUI
	out []string
	// sent and answered record what the injected network calls received.
	sent     []string
	answered []policy.Answer
	sendErr  error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{ui: newChatUI("sess-1", "http://localhost:8080/#sess-1", false)}
	// A blinking cursor schedules a timer on every keystroke. Nothing here
	// asserts on the cursor, and a test must not spend a blink interval per
	// key, so it is held static.
	h.ui.ta.Cursor.SetMode(cursor.CursorStatic)
	h.ui.emit = func(s string) tea.Cmd {
		h.out = append(h.out, s)
		// A real emit returns a command. Returning one here too keeps the
		// model on the same code path it takes in production, where a
		// sequence of two commands is not collapsed into one.
		return func() tea.Msg { return nil }
	}
	h.ui.send = func(text string) error {
		h.sent = append(h.sent, text)
		return h.sendErr
	}
	h.ui.resolve = func(_ int64, ans policy.Answer) error {
		h.answered = append(h.answered, ans)
		return nil
	}
	return h
}

// run applies a message and drains the returned command, feeding any message
// it produces back into the model, which is what the runtime does.
func (h *harness) run(msg tea.Msg) {
	_, cmd := h.ui.Update(msg)
	h.drain(cmd)
}

// drain executes a command the way the runtime would and feeds any resulting
// message back into the model. Commands that do not return promptly are
// timers -- the cursor blink is one -- and the runtime delivers those later;
// a test must not block on them.
func (h *harness) drain(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	var out tea.Msg
	select {
	case out = <-done:
	case <-time.After(150 * time.Millisecond):
		return
	}
	switch m := out.(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range m {
			h.drain(c)
		}
	case sendErrMsg, resolveErr:
		h.run(m)
	default:
		// tea.Sequence produces an unexported []tea.Cmd. Recognise it by
		// shape so ordered commands are executed rather than dropped.
		for _, c := range asCmdSlice(out) {
			h.drain(c)
		}
	}
}

// asCmdSlice returns the commands inside a message that is a slice of
// commands, and nil for anything else.
func asCmdSlice(msg tea.Msg) []tea.Cmd {
	v := reflect.ValueOf(msg)
	if v.Kind() != reflect.Slice || v.Type().Elem() != reflect.TypeOf(tea.Cmd(nil)) {
		return nil
	}
	cmds := make([]tea.Cmd, v.Len())
	for i := range cmds {
		cmds[i] = v.Index(i).Interface().(tea.Cmd)
	}
	return cmds
}

func (h *harness) key(s string) {
	h.run(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)})
}

func (h *harness) press(t tea.KeyType) { h.run(tea.KeyMsg{Type: t}) }

func (h *harness) typed(s string) {
	for _, r := range s {
		h.run(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// ansiCodes matches the styling escape sequences lipgloss and glamour emit.
// Assertions run against the plain text: a test cares that a word reached the
// transcript, not that it arrived wearing a colour.
var ansiCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansiCodes.ReplaceAllString(s, "") }

func (h *harness) transcript() string { return plain(strings.Join(h.out, "\n")) }

func TestEnterSendsTheMessageAndClearsTheInput(t *testing.T) {
	h := newHarness(t)
	h.typed("hello spore")
	h.press(tea.KeyEnter)

	if len(h.sent) != 1 || h.sent[0] != "hello spore" {
		t.Fatalf("sent %v, want one message %q", h.sent, "hello spore")
	}
	if got := h.ui.ta.Value(); got != "" {
		t.Errorf("input not cleared: %q", got)
	}
	if h.ui.state != stateBusy {
		t.Errorf("state %v, want busy after sending", h.ui.state)
	}
	if !strings.Contains(h.transcript(), "hello spore") {
		t.Errorf("sent message not echoed to the transcript: %q", h.transcript())
	}
}

func TestEmptyInputSendsNothing(t *testing.T) {
	h := newHarness(t)
	h.typed("   ")
	h.press(tea.KeyEnter)
	if len(h.sent) != 0 {
		t.Errorf("blank input was sent: %v", h.sent)
	}
}

func TestUpArrowRecallsHistoryAndDownRestoresTheDraft(t *testing.T) {
	h := newHarness(t)
	h.typed("first")
	h.press(tea.KeyEnter)
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"}})
	h.typed("second")
	h.press(tea.KeyEnter)
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"}})

	h.typed("draft")
	h.press(tea.KeyUp)
	if got := h.ui.ta.Value(); got != "second" {
		t.Errorf("first up: got %q, want %q", got, "second")
	}
	h.press(tea.KeyUp)
	if got := h.ui.ta.Value(); got != "first" {
		t.Errorf("second up: got %q, want %q", got, "first")
	}
	h.press(tea.KeyUp)
	if got := h.ui.ta.Value(); got != "first" {
		t.Errorf("up past the oldest entry should stay put, got %q", got)
	}
	h.press(tea.KeyDown)
	h.press(tea.KeyDown)
	if got := h.ui.ta.Value(); got != "draft" {
		t.Errorf("down should restore the unsent draft, got %q", got)
	}
}

func TestArrowKeysNeverReachTheTranscript(t *testing.T) {
	// The bug this replaces: raw escape sequences echoed as text. Whatever
	// the keys do, they must not end up in the message being composed.
	h := newHarness(t)
	h.typed("abc")
	h.press(tea.KeyUp)
	h.press(tea.KeyDown)
	h.press(tea.KeyLeft)
	h.press(tea.KeyRight)
	h.press(tea.KeyBackspace)
	if got := h.ui.ta.Value(); got != "ab" {
		t.Errorf("editing keys produced %q, want %q", got, "ab")
	}
	if strings.ContainsAny(h.ui.ta.Value(), "\x1b[") {
		t.Errorf("escape sequence leaked into the input: %q", h.ui.ta.Value())
	}
}

func TestTextAndToolCallsKeepTheirOrder(t *testing.T) {
	h := newHarness(t)
	h.typed("go")
	h.press(tea.KeyEnter)
	h.out = nil // drop the echo of the prompt

	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireText, Text: "before the call"}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireToolCall, Tool: "fs_read", Args: `{"path":"a.go"}`}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireToolResult, Content: "12345"}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireText, Text: "after the call"}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireTurnDone, Model: "opus", TokensIn: 10, TokensOut: 20}})

	tr := h.transcript()
	iBefore := strings.Index(tr, "before the call")
	iTool := strings.Index(tr, "fs_read")
	iAfter := strings.Index(tr, "after the call")
	if iBefore < 0 || iTool < 0 || iAfter < 0 {
		t.Fatalf("a segment is missing from the transcript:\n%s", tr)
	}
	if !(iBefore < iTool && iTool < iAfter) {
		t.Errorf("segments out of order (%d, %d, %d):\n%s", iBefore, iTool, iAfter, tr)
	}
	if !strings.Contains(tr, "opus") {
		t.Errorf("turn footer missing:\n%s", tr)
	}
	if h.ui.state != stateIdle {
		t.Errorf("state %v, want idle after the turn", h.ui.state)
	}
}

func TestApprovalKeysMapToAnswers(t *testing.T) {
	cases := []struct {
		key     string
		pattern string
		want    policy.Answer
	}{
		{"y", "", policy.Answer{Allow: true, Scope: policy.ScopeOnce}},
		{"n", "", policy.Answer{Allow: false, Scope: policy.ScopeOnce}},
		{"s", "", policy.Answer{Allow: true, Scope: policy.ScopeSession}},
		{"p", "shell_exec(ls *)", policy.Answer{Allow: true, Scope: policy.ScopePattern}},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			h := newHarness(t)
			h.run(streamMsg{daemon.WireEvent{
				Type: daemon.WireApproval, Tool: "shell_exec", PendingID: 7,
				Args: `{"cmd":"ls"}`, Rule: "shell_exec", Pattern: tc.pattern,
			}})
			if h.ui.state != stateApproving {
				t.Fatalf("state %v, want approving", h.ui.state)
			}
			h.key(tc.key)
			if len(h.answered) != 1 || h.answered[0] != tc.want {
				t.Fatalf("answers %v, want one %v", h.answered, tc.want)
			}
			if h.ui.pending != nil {
				t.Error("approval still on screen after an answer")
			}
		})
	}
}

func TestUnofferedPatternKeyIsIgnored(t *testing.T) {
	h := newHarness(t)
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireApproval, Tool: "shell_exec", PendingID: 7, Args: "{}"}})
	h.key("p")
	if len(h.answered) != 0 {
		t.Errorf("p answered %v even though no pattern was offered", h.answered)
	}
	h.key("z")
	if len(h.answered) != 0 {
		t.Errorf("an unrelated key answered the approval: %v", h.answered)
	}
	if h.ui.pending == nil {
		t.Error("the approval should still be waiting")
	}
}

func TestApprovalAnsweredElsewhereIsDropped(t *testing.T) {
	h := newHarness(t)
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireApproval, Tool: "shell_exec", PendingID: 7, Args: "{}"}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireResolved, PendingID: 7, Decision: "allow"}})
	if h.ui.pending != nil {
		t.Error("approval answered elsewhere is still on screen")
	}
	h.key("y")
	if len(h.answered) != 0 {
		t.Errorf("a key press answered an approval that was already settled: %v", h.answered)
	}
}

func TestSecondApprovalIsQueuedAndShownInTurn(t *testing.T) {
	h := newHarness(t)
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireApproval, Tool: "one", PendingID: 1, Args: "{}"}})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireApproval, Tool: "two", PendingID: 2, Args: "{}"}})
	if h.ui.pending == nil || h.ui.pending.Tool != "one" {
		t.Fatalf("first approval should be on screen, got %+v", h.ui.pending)
	}
	h.key("y")
	if h.ui.pending == nil || h.ui.pending.Tool != "two" {
		t.Fatalf("second approval should follow the first, got %+v", h.ui.pending)
	}
}

func TestTypingDuringATurnQueuesTheMessage(t *testing.T) {
	h := newHarness(t)
	h.typed("first")
	h.press(tea.KeyEnter)
	h.typed("second")
	h.press(tea.KeyEnter)

	if len(h.sent) != 1 {
		t.Fatalf("sent %v, want only the first message while busy", h.sent)
	}
	if len(h.ui.queued) != 1 {
		t.Fatalf("queued %v, want the second message held", h.ui.queued)
	}
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"}})
	if len(h.sent) != 2 || h.sent[1] != "second" {
		t.Fatalf("sent %v, want the queued message released after the turn", h.sent)
	}
}

func TestSendFailureReturnsToIdle(t *testing.T) {
	h := newHarness(t)
	h.sendErr = errors.New("daemon gone")
	h.typed("hi")
	h.press(tea.KeyEnter)
	if h.ui.state != stateIdle {
		t.Errorf("state %v, want idle after a failed send", h.ui.state)
	}
	if !strings.Contains(h.transcript(), "daemon gone") {
		t.Errorf("failure not reported:\n%s", h.transcript())
	}
}

func TestStreamEndStoresTheFatalError(t *testing.T) {
	h := newHarness(t)
	want := errors.New("connection reset")
	h.run(streamEndMsg{err: want})
	if !errors.Is(h.ui.fatal, want) {
		t.Errorf("fatal %v, want %v", h.ui.fatal, want)
	}
}

func TestViewShowsThePromptAndKeyHints(t *testing.T) {
	h := newHarness(t)
	h.run(tea.WindowSizeMsg{Width: 100, Height: 30})
	view := plain(h.ui.View())
	for _, want := range []string{"enter", "ctrl+j", "history", "quit"} {
		if !strings.Contains(view, want) {
			t.Errorf("hint %q missing from the view:\n%s", want, view)
		}
	}
}

func TestApprovalViewShowsToolArgumentsAndKeys(t *testing.T) {
	h := newHarness(t)
	h.run(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.run(streamMsg{daemon.WireEvent{
		Type: daemon.WireApproval, Tool: "shell_exec", PendingID: 3,
		Args: `{"cmd":"rm -rf build"}`, Rule: "shell_exec", Pattern: "shell_exec(rm *)",
	}})
	view := plain(h.ui.View())
	for _, want := range []string{"shell_exec", "rm -rf build", "allow once", "deny", "shell_exec(rm *)"} {
		if !strings.Contains(view, want) {
			t.Errorf("approval view missing %q:\n%s", want, view)
		}
	}
}

func TestLiveRegionIsClippedToTheTerminalHeight(t *testing.T) {
	h := newHarness(t)
	h.run(tea.WindowSizeMsg{Width: 80, Height: 20})
	h.run(streamMsg{daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("line\n", 200)}})
	if got := strings.Count(h.ui.View(), "\n"); got > 20 {
		t.Errorf("view is %d lines tall, taller than the 20-row terminal", got)
	}
}

func TestRenderedMarkdownHasNoTrailingWhitespace(t *testing.T) {
	out := renderMarkdown(newMarkdown(60), "A short **line** of prose.\n\n- a bullet\n")
	if out == "" {
		t.Fatal("renderer produced nothing")
	}
	for _, line := range strings.Split(out, "\n") {
		visible := plain(line)
		if visible != strings.TrimRight(visible, " \t") {
			t.Errorf("line has trailing padding: %q", visible)
		}
	}
	if !strings.Contains(plain(out), "a bullet") {
		t.Errorf("content lost while trimming:\n%s", plain(out))
	}
}
