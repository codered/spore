package mirror

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

// The tests below run against a real store rather than fakeSource. The seam
// this feature wires is a table: a fake that returns tombstones from a slice
// would pass whether or not UnindexFact ever wrote one, which is the failure
// the whole delete path exists to prevent.
func realStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestDeletePropagatesToTarget(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	tgt := &fakeTarget{}
	m := New(st, tgt, "weaviate", quiet())

	if err := st.IndexFact(ctx, "prefers-tabs", "the user prefers tabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if len(tgt.got) != 1 {
		t.Fatalf("the fact was not mirrored in the first place: %v", tgt.got)
	}

	if err := st.UnindexFact(ctx, "prefers-tabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if got := tgt.deleted; len(got) != 1 || got[0] != "prefers-tabs" {
		t.Fatalf("deleted %v, want [prefers-tabs] -- the vector outlived the fact", got)
	}
}

// A fact deleted and written again under the same name occupies the same
// object id, because objectID hashes kind and ref id alone. Applying the
// tombstone after the re-index would destroy the live vector.
func TestStaleTombstoneSkipped(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	tgt := &fakeTarget{}
	m := New(st, tgt, "weaviate", quiet())

	if err := st.IndexFact(ctx, "prefers-tabs", "the user prefers tabs"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnindexFact(ctx, "prefers-tabs"); err != nil {
		t.Fatal(err)
	}
	// Written again before the mirror ever drained the tombstone.
	if err := st.IndexFact(ctx, "prefers-tabs", "the user prefers tabs, still"); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if len(tgt.deleted) != 0 {
		t.Fatalf("deleted %v, want nothing -- the fact is back in the index", tgt.deleted)
	}
	if len(tgt.got) != 1 || tgt.got[0].ID != "prefers-tabs" {
		t.Errorf("the re-created fact was not mirrored: %v", tgt.got)
	}

	// The stale tombstone is consumed rather than reconsidered forever.
	del, err := st.SyncDelCursor(ctx, "weaviate")
	if err != nil {
		t.Fatal(err)
	}
	if del == 0 {
		t.Error("del_cursor stayed at 0; the stale tombstone will be walked on every pass")
	}
}

// Inserts run first. A row read into an in-flight batch can be deleted in
// SQLite before that batch lands, so draining tombstones afterwards is what
// catches the object created after its own tombstone was written.
func TestInsertsRunBeforeDeletes(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	tgt := &fakeTarget{}
	m := New(st, tgt, "weaviate", quiet())

	if err := st.IndexFact(ctx, "prefers-dark", "the user prefers dark mode"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}
	tgt.calls = nil

	// One pass with both an insert and a deletion pending.
	if err := st.IndexFact(ctx, "prefers-tabs", "the user prefers tabs"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnindexFact(ctx, "prefers-dark"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if len(tgt.calls) != 2 || tgt.calls[0] != "index" || tgt.calls[1] != "delete" {
		t.Fatalf("calls = %v, want [index delete]", tgt.calls)
	}
}

// The end of the path the triggers open: a deleted message loses its vector
// without anything in the mirror knowing what a message is.
func TestMessageDeletePropagates(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	tgt := &fakeTarget{}
	m := New(st, tgt, "weaviate", quiet())

	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AppendMessage(ctx, store.Message{
		SessionID:  sid,
		Role:       "user",
		BlocksJSON: []byte(`[{"type":"text","text":"a searchable line"}]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tgt.got) != 1 {
		t.Fatalf("the message was not mirrored in the first place: %v", tgt.got)
	}

	if _, err := st.DB().Exec(`DELETE FROM messages WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}

	want := strconv.FormatInt(id, 10)
	if got := tgt.deleted; len(got) != 1 || got[0] != want {
		t.Fatalf("deleted %v, want [%s]", got, want)
	}
}

// timeFormat is the shape the store writes created_at in. The tests below
// write the column directly rather than sleeping, so the age of a tombstone is
// a fact about the row instead of a fact about how long the test ran.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func writeTombstone(t *testing.T, st *store.Store, refID string, age time.Duration) {
	t.Helper()
	when := time.Now().UTC().Add(-age).Format(timeFormat)
	if _, err := st.DB().Exec(
		`INSERT INTO recall_tombstones (kind, ref_id, created_at) VALUES ('fact', ?, ?)`,
		refID, when); err != nil {
		t.Fatal(err)
	}
}

func countTombstones(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM recall_tombstones`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The table is bounded by the sweep alone. Without it the feed grows with the
// history of every deletion the installation has ever made.
func TestSweepDropsAgedTombstones(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	m := New(st, &fakeTarget{}, "weaviate", quiet())

	writeTombstone(t, st, "long-gone", tombstoneTTL+time.Hour)

	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if n := countTombstones(t, st); n != 0 {
		t.Fatalf("%d tombstones survived the sweep, want 0", n)
	}
}

// A tombstone inside the window stays, because a backend that has not caught
// up yet still has to see it.
func TestSweepSparesFreshTombstones(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	m := New(st, &fakeTarget{}, "weaviate", quiet())

	writeTombstone(t, st, "just-deleted", time.Hour)

	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}

	if n := countTombstones(t, st); n != 1 {
		t.Fatalf("got %d tombstones, want the fresh one kept", n)
	}
}

// `recall reindex` drops the collection and rebuilds recall_fts, which
// renumbers every rowid. Deletes queued against the collection that no longer
// exists mean nothing, and a del_cursor pointing into a cleared table would
// silence the tombstones written after it.
func TestResetClearsTombstonesAndDelCursor(t *testing.T) {
	ctx := context.Background()
	st := realStore(t)
	tgt := &fakeTarget{}
	m := New(st, tgt, "weaviate", quiet())

	if err := st.IndexFact(ctx, "prefers-tabs", "the user prefers tabs"); err != nil {
		t.Fatal(err)
	}
	if err := st.UnindexFact(ctx, "prefers-tabs"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if del, err := st.SyncDelCursor(ctx, "weaviate"); err != nil || del == 0 {
		t.Fatalf("del_cursor = %d (err %v); the test needs it moved before Reset", del, err)
	}

	if err := m.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	if n := countTombstones(t, st); n != 0 {
		t.Errorf("%d tombstones survived Reset, want 0", n)
	}
	del, err := st.SyncDelCursor(ctx, "weaviate")
	if err != nil {
		t.Fatal(err)
	}
	if del != 0 {
		t.Errorf("del_cursor = %d after Reset, want 0", del)
	}
	cursor, err := st.SyncCursor(ctx, "weaviate")
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 0 {
		t.Errorf("cursor = %d after Reset, want 0", cursor)
	}
}
