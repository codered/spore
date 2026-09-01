package discord

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
)

// drain consumes events from a renderer, closing the channel when done.
func drain(t *testing.T, r *renderer, evs ...daemon.WireEvent) {
	t.Helper()
	ch := make(chan daemon.WireEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	r.Consume(context.Background(), ch)
}

// finalMessages returns the final on-screen state of each message sent to
// channelID: per message id in send order, the latest edit's Message if one
// exists, otherwise the sent Message. This is what the user actually sees.
func finalMessages(f *fakeClient, channelID string) []Message {
	sent := f.sentTo(channelID)
	edits := f.editsTo(channelID)

	// Build a map of message id -> latest edit
	latestEdit := make(map[string]Message)
	for _, e := range edits {
		latestEdit[e.MessageID] = e.Message
	}

	// For each sent message, use its latest edit if any, else the sent one
	result := make([]Message, len(sent))
	for i, s := range sent {
		if edit, ok := latestEdit[s.MessageID]; ok {
			result[i] = edit
		} else {
			result[i] = s.Message
		}
	}
	return result
}

func TestRendererCoalescesTextIntoOneMessage(t *testing.T) {
	f := newFakeClient()
	// A zero throttle makes every flush immediate, so the test is not timing
	// dependent; coalescing is proved by the event loop, not by the clock.
	r := newRenderer(f, "C1", 0)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "Hello, "},
		daemon.WireEvent{Type: daemon.WireText, Text: "world."},
		daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"},
	)

	sent := f.sentTo("C1")
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(sent), sent)
	}
	last, _ := f.lastEdit("C1")
	final := last.Message.Content
	if final == "" {
		final = sent[0].Message.Content
	}
	if final != "Hello, world." {
		t.Fatalf("final content = %q, want %q", final, "Hello, world.")
	}
}

func TestRendererStartsANewMessageAtTheLimit(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	// 1500 + 900 characters cannot fit in one 2000-character message.
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("a", 1500)},
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("b", 900)},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	sent := f.sentTo("C1")
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	for i, m := range sent {
		if len(m.Message.Content) > messageLimit {
			t.Fatalf("message %d is %d characters, over the %d limit", i, len(m.Message.Content), messageLimit)
		}
	}
	// Nothing may be lost at the seam.
	var all strings.Builder
	for _, m := range f.finalContents("C1") {
		all.WriteString(m)
	}
	if got := all.String(); got != strings.Repeat("a", 1500)+strings.Repeat("b", 900) {
		t.Fatalf("content lost at the message boundary: got %d characters, want 2400", len(got))
	}
}

func TestRendererShowsToolCallsAsEmbeds(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "checking"},
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "fs_read", Args: `{"path":"/w/a.go"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "package main"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	// Check what actually survives on screen (not historical calls).
	finals := finalMessages(f, "C1")

	// Both the tool call and result embeds must be visible in their own messages.
	var callFound, resultFound bool
	for _, msg := range finals {
		for _, e := range msg.Embeds {
			if strings.Contains(e.Title, "⚙") && strings.Contains(e.Title, "fs_read") {
				callFound = true
			}
			if strings.Contains(e.Title, "↳") && strings.Contains(e.Title, "fs_read") {
				resultFound = true
			}
		}
	}
	if !callFound || !resultFound {
		t.Fatalf("both tool call and result embeds must survive on screen: callFound=%v, resultFound=%v, finals=%#v", callFound, resultFound, finals)
	}
}

func TestRendererMarksAFailedToolCall(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "shell_exec", Args: `{"cmd":"nope"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "not found", IsError: true},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	// Check what actually survives on screen.
	finals := finalMessages(f, "C1")
	for _, msg := range finals {
		for _, e := range msg.Embeds {
			if e.Error {
				return
			}
		}
	}
	t.Fatal("a failed tool call was not marked as an error in the final on-screen state")
}

func TestRendererReportsATurnError(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	drain(t, r, daemon.WireEvent{Type: daemon.WireError, Error: "provider exploded"})

	for _, c := range f.finalContents("C1") {
		if strings.Contains(c, "provider exploded") {
			return
		}
	}
	t.Fatal("the turn error never reached Discord; a silent failure on a phone is indistinguishable from a hang")
}

func TestRendererSurvivesASendFailure(t *testing.T) {
	// Discord is a network. A failed Send must not kill the goroutine
	// draining the hub, or the session stops receiving events forever. It
	// must also not lose the user's text: unlike a failed Edit — where the
	// content is already captured in currentContent and the next successful
	// edit resends everything — a failed Send has no such recovery path,
	// since nothing server-side ever saw the content. The buffered text must
	// be retried on the next flush, not overwritten and lost.
	f := newFakeClient()
	f.setFailNext("Send", errors.New("rate limited"))
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "FIRST-CHUNK"},
		daemon.WireEvent{Type: daemon.WireText, Text: "SECOND-CHUNK"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	var all strings.Builder
	for _, c := range f.finalContents("C1") {
		all.WriteString(c)
	}
	got := all.String()
	if !strings.Contains(got, "FIRST-CHUNK") || !strings.Contains(got, "SECOND-CHUNK") {
		t.Fatalf("a failed Send lost text: final content = %q, want it to contain both %q and %q", got, "FIRST-CHUNK", "SECOND-CHUNK")
	}
}

