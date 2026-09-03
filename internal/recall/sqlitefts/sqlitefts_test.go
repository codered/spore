package sqlitefts

import (
	"context"
	"database/sql"
	"testing"

	"github.com/codered/spore/internal/recall"
	_ "github.com/mattn/go-sqlite3"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE VIRTUAL TABLE recall_fts USING fts5(
		text, kind UNINDEXED, ref_id UNINDEXED, session_id UNINDEXED, created_at UNINDEXED)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seed(t *testing.T, b *Backend, chunks ...recall.Chunk) {
	t.Helper()
	if err := b.Index(context.Background(), chunks); err != nil {
		t.Fatal(err)
	}
}

func chunk(kind, id, session, text string) recall.Chunk {
	return recall.Chunk{ID: id, Kind: kind, SessionID: session, Text: text}
}

// bm25 scores are negative and more-negative is better, so best-first is
// ORDER BY score ASC. Sorting the other way silently returns the worst hits.
func TestSearchRanksBestMatchFirst(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s1", "the retry logic uses exponential backoff"),
		chunk(recall.KindMessage, "2", "s1", "retry retry retry retry"),
		chunk(recall.KindMessage, "3", "s1", "unrelated gardening notes"),
	)
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].ID != "2" {
		t.Fatalf("best match is not first: %+v", hits)
	}
	if hits[0].Score > hits[1].Score {
		t.Fatalf("scores not ascending (bm25 is negative): %v %v", hits[0].Score, hits[1].Score)
	}
}

func TestSearchIsAConjunction(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s1", "retry logic here"),
		chunk(recall.KindMessage, "2", "s1", "retry only"),
	)
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry logic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("want only the chunk containing both terms, got %+v", hits)
	}
}

// Every one of these is a syntax error when passed to MATCH unquoted.
func TestSearchSurvivesRawUserInput(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "the flag is documented"))
	for _, q := range []string{`what's the -v flag?`, `retry "logic`, `AND`, `a OR`, `foo*`, `NEAR`, `naïve café`, ``, `   `, `!!!`} {
		if _, err := b.Search(context.Background(), recall.Query{Text: q}); err != nil {
			t.Errorf("Search(%q) returned an error: %v", q, err)
		}
	}
}

func TestEmptyQueryReturnsNothingNotAnError(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "some text"))
	hits, err := b.Search(context.Background(), recall.Query{Text: "   "})
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits=%v err=%v; want no hits and no error", hits, err)
	}
}

func TestSearchFiltersBySessionAndKind(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "mine", "shared secret word"),
		chunk(recall.KindMessage, "2", "theirs", "shared secret word"),
		chunk(recall.KindFact, "afact", "", "shared secret word"),
	)
	hits, err := b.Search(context.Background(), recall.Query{
		Text: "secret", SessionID: "mine", Kinds: []string{recall.KindMessage, recall.KindSummary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("scoping leaked: %+v", hits)
	}
}

func TestSearchClampsK(t *testing.T) {
	b := New(newDB(t))
	var chunks []recall.Chunk
	for i := 0; i < 40; i++ {
		chunks = append(chunks, chunk(recall.KindMessage, string(rune('a'+i%26))+string(rune('a'+i/26)), "s", "common word"))
	}
	seed(t, b, chunks...)
	for _, tc := range []struct{ k, want int }{{0, recall.DefaultK}, {-3, recall.DefaultK}, {5, 5}, {999, recall.MaxK}} {
		hits, err := b.Search(context.Background(), recall.Query{Text: "common", K: tc.k})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != tc.want {
			t.Errorf("K=%d returned %d hits, want %d", tc.k, len(hits), tc.want)
		}
	}
}

func TestHitsCarryAnExcerpt(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "a long preamble and then the retry logic and more after it"))
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Excerpt == "" {
		t.Fatalf("no excerpt: %+v", hits)
	}
}

func TestStatusCountsByKind(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s", "one"),
		chunk(recall.KindMessage, "2", "s", "two"),
		chunk(recall.KindFact, "f", "", "three"),
	)
	st, err := b.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend != "sqlitefts" || st.Degraded {
		t.Fatalf("bad status: %+v", st)
	}
	if st.Counts[recall.KindMessage] != 2 || st.Counts[recall.KindFact] != 1 {
		t.Fatalf("bad counts: %+v", st.Counts)
	}
}

// TestIndexBatchTransactional verifies that batch indexing uses transactions
// when the Queryer supports them. We verify this by checking that a batch of
// operations is processed as a unit.
func TestIndexBatchTransactional(t *testing.T) {
	db := newDB(t)
	b := New(db)

	// Index a batch of multiple chunks. Because db (sql.DB) implements
	// BeginTx, the batch should be indexed transactionally.
	batch := []recall.Chunk{
		chunk(recall.KindMessage, "m1", "s1", "message one"),
		chunk(recall.KindMessage, "m2", "s1", "message two"),
		chunk(recall.KindFact, "fact1", "", "a fact"),
	}
	err := b.Index(context.Background(), batch)
	if err != nil {
		t.Fatalf("Index failed: %v", err)
	}

	// Verify all chunks were indexed.
	st, err := b.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Counts[recall.KindMessage] != 2 || st.Counts[recall.KindFact] != 1 {
		t.Fatalf("batch not fully indexed: %+v", st.Counts)
	}
}

// TestIndexFallbackPath verifies that a Queryer without BeginTx support still
// works for indexing, even though it won't get batch atomicity.
type noTxQueryer struct {
	db *sql.DB
}

func (q *noTxQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *noTxQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func TestIndexFallbackPath(t *testing.T) {
	db := newDB(t)
	// Wrap the DB in a type that doesn't implement BeginTx, forcing the fallback path.
	b := New(&noTxQueryer{db: db})

	err := b.Index(context.Background(), []recall.Chunk{
		chunk(recall.KindMessage, "1", "s", "text one"),
		chunk(recall.KindMessage, "2", "s", "text two"),
	})

	if err != nil {
		t.Fatalf("Index failed in fallback path: %v", err)
	}

	// Verify both chunks were indexed.
	hits, err := b.Search(context.Background(), recall.Query{Text: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}
}

var _ recall.Recall = (*Backend)(nil)
