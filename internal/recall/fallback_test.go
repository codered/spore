package recall

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

// fake is a Recall whose every method can be told to fail. It is the whole
// test double this package needs: the fallback's job is deciding between two
// of these.
type fake struct {
	err     error
	hits    []Hit
	indexed []Chunk
	status  Status
}

func (f *fake) Index(_ context.Context, c []Chunk) error {
	if f.err != nil {
		return f.err
	}
	f.indexed = append(f.indexed, c...)
	return nil
}

func (f *fake) Search(context.Context, Query) ([]Hit, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

func (f *fake) Status(context.Context) (Status, error) {
	if f.err != nil {
		return Status{}, f.err
	}
	return f.status, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFallbackPrefersThePrimary(t *testing.T) {
	primary := &fake{hits: []Hit{{Chunk: Chunk{ID: "1"}}}}
	secondary := &fake{hits: []Hit{{Chunk: Chunk{ID: "2"}}}}
	got, err := NewFallback(primary, secondary, quietLogger()).Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("hits = %+v, want the primary's", got)
	}
}

func TestFallbackServesKeywordHitsWhenThePrimaryFails(t *testing.T) {
	primary := &fake{err: errors.New("connection refused")}
	secondary := &fake{hits: []Hit{{Chunk: Chunk{ID: "2"}}}}
	got, err := NewFallback(primary, secondary, quietLogger()).Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatalf("a failing primary broke the search: %v", err)
	}
	if len(got) != 1 || got[0].ID != "2" {
		t.Errorf("hits = %+v, want the secondary's", got)
	}
}

func TestFallbackReportsDegradedAfterItFellBack(t *testing.T) {
	primary := &fake{err: errors.New("connection refused")}
	secondary := &fake{status: Status{Backend: "sqlitefts", Counts: map[string]int{"message": 3}}}
	f := NewFallback(primary, secondary, quietLogger())
	if _, err := f.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	st, err := f.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Degraded {
		t.Error("status is healthy after a fallback")
	}
	if st.Reason == "" {
		t.Error("degraded with no reason")
	}
	// An operator asking `recall status` while degraded wants to know what is
	// actually searchable, which is the keyword index.
	if st.Counts["message"] != 3 {
		t.Errorf("counts = %v, want the working backend's", st.Counts)
	}
}

func TestFallbackSurfacesAPrimaryStatusWhenHealthy(t *testing.T) {
	primary := &fake{status: Status{Backend: "weaviate", Counts: map[string]int{"message": 9}}}
	secondary := &fake{status: Status{Backend: "sqlitefts"}}
	st, err := NewFallback(primary, secondary, quietLogger()).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend != "weaviate" || st.Counts["message"] != 9 || st.Degraded {
		t.Errorf("status = %+v, want the primary's, healthy", st)
	}
}

func TestFallbackPassesThroughAPrimaryDegradedStatus(t *testing.T) {
	primary := &fake{status: Status{Backend: "weaviate", Degraded: true, Reason: "not ready"}}
	secondary := &fake{status: Status{Backend: "sqlitefts", Counts: map[string]int{"message": 2}}}
	st, err := NewFallback(primary, secondary, quietLogger()).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Degraded || st.Reason != "not ready" {
		t.Errorf("status = %+v, want it degraded with the primary's reason", st)
	}
}

func TestFallbackIndexesOnlyThePrimary(t *testing.T) {
	// The secondary is written inside AppendMessage's transaction and is never
	// behind. Writing it here again would double every row.
	primary := &fake{}
	secondary := &fake{}
	f := NewFallback(primary, secondary, quietLogger())
	if err := f.Index(context.Background(), []Chunk{{ID: "1", Kind: KindMessage}}); err != nil {
		t.Fatal(err)
	}
	if len(primary.indexed) != 1 {
		t.Errorf("primary got %d chunks, want 1", len(primary.indexed))
	}
	if len(secondary.indexed) != 0 {
		t.Errorf("secondary got %d chunks, want none", len(secondary.indexed))
	}
}

func TestFallbackRecoversWhenThePrimaryComesBack(t *testing.T) {
	primary := &fake{err: errors.New("down")}
	f := NewFallback(primary, &fake{}, quietLogger())
	if _, err := f.Search(context.Background(), Query{Text: "x"}); err != nil {
		t.Fatal(err)
	}
	primary.err = nil
	primary.hits = []Hit{{Chunk: Chunk{ID: "1"}}}
	got, err := f.Search(context.Background(), Query{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "1" {
		t.Errorf("hits = %+v, want the recovered primary's", got)
	}
	if st, _ := f.Status(context.Background()); st.Degraded {
		t.Error("still degraded after the primary recovered")
	}
}

var _ Recall = (*Fallback)(nil)
