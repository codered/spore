package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func strp(s string) *string { return &s }

func TestRefinementRoundTripKeepsNilContent(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	id, err := st.AddRefinement(ctx, Refinement{
		RoundID: "r1", SessionID: sid, Trigger: "manual", Kind: "fact.create",
		Target: "prefers-tabs", Before: nil, After: strp("body"), Rationale: "user said so", Status: RefineApplied,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.Refinement(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Refinement: ok=%v err=%v", ok, err)
	}
	if got.Before != nil {
		t.Errorf("Before = %q, want nil (the target did not exist)", *got.Before)
	}
	if got.After == nil || *got.After != "body" {
		t.Errorf("After = %v, want body", got.After)
	}
	if got.Status != RefineApplied || got.RoundID != "r1" || got.Target != "prefers-tabs" {
		t.Errorf("row = %+v", got)
	}
}

func TestSetRefinementStatusIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceDiscord)
	id, _ := st.AddRefinement(ctx, Refinement{RoundID: "r", SessionID: sid, Trigger: "idle", Kind: "fact.delete", Target: "x", Before: strp("old"), Rationale: "r", Status: RefineProposed})
	moved, err := st.SetRefinementStatus(ctx, id, RefineProposed, RefineRejected)
	if err != nil || !moved {
		t.Fatalf("first move: moved=%v err=%v", moved, err)
	}
	moved, err = st.SetRefinementStatus(ctx, id, RefineProposed, RefineApplied)
	if err != nil || moved {
		t.Fatalf("second move from a status the row no longer has: moved=%v err=%v", moved, err)
	}
}

func TestLatestAppliedRoundAndListing(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	if _, ok, _ := st.LatestAppliedRound(ctx, sid); ok {
		t.Fatal("a session with no rows has no applied round")
	}
	_, _ = st.AddRefinement(ctx, Refinement{RoundID: "old", SessionID: sid, Trigger: "manual", Kind: "notes.append", Target: "/w/.spore/agent.md", After: strp("a"), Rationale: "r", Status: RefineApplied})
	_, _ = st.AddRefinement(ctx, Refinement{RoundID: "new", SessionID: sid, Trigger: "manual", Kind: "notes.append", Target: "/w/.spore/agent.md", After: strp("b"), Rationale: "r", Status: RefineApplied})
	_, _ = st.AddRefinement(ctx, Refinement{RoundID: "prop", SessionID: sid, Trigger: "manual", Kind: "fact.delete", Target: "x", Before: strp("x"), Rationale: "r", Status: RefineProposed})
	round, ok, err := st.LatestAppliedRound(ctx, sid)
	if err != nil || !ok || round != "new" {
		t.Fatalf("LatestAppliedRound = %q %v %v, want new", round, ok, err)
	}
	proposed, _ := st.Refinements(ctx, RefineProposed, 10)
	if len(proposed) != 1 || proposed[0].RoundID != "prop" {
		t.Errorf("proposed = %+v", proposed)
	}
	all, _ := st.Refinements(ctx, "", 10)
	if len(all) != 3 || all[0].RoundID != "prop" {
		t.Errorf("all (newest first) = %+v", all)
	}
	rows, _ := st.RoundRefinements(ctx, "new")
	if len(rows) != 1 {
		t.Errorf("RoundRefinements = %+v", rows)
	}
}

func TestRefinedThroughOnlyMovesForward(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	if err := st.SetRefinedThrough(ctx, sid, 7); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRefinedThrough(ctx, sid, 3); err != nil {
		t.Fatal(err)
	}
	got, err := st.RefinedThrough(ctx, sid)
	if err != nil || got != 7 {
		t.Fatalf("RefinedThrough = %d %v, want 7", got, err)
	}
}

func appendText(t *testing.T, st *Store, sid, role, text string) {
	t.Helper()
	if _, err := st.AppendMessage(context.Background(), Message{SessionID: sid, Role: role, BlocksJSON: []byte(`[{"type":"text","text":"` + text + `"}]`)}); err != nil {
		t.Fatal(err)
	}
}

