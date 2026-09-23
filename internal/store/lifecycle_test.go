package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"sort"
	"testing"
)

func sortedIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func TestDeletingASessionRemovesItsSubAgentsAndNothingElse(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	root, _ := st.CreateSession(ctx, "root", "/ws")
	kid, err := st.CreateChildSession(ctx, "kid", "/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	grandkid, err := st.CreateChildSession(ctx, "grandkid", "/ws", kid)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := st.CreateSession(ctx, "other", "/ws")
	for _, id := range []string{root, kid, other} {
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: []byte(`[]`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.BindExternal(ctx, "discord", "thread-1", root); err != nil {
		t.Fatal(err)
	}

	deleted, err := st.DeleteSessions(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedIDs(deleted), sortedIDs([]string{root, kid, grandkid}); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("deleted = %v, want the root and both descendants %v", got, want)
	}
	for _, id := range []string{root, kid, grandkid} {
		if _, found, _ := st.Session(ctx, id); found {
			t.Errorf("session %s outlived its delete", id)
		}
		if msgs, _ := st.Messages(ctx, id); len(msgs) != 0 {
			t.Errorf("session %s left %d messages", id, len(msgs))
		}
	}
	if _, found, _ := st.SessionForExternal(ctx, "discord", "thread-1"); found {
		t.Error("the Discord binding outlived its session")
	}
	if _, found, _ := st.Session(ctx, other); !found {
		t.Fatal("deleting one tree removed an unrelated session")
	}
	if msgs, _ := st.Messages(ctx, other); len(msgs) != 1 {
		t.Fatalf("the unrelated session has %d messages, want 1", len(msgs))
	}
}

func TestDeletingAnUnknownSessionDeletesNothing(t *testing.T) {
	st := openTestStore(t)
	deleted, err := st.DeleteSessions(context.Background(), []string{"nope"})
	if err != nil || len(deleted) != 0 {
		t.Fatalf("deleted = %v, err = %v; want nothing and no error", deleted, err)
	}
}

func TestDeleteAllSessionsEmptiesTheStore(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	a, _ := st.CreateSession(ctx, "a", "/ws")
	if _, err := st.CreateChildSession(ctx, "kid", "/ws", a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateSession(ctx, "b", "/ws"); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.DeleteAllSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 {
		t.Fatalf("deleted %d sessions, want 3", len(deleted))
	}
	if left, _ := st.ListSessions(ctx, 100, true); len(left) != 0 {
		t.Fatalf("%d sessions left after deleting all", len(left))
	}
}

func TestSessionTreeListsTheRootAndEveryDescendant(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	root, _ := st.CreateSession(ctx, "root", "/ws")
	kid, _ := st.CreateChildSession(ctx, "kid", "/ws", root)
	tree, err := st.SessionTree(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedIDs(tree); len(got) != 2 || got[0] != sortedIDs([]string{root, kid})[0] {
		t.Fatalf("tree = %v, want %s and %s", tree, root, kid)
	}
}

func TestBindingsForSessionsReportsEachBridgeChannel(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	a, _ := st.CreateSession(ctx, "a", "/ws")
	b, _ := st.CreateSession(ctx, "b", "/ws")
	if err := st.BindExternal(ctx, "discord", "thread-a", a); err != nil {
		t.Fatal(err)
	}
	if err := st.BindExternal(ctx, "discord", "dm-b", b); err != nil {
		t.Fatal(err)
	}
	got, err := st.BindingsForSessions(ctx, "discord", []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ExternalID != "thread-a" || got[0].SessionID != a {
		t.Fatalf("bindings = %+v, want only thread-a for %s", got, a)
	}
}

func TestAJobRunKnowsItsJobAndReadsUnreadUntilSeen(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSessionFrom(ctx, "joke", "/ws", SourceJob)
	if err := st.SetSessionJob(ctx, id, 7); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`)}); err != nil {
		t.Fatal(err)
	}
	sess, _, _ := st.Session(ctx, id)
	if sess.JobID != 7 || sess.LastSeq != 2 || sess.SeenSeq != 0 || !sess.Unread() {
		t.Fatalf("before seen: %+v, want job 7, last seq 2, seen 0, unread", sess)
	}
	if err := st.MarkSessionSeen(ctx, id); err != nil {
		t.Fatal(err)
	}
	list, _ := st.ListSessions(ctx, 10, false)
	if len(list) != 1 || list[0].SeenSeq != 2 || list[0].Unread() {
		t.Fatalf("after seen: %+v, want seen 2 and not unread", list)
	}
}

func TestOpenAddsTheJobAndSeenColumnsToAnOlderSessionsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE sessions (
		id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', workspace TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
		INSERT INTO sessions (id, title, workspace, source, created_at, updated_at)
		VALUES ('old1', 'before', '/ws', 'chat', '2026-09-01T00:00:00.000000000Z', '2026-09-01T00:00:00.000000000Z');`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open over an older sessions table: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	sess, found, err := st.Session(context.Background(), "old1")
	if err != nil || !found {
		t.Fatalf("old row after migration: found=%v err=%v", found, err)
	}
	if sess.Title != "before" || sess.JobID != 0 || sess.SeenSeq != 0 {
		t.Fatalf("old row = %+v, want it kept with job 0 and nothing seen", sess)
	}
}
