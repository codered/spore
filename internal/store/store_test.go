package store

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendAndReadMessagesInOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id, err := s.CreateSession(ctx, "first session")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, role := range []string{"user", "assistant", "user"} {
		if _, err := s.AppendMessage(ctx, Message{
			SessionID:  id,
			Role:       role,
			BlocksJSON: []byte(`[{"type":"text","text":"hi"}]`),
			Model:      "anthropic/claude-opus-5",
			CallSite:   "chat",
			TokensIn:   10,
			TokensOut:  4,
			CostUSD:    0.001,
		}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	msgs, err := s.Messages(ctx, id)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Errorf("message %d has Seq %d, want %d", i, m.Seq, i+1)
		}
	}
	if msgs[1].Role != "assistant" || msgs[1].TokensIn != 10 {
		t.Errorf("round-trip lost fields: %+v", msgs[1])
	}
}

func TestSummaryRoundTripAndSessionListing(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id, err := s.CreateSession(ctx, "summarised")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.SetSummary(ctx, id, "the user asked about spore", 8); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if text != "the user asked about spore" || through != 8 {
		t.Errorf("Summary = (%q, %d)", text, through)
	}

	// Overwriting replaces rather than accumulating.
	if err := s.SetSummary(ctx, id, "updated", 20); err != nil {
		t.Fatalf("SetSummary (update): %v", err)
	}
	text, through, _ = s.Summary(ctx, id)
	if text != "updated" || through != 20 {
		t.Errorf("after update Summary = (%q, %d)", text, through)
	}

	sessions, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != id {
		t.Errorf("ListSessions = %+v", sessions)
	}
}

func TestSummaryAbsentIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, _ := s.CreateSession(ctx, "empty")
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary on session with no summary: %v", err)
	}
	if text != "" || through != 0 {
		t.Errorf("want zero values, got (%q, %d)", text, through)
	}
}
