package mirror

import (
	"context"
	"path/filepath"
	"testing"

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