func TestIdleSessionsEligibility(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	later := time.Now().Add(time.Hour) // everything is "idle" relative to this

	fresh, _ := st.CreateSessionFrom(ctx, "fresh", "", SourceChat)
	appendText(t, st, fresh, "user", "hello")

	empty, _ := st.CreateSessionFrom(ctx, "empty", "", SourceChat)
	_ = empty

	done, _ := st.CreateSessionFrom(ctx, "done", "", SourceChat)
	appendText(t, st, done, "user", "hello")
	_ = st.MarkRefineAttempt(ctx, done)
	_ = st.SetRefinedThrough(ctx, done, 1)
	appendText(t, st, done, RoleNote, "refined: no changes") // note bumps updated_at, but no new user row

	failed, _ := st.CreateSessionFrom(ctx, "failed", "", SourceChat)
	appendText(t, st, failed, "user", "hello")
	time.Sleep(2 * time.Millisecond)
	_ = st.MarkRefineAttempt(ctx, failed) // attempted after the last message, watermark not moved

	child, _ := st.CreateChildSession(ctx, "child", "", fresh)
	appendText(t, st, child, "user", "hello")

	// Shaped like fresh -- an unreviewed user message and no attempt -- so
	// only the source exclusion keeps it out.
	job, _ := st.CreateSessionFrom(ctx, "job", "", SourceJob)
	appendText(t, st, job, "user", "run task")

	ids, err := st.IdleSessions(ctx, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != fresh {
		t.Fatalf("IdleSessions = %v, want only %s (not job)", ids, fresh)
	}
	if ids, _ := st.IdleSessions(ctx, time.Now().Add(-time.Hour)); len(ids) != 0 {
		t.Fatalf("nothing is idle an hour ago, got %v", ids)
	}

	// A failed session becomes eligible again once a new message arrives.
	time.Sleep(2 * time.Millisecond)
	appendText(t, st, failed, "user", "again")
	ids, _ = st.IdleSessions(ctx, later)
	if len(ids) != 2 {
		t.Fatalf("after a new message the failed session is eligible again, got %v", ids)
	}
}

func TestRefinementNoteDoesNotBumpSessionUpdateTime(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	appendText(t, st, sid, "user", "hello")

	sess1, _, _ := st.Session(ctx, sid)
	origTime := sess1.UpdatedAt

	time.Sleep(2 * time.Millisecond)

	_, _ = st.AppendMessage(ctx, Message{
		SessionID: sid, Role: RoleNote, BlocksJSON: []byte(`[{"type":"text","text":"quiet"}]`),
		Quiet: true,
	})

	sess2, _, _ := st.Session(ctx, sid)
	if sess2.UpdatedAt != origTime {
		t.Errorf("quiet note bumped UpdatedAt from %v to %v", origTime, sess2.UpdatedAt)
	}

	time.Sleep(2 * time.Millisecond)

	_, _ = st.AppendMessage(ctx, Message{
		SessionID: sid, Role: RoleNote, BlocksJSON: []byte(`[{"type":"text","text":"loud"}]`),
		Quiet: false,
	})

	sess3, _, _ := st.Session(ctx, sid)
	if sess3.UpdatedAt.Equal(origTime) || sess3.UpdatedAt.Equal(sess2.UpdatedAt) {
		t.Errorf("non-quiet note did not bump UpdatedAt: was %v, now %v", origTime, sess3.UpdatedAt)
	}
}

func TestOpenAddsRefineColumnsToAnOldSessionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace TEXT NOT NULL DEFAULT '', parent_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '', job_id INTEGER NOT NULL DEFAULT 0, seen_seq INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('s1', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if got, err := st.RefinedThrough(context.Background(), "s1"); err != nil || got != 0 {
		t.Fatalf("RefinedThrough on a migrated row = %d %v", got, err)
	}
	if err := st.MarkRefineAttempt(context.Background(), "s1"); err != nil {
		t.Fatalf("MarkRefineAttempt on a migrated row: %v", err)
	}
}
