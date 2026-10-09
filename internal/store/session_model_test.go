package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSetSessionModelRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Session(ctx, id)

	if err := s.SetSessionModel(ctx, id, "chat", "studio/unsloth/Qwen3.8-27B-GGUF"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, id, "subagent", "jetson/gemma"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Session(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Session: %v %v", ok, err)
	}
	if got.ChatModel != "studio/unsloth/Qwen3.8-27B-GGUF" || got.SubagentModel != "jetson/gemma" {
		t.Fatalf("models = %q, %q", got.ChatModel, got.SubagentModel)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("choosing a model moved updated_at")
	}
	if err := s.SetSessionModel(ctx, id, "chat", ""); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, id)
	if got.ChatModel != "" {
		t.Fatalf("ChatModel = %q after clearing", got.ChatModel)
	}
}

func TestSetSessionModelRefusesOtherSitesAndMissingSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "t", "/ws")
	if err := s.SetSessionModel(ctx, id, "title", "a/b"); err == nil {
		t.Error("title accepted as a per-session site")
	}
	if err := s.SetSessionModel(ctx, "missing", "chat", "a/b"); err == nil {
		t.Error("missing session accepted")
	}
}

func TestOldDatabaseGainsModelColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('old', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.Session(context.Background(), "old")
	if err != nil || !ok || got.ChatModel != "" {
		t.Fatalf("old row: %+v %v %v", got, ok, err)
	}
}