func TestRendererSplitsAtNewlines(t *testing.T) {
	// splitAt prefers the last '\n' between n/2 and n when splitting.
	// This tests that the newline-preferring branch is exercised and correct.
	// Input: 40 lines of 80 bytes (79 'x' chars + newline).
	// Total: 3200 bytes, which exceeds messageLimit (2000), causing a split.
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	input := strings.Repeat(strings.Repeat("x", 79)+"\n", 40)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: input},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	contents := f.finalContents("C1")

	// Assertion 1: At least 2 messages were sent (content exceeded limit and split).
	if len(contents) < 2 {
		t.Fatalf("split %d messages, want at least 2", len(contents))
	}

	// Assertion 2: Concatenated content equals input byte-for-byte (test is load-bearing).
	var all strings.Builder
	for _, c := range contents {
		all.WriteString(c)
	}
	if got := all.String(); got != input {
		t.Fatalf("content not preserved: got %d bytes, want %d bytes", len(got), len(input))
	}

	// Assertion 3: No message exceeds messageLimit.
	for i, c := range contents {
		if len(c) > messageLimit {
			t.Fatalf("message %d is %d bytes, exceeds %d limit", i, len(c), messageLimit)
		}
	}
}

func TestRendererBackToBackToolCalls(t *testing.T) {
	// Two tool calls and results with no text between them exercises the
	// WireToolResult reset, which is load-bearing: without it, the second
	// call's embed Edit overwrites the first result's message.
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "fs_read", Args: `{"path":"/a"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "content1"},
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t2", Tool: "shell_exec", Args: `{"cmd":"ls"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t2", Content: "result2"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	// All four embeds must survive in their own messages.
	finals := finalMessages(f, "C1")
	var titles []string
	for _, msg := range finals {
		for _, e := range msg.Embeds {
			titles = append(titles, e.Title)
		}
	}

	expected := []string{"⚙ fs_read", "↳ fs_read", "⚙ shell_exec", "↳ shell_exec"}
	if len(titles) != len(expected) {
		t.Fatalf("expected %d embeds, got %d: titles=%v", len(expected), len(titles), titles)
	}
	for i, exp := range expected {
		if titles[i] != exp {
			t.Fatalf("embed %d: got %q, want %q", i, titles[i], exp)
		}
	}
}

func TestRendererStaysUnderLimitAfterAFailedEdit(t *testing.T) {
	// A failed Edit on the non-split path makes onScreen and currentContent
	// diverge. The split path resets both together, hiding the drift, so the
	// bug only manifests when a failed Edit is followed by a chunk that fits
	// the optimistic room (messageLimit - onScreen) but not the true remaining
	// space (messageLimit - currentContent.Len()).
	//
	// Scenario: 1500 sends (onScreen=1500, currentContent=1500); 100 fails Edit
	// (currentContent grows to 1600, onScreen stays 1500); 450 fits optimistic
	// room of 500 but overflows true remaining 400, sending 2050 bytes.
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	f.setFailNext("Edit", errors.New("rate limited"))

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("a", 1500)},
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("b", 100)},
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("c", 450)},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	for i, m := range finalMessages(f, "C1") {
		if len(m.Content) > messageLimit {
			t.Fatalf("message %d is %d bytes, over the %d limit", i, len(m.Content), messageLimit)
		}
	}
}

func TestRendererKeepsTextWhenAnEditFailsDuringASplit(t *testing.T) {
	// An open message being edited, where the new text also needs a split.
	// The head chunk goes out as an Edit; if that Edit fails, head must be
	// retried, not silently dropped — the live Discord message still shows
	// only the pre-edit content, so nothing else is holding that text.
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("a", 1500)},
		daemon.WireEvent{Type: daemon.WireText, Text: func() string {
			f.setFailNext("Edit", errors.New("rate limited"))
			return strings.Repeat("b", 1500)
		}()},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)
	var all strings.Builder
	for _, c := range f.finalContents("C1") {
		all.WriteString(c)
	}
	got := all.String()
	if a, b := strings.Count(got, "a"), strings.Count(got, "b"); a != 1500 || b != 1500 {
		t.Fatalf("text lost on a failed Edit during a split: got %d a's and %d b's, want 1500 and 1500", a, b)
	}
	for _, m := range finalMessages(f, "C1") {
		if len(m.Content) > messageLimit {
			t.Fatalf("message %d bytes over the %d limit", len(m.Content), messageLimit)
		}
	}
}
