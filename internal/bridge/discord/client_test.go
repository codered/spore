package discord

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// TestFakeClientSatisfiesClient is a compile-time assertion with a runtime
// home, so the failure is a test failure rather than a confusing error in
// an unrelated file.
func TestFakeClientSatisfiesClient(t *testing.T) {
	var _ Client = newFakeClient()
}

// TestGatewayClientSatisfiesClient pins the "no dial on construction"
// contract: NewGatewayClient must not dial, because it is called during
// daemon startup and a network round trip there would make startup fail on
// a flaky link rather than on a bad token. A bogus token proves it — if
// NewGatewayClient ever tried to reach Discord, this test would hang or
// fail on the network rather than returning immediately.
func TestGatewayClientSatisfiesClient(t *testing.T) {
	var _ Client = (*gatewayClient)(nil)

	c, err := NewGatewayClient("not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}

// TestFakeClientDetachesNestedSlices pins the fix for the aliasing bug found
// in review: Send and Edit must clone Message's nested Embeds/Buttons
// slices before storing them, because Task 7's renderer streams a turn by
// editing one message repeatedly and the natural implementation reuses an
// Embeds/Buttons buffer across those edits. If the fake stored a reference
// to that buffer instead of a copy, a later mutation by the renderer's
// goroutine would silently corrupt what a test goroutine reads back through
// sentTo/lastEdit — a race that -race cannot catch deterministically and
// that a correctness bug, not just a race, either way.
func TestFakeClientDetachesNestedSlices(t *testing.T) {
	f := newFakeClient()
	ctx := context.Background()

	embeds := []Embed{{Title: "original-embed"}}
	buttons := []Button{{CustomID: "b1", Label: "original-button"}}
	id, err := f.Send(ctx, "chan1", Message{Content: "hello", Embeds: embeds, Buttons: buttons})
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the caller's buffers in place, exactly as a renderer reusing a
	// buffer across streamed edits would.
	embeds[0].Title = "mutated-after-send"
	buttons[0].Label = "mutated-after-send"

	got := f.sentTo("chan1")
	if len(got) != 1 {
		t.Fatalf("sentTo returned %d messages, want 1", len(got))
	}
	if got[0].Message.Embeds[0].Title != "original-embed" {
		t.Errorf("sentTo aliased the caller's Embeds slice: got %q", got[0].Message.Embeds[0].Title)
	}
	if got[0].Message.Buttons[0].Label != "original-button" {
		t.Errorf("sentTo aliased the caller's Buttons slice: got %q", got[0].Message.Buttons[0].Label)
	}

	editEmbeds := []Embed{{Title: "original-edit-embed"}}
	if err := f.Edit(ctx, "chan1", id, Message{Content: "edited", Embeds: editEmbeds}); err != nil {
		t.Fatal(err)
	}
	editEmbeds[0].Title = "mutated-after-edit"

	edit, ok := f.lastEdit("chan1")
	if !ok {
		t.Fatal("lastEdit found nothing")
	}
	if edit.Message.Embeds[0].Title != "original-edit-embed" {
		t.Errorf("lastEdit aliased the caller's Embeds slice: got %q", edit.Message.Embeds[0].Title)
	}
}

// TestFakeClientAccessors pins the behaviour of the accessors added for
// later tasks: openCount (counting failed attempts too, per Task 10's
// supervisor test), closed, allSent, editsTo, and finalContents.
func TestFakeClientAccessors(t *testing.T) {
	f := newFakeClient()
	ctx := context.Background()

	if got := f.openCount(); got != 0 {
		t.Fatalf("openCount before any Open = %d, want 0", got)
	}
	if f.closed() {
		t.Fatal("closed before Close was called")
	}

	f.setFailNext("Open", errors.New("boom"))
	if err := f.Open(ctx, nil, nil); err == nil {
		t.Fatal("expected the queued Open error")
	}
	if got := f.openCount(); got != 1 {
		t.Fatalf("openCount after a failed Open = %d, want 1 (failed attempts count)", got)
	}
	if err := f.Open(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.openCount(); got != 2 {
		t.Fatalf("openCount after a second Open = %d, want 2", got)
	}

	id1, err := f.Send(ctx, "c1", Message{Content: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Send(ctx, "c1", Message{Content: "second"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Edit(ctx, "c1", id1, Message{Content: "first-edited"}); err != nil {
		t.Fatal(err)
	}

	if got := f.allSent(); len(got) != 2 {
		t.Fatalf("allSent returned %d messages, want 2", len(got))
	}

	edits := f.editsTo("c1")
	if len(edits) != 1 || edits[0].Message.Content != "first-edited" {
		t.Fatalf("editsTo(c1) = %+v, want one edit with content %q", edits, "first-edited")
	}

	final := f.finalContents("c1")
	want := []string{"first-edited", "second"}
	if !reflect.DeepEqual(final, want) {
		t.Fatalf("finalContents(c1) = %v, want %v", final, want)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !f.closed() {
		t.Fatal("expected closed() to report true after Close")
	}
}
