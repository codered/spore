package mirror

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/store"
)

// fakeSource is the store as the mirror sees it: rows with rising ids and a
// cursor it can move.
type fakeSource struct {
	rows       []store.IndexRow
	cursors    map[string]int64
	err        error
	tombstones []store.TombstoneRow
}

func newSource(texts ...string) *fakeSource {
	s := &fakeSource{cursors: map[string]int64{}}
	for i, text := range texts {
		s.rows = append(s.rows, store.IndexRow{
			RowID: int64(i + 1), Kind: recall.KindMessage,
			RefID: text, SessionID: "sess", CreatedAt: "2026-09-03T10:00:00Z", Text: text,
		})
	}
	return s
}

func (s *fakeSource) IndexRowsSince(_ context.Context, cursor int64, limit int) ([]store.IndexRow, error) {
	if s.err != nil {
		return nil, s.err
	}
	var out []store.IndexRow
	for _, r := range s.rows {
		if r.RowID > cursor {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeSource) SyncCursor(_ context.Context, backend string) (int64, error) {
	return s.cursors[backend], nil
}

func (s *fakeSource) SetSyncCursor(_ context.Context, backend string, c int64) error {
	s.cursors[backend] = c
	return nil
}

func (s *fakeSource) SyncDelCursor(_ context.Context, backend string) (int64, error) {
	return s.cursors[backend+"_del"], nil
}

func (s *fakeSource) SetSyncDelCursor(_ context.Context, backend string, c int64) error {
	s.cursors[backend+"_del"] = c
	return nil
}

func (s *fakeSource) TombstonesSince(_ context.Context, delCursor int64, limit int) ([]store.TombstoneRow, error) {
	var out []store.TombstoneRow
	for _, t := range s.tombstones {
		if t.ID > delCursor {
			out = append(out, t)
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *fakeSource) HasTombstoneKey(_ context.Context, kind, refID string) (bool, error) {
	for _, r := range s.rows {
		if r.Kind == kind && r.RefID == refID {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeSource) SweepTombstones(_ context.Context, _ time.Time) error {
	// For the test fake, just remove tombstones that are old enough.
	// We don't track created_at for simplicity in the test.
	return nil
}

func (s *fakeSource) ResetDelCursor(_ context.Context, backend string) error {
	s.cursors[backend+"_del"] = 0
	return nil
}

func (s *fakeSource) ClearTombstones(_ context.Context) error {
	s.tombstones = nil
	return nil
}

type fakeTarget struct {
	got        []recall.Chunk
	fail       int // fail this many calls before succeeding
	failDelete bool
	deleted    []string // ref ids, in the order they were deleted
	calls      []string // "index" and "delete", in the order they arrived
}

func (t *fakeTarget) Index(_ context.Context, chunks []recall.Chunk) error {
	if t.fail > 0 {
		t.fail--
		return errors.New("weaviate: connection refused")
	}
	t.got = append(t.got, chunks...)
	t.calls = append(t.calls, "index")
	return nil
}

func (t *fakeTarget) Search(context.Context, recall.Query) ([]recall.Hit, error) { return nil, nil }
func (t *fakeTarget) Status(context.Context) (recall.Status, error)              { return recall.Status{}, nil }
func (t *fakeTarget) Delete(_ context.Context, _, refID string) error {
	if t.failDelete {
		return errors.New("weaviate: delete failed")
	}
	t.deleted = append(t.deleted, refID)
	t.calls = append(t.calls, "delete")
	return nil
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestMirrorPushesEverythingOnAFirstRun(t *testing.T) {
	src := newSource("one", "two", "three")
	tgt := &fakeTarget{}
	n, err := New(src, tgt, "weaviate", quiet()).Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || len(tgt.got) != 3 {
		t.Fatalf("wrote %d chunks (target saw %d), want 3", n, len(tgt.got))
	}
	if src.cursors["weaviate"] != 3 {
		t.Errorf("cursor = %d, want 3", src.cursors["weaviate"])
	}
	if tgt.got[0].Kind != recall.KindMessage || tgt.got[0].Text != "one" {
		t.Errorf("first chunk = %+v", tgt.got[0])
	}
	if tgt.got[0].CreatedAt.IsZero() {
		t.Error("created_at did not survive the mapping")
	}
}

func TestMirrorSendsOnlyWhatIsNew(t *testing.T) {
	src := newSource("one", "two")
	tgt := &fakeTarget{}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.rows = append(src.rows, store.IndexRow{
		RowID: 3, Kind: recall.KindFact, RefID: "coffee", Text: "black", CreatedAt: "2026-09-03T10:00:00Z",
	})
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("wrote %d chunks, want only the new one", n)
	}
	if len(tgt.got) != 3 || tgt.got[2].ID != "coffee" {
		t.Errorf("target saw %+v, want the fact appended once", tgt.got)
	}
}

func TestMirrorDoesNotAdvanceTheCursorWhenTheTargetFails(t *testing.T) {
	// This is the property that makes a crash safe: a batch the target never
	// accepted must be sent again, which it only is if the cursor stayed put.
	src := newSource("one", "two")
	tgt := &fakeTarget{fail: 1}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err == nil {
		t.Fatal("a failing target reported success")
	}
	if src.cursors["weaviate"] != 0 {
		t.Fatalf("cursor advanced to %d despite the failure", src.cursors["weaviate"])
	}
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("retry wrote %d chunks, want both", n)
	}
}

func TestMirrorIsIdleWhenThereIsNothingNew(t *testing.T) {
	src := newSource("one")
	tgt := &fakeTarget{}
	m := New(src, tgt, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := m.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("wrote %d chunks with nothing new", n)
	}
	if len(tgt.got) != 1 {
		t.Errorf("target saw %d chunks, want no re-send", len(tgt.got))
	}
}

func TestResetRewindsTheCursor(t *testing.T) {
	src := newSource("one", "two")
	m := New(src, &fakeTarget{}, "weaviate", quiet())
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.cursors["weaviate"] != 0 {
		t.Errorf("cursor = %d after a reset, want 0", src.cursors["weaviate"])
	}
}

func TestMirrorSkipsEmptyTextButStillMovesPastIt(t *testing.T) {
	src := newSource("one")
	src.rows = append(src.rows, store.IndexRow{RowID: 2, Kind: recall.KindMessage, RefID: "2", Text: "  "})
	tgt := &fakeTarget{}
	n, err := New(src, tgt, "weaviate", quiet()).Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("wrote %d chunks, want the blank row skipped", n)
	}
	// The cursor must still clear the skipped row, or the mirror re-reads it
	// on every pass forever.
	if src.cursors["weaviate"] != 2 {
		t.Errorf("cursor = %d, want it past the skipped row", src.cursors["weaviate"])
	}
}

func TestMirrorPagesThroughMoreThanOneBatch(t *testing.T) {
	texts := make([]string, batchSize+7)
	for i := range texts {
		texts[i] = "chunk"
	}
	src := newSource(texts...)
	// Each row needs its own ref id, or they would all be one object.
	for i := range src.rows {
		src.rows[i].RefID = string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	tgt := &fakeTarget{}
	n, err := New(src, tgt, "weaviate", quiet()).Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(texts) {
		t.Errorf("wrote %d chunks, want %d -- the pass stopped at one batch", n, len(texts))
	}
}

// A target that errors leaves del_cursor unmoved, so the next pass retries the
// same tombstone. Head-of-line blocking is intended: the tombstones behind it
// wait rather than being skipped, because a skipped one is a vector that
// survives its fact forever, which is the whole bug this feed exists to fix.
func TestDeleteFailureKeepsCursor(t *testing.T) {
	src := &fakeSource{
		cursors: map[string]int64{"weaviate": 0, "weaviate_del": 0},
		tombstones: []store.TombstoneRow{
			{ID: 1, Kind: "fact", RefID: "fail-me"},
			{ID: 2, Kind: "fact", RefID: "behind-it"},
		},
	}
	tgt := &fakeTarget{failDelete: true}
	m := New(src, tgt, "weaviate", quiet())

	// The failure is returned rather than swallowed, so Run reports the mirror
	// as behind instead of logging that it caught up.
	if _, err := m.Once(context.Background()); err == nil {
		t.Fatal("a failed delete was reported as a clean pass")
	}
	if src.cursors["weaviate_del"] != 0 {
		t.Fatalf("del_cursor advanced to %d despite the failure", src.cursors["weaviate_del"])
	}
	if len(tgt.deleted) != 0 {
		t.Fatalf("the tombstone behind the failing one was applied: %v", tgt.deleted)
	}

	// The sidecar comes back. Both deletions land, oldest first.
	tgt.failDelete = false
	if _, err := m.Once(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if got := tgt.deleted; len(got) != 2 || got[0] != "fail-me" || got[1] != "behind-it" {
		t.Fatalf("deleted %v, want [fail-me behind-it] in that order", got)
	}
	if src.cursors["weaviate_del"] != 2 {
		t.Errorf("del_cursor = %d, want 2", src.cursors["weaviate_del"])
	}
}
