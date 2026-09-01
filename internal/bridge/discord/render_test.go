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

	var embeds []Embed
	for _, m := range append(f.sentTo("C1"), f.editsTo("C1")...) {
		embeds = append(embeds, m.Message.Embeds...)
	}
	if len(embeds) == 0 {
		t.Fatal("the tool call produced no embed")
	}
	found := false
	for _, e := range embeds {
		if strings.Contains(e.Title, "fs_read") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no embed names the tool: %+v", embeds)
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
	for _, m := range append(f.sentTo("C1"), f.editsTo("C1")...) {
		for _, e := range m.Message.Embeds {
			if e.Error {
				return
			}
		}
	}
	t.Fatal("a failed tool call was not marked as an error")
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
	// Discord is a network. A failed edit must not kill the goroutine
	// draining the hub, or the session stops receiving events forever.
	f := newFakeClient()
	f.setFailNext("Send", errors.New("rate limited"))
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "first"},
		daemon.WireEvent{Type: daemon.WireText, Text: "second"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)
	// The point is that Consume returned rather than panicked or blocked.
}
