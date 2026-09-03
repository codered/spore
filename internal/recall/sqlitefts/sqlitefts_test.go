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

// countingTxQueryer wraps a real *sql.DB and counts BeginTx calls. It exists
// to answer one question the fallback tests below cannot: does Index open a
// transaction for the whole batch, or one per chunk? A transaction-per-chunk
// loop would still "use a transaction" and would still pass every other test
// in this file, while quietly giving up batch atomicity.
type countingTxQueryer struct {
	db       *sql.DB
	beginTxN int
}

func (q *countingTxQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *countingTxQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return q.db.ExecContext(ctx, query, args...)
}

func (q *countingTxQueryer) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	q.beginTxN++
	return q.db.BeginTx(ctx, opts)
}

// TestIndexUsesATransaction pins the one property that matters about Index's
// transactional path: the whole batch shares a single transaction. It fails
// if Index is changed to skip the transaction (BeginTx is never called), and
// it fails just as surely if Index is "fixed" into a transaction-per-chunk
// loop (BeginTx is called more than once) — that would still look
// transactional while defeating the all-or-nothing guarantee the batch
// depends on. FTS5 accepts essentially any text, so there is no query that
// makes a chunk fail mid-batch to prove rollback directly; call-counting is
// the deterministic alternative.
func TestIndexUsesATransaction(t *testing.T) {
	wrapper := &countingTxQueryer{db: newDB(t)}
	b := New(wrapper)

	batch := []recall.Chunk{
		chunk(recall.KindMessage, "m1", "s", "message one"),
		chunk(recall.KindMessage, "m2", "s", "message two"),
		chunk(recall.KindFact, "f1", "", "message three"),
	}
	if err := b.Index(context.Background(), batch); err != nil {
		t.Fatalf("Index batch failed: %v", err)
	}

	hits, err := b.Search(context.Background(), recall.Query{Text: "message"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("expected 3 hits, got %d (batch did not land): %+v", len(hits), hits)
	}

	if wrapper.beginTxN != 1 {
		t.Fatalf("BeginTx called %d times for one batch, want exactly 1", wrapper.beginTxN)
	}
}

// fallbackQueryer wraps a real *sql.DB but deliberately does not implement
// BeginTx, forcing Index down its non-transactional fallback. failAfter, when
// nonzero, makes the Nth ExecContext call fail, so the test can show the
// fallback's defining trait: a mid-batch failure leaves earlier writes in
// place instead of rolling them back.
type fallbackQueryer struct {
	db        *sql.DB
	failAfter int
	execN     int
}

func (q *fallbackQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return q.db.QueryContext(ctx, query, args...)
}

func (q *fallbackQueryer) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	q.execN++
	if q.failAfter != 0 && q.execN == q.failAfter {
		return nil, sql.ErrConnDone
	}
	return q.db.ExecContext(ctx, query, args...)
}

// TestIndexFallbackPath covers the Queryer-without-BeginTx path. This path is
// NOT atomic: it is not a cheaper way to get the same guarantee as
// TestIndexUsesATransaction, it is a different, weaker contract, and the two
// must never be mistaken for each other.
func TestIndexFallbackPath(t *testing.T) {
	db := newDB(t)

	// A normal batch still lands correctly on the fallback path.
	b := New(&fallbackQueryer{db: db})
	if err := b.Index(context.Background(), []recall.Chunk{
		chunk(recall.KindMessage, "1", "s", "text one"),
		chunk(recall.KindMessage, "2", "s", "text two"),
	}); err != nil {
		t.Fatalf("Index failed in fallback path: %v", err)
	}
	hits, err := b.Search(context.Background(), recall.Query{Text: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits, got %d", len(hits))
	}

	// A failure mid-batch proves the fallback is not atomic: the chunk whose
	// DELETE+INSERT already ran stays updated even though the batch as a
	// whole returned an error.
	wrapper := &fallbackQueryer{db: db}
	bSafe := New(wrapper)
	if err := bSafe.Index(context.Background(), []recall.Chunk{
		chunk(recall.KindMessage, "safe", "s1", "safe text"),
	}); err != nil {
		t.Fatalf("initial index failed: %v", err)
	}
	wrapper.execN = 0
	wrapper.failAfter = 3 // fail on "other"'s DELETE, after "safe"'s DELETE+INSERT succeed
	err = bSafe.Index(context.Background(), []recall.Chunk{
		chunk(recall.KindMessage, "safe", "s1", "updated safe text"),
		chunk(recall.KindMessage, "other", "s1", "other text"),
	})
	if err == nil {
		t.Fatal("expected an error from the forced failure")
	}

	rows, err := db.QueryContext(context.Background(),
		`SELECT text FROM recall_fts WHERE kind = ? AND ref_id = ?`, recall.KindMessage, "safe")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("safe chunk missing")
	}
	var text string
	if err := rows.Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "updated safe text" {
		t.Fatalf("fallback path is supposed to leave partial writes in place: got %q", text)
	}
}

var _ recall.Recall = (*Backend)(nil)
