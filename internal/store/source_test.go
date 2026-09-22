package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestCreateSessionFromRecordsTheSource(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.CreateSessionFrom(ctx, "t", "/ws", SourceChat)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok, err := s.Session(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Session: ok=%v err=%v", ok, err)
	}
	if sess.Source != SourceChat {
		t.Fatalf("Source = %q, want %q", sess.Source, SourceChat)
	}
}

func TestCreateSessionWithoutASourceIsUnknown(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	sess, _, _ := s.Session(ctx, id)
	if sess.Source != SourceUnknown {
		t.Fatalf("Source = %q, want %q", sess.Source, SourceUnknown)
	}
}

func TestChildSessionsAreSubagents(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	parent, _ := s.CreateSessionFrom(ctx, "p", "/ws", SourceChat)
	child, err := s.CreateChildSession(ctx, "c", "/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, sess := range list {
		got[sess.ID] = sess.Source
	}
	if got[parent] != SourceChat || got[child] != SourceSubagent {
		t.Fatalf("sources = %v, want parent chat and child subagent", got)
	}
}

// A database written before the column existed must gain it on Open, with
// a parent meaning subagent and everything else unknown. This proves the
// migration is invoked, not merely defined.
func TestOpenAddsAndBackfillsSessionSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
	INSERT INTO sessions VALUES ('top', 'a chat', '/ws', '', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');
	INSERT INTO sessions VALUES ('kid', 'a child', '/ws', 'top', '2026-09-01T00:00:00Z', '2026-09-01T00:00:00Z');`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on an old database: %v", err)
	}
	defer s.Close()
	for id, want := range map[string]string{"top": SourceUnknown, "kid": SourceSubagent} {
		sess, ok, err := s.Session(context.Background(), id)
		if err != nil || !ok {
			t.Fatalf("Session(%s): ok=%v err=%v", id, ok, err)
		}
		if sess.Source != want {
			t.Errorf("Source(%s) = %q, want %q", id, sess.Source, want)
		}
	}
}

func TestPendingCallsAllSpansSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateSession(ctx, "a", "/ws")
	b, _ := s.CreateSession(ctx, "b", "/ws")
	for _, sid := range []string{a, b} {
		if _, err := s.AddPendingCall(ctx, PendingCall{
			SessionID: sid, ToolUseID: "t-" + sid, Tool: "shell", Profile: "local", Rule: "ask", ArgsJSON: []byte(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.PendingCallsAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].SessionID != a || all[1].SessionID != b {
		t.Fatalf("PendingCallsAll = %+v, want one per session, oldest first", all)
	}
}
