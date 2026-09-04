package mirror

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/store"
)

// fakeSource is the store as the mirror sees it: rows with rising ids and a
// cursor it can move.
type fakeSource struct {
	rows    []store.IndexRow
	cursors map[string]int64
	err     error
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

type fakeTarget struct {
	got  []recall.Chunk
	fail int // fail this many calls before succeeding
}

func (t *fakeTarget) Index(_ context.Context, chunks []recall.Chunk) error {
	if t.fail > 0 {
		t.fail--
		return errors.New("weaviate: connection refused")
	}
	t.got = append(t.got, chunks...)
	return nil
}

func (t *fakeTarget) Search(context.Context, recall.Query) ([]recall.Hit, error) { return nil, nil }
func (t *fakeTarget) Status(context.Context) (recall.Status, error)              { return recall.Status{}, nil }

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
