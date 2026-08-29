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

func TestListSessionsOrdering(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	// Create two sessions
	id1, err := s.CreateSession(ctx, "first")
	if err != nil {
		t.Fatalf("CreateSession 1: %v", err)
	}
	id2, err := s.CreateSession(ctx, "second")
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}

	// Manually set timestamps to test ordering with fixed-width format
	// id1 gets an exact second, id2 gets +500ms (later)
	exactSecond := "2026-01-01T10:00:00.000000000Z"
	withMillis := "2026-01-01T10:00:00.500000000Z"

	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, exactSecond, id1); err != nil {
		t.Fatalf("update session 1 timestamp: %v", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, withMillis, id2); err != nil {
		t.Fatalf("update session 2 timestamp: %v", err)
	}

	// ListSessions orders by updated_at DESC, so id2 (with later timestamp) should be first
	sessions, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("got %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != id2 {
		t.Errorf("first session should be id2 (later timestamp), got %s", sessions[0].ID)
	}
	if sessions[1].ID != id1 {
		t.Errorf("second session should be id1 (earlier timestamp), got %s", sessions[1].ID)
	}
}
