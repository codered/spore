//go:build weaviate

package weaviate

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/mirror"
	"github.com/codered/spore/internal/store"
)

// This file asserts the same properties the unit tests do, against a real
// server. It exists because a stub cannot tell you that a filter you built is
// a filter Weaviate accepts.
func liveBackend(t *testing.T) *Backend {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	url := os.Getenv("SPORE_WEAVIATE_URL")
	if url == "" {
		url = "http://127.0.0.1:8080"
	}
	b, err := New(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Ready(ctx); err != nil {
		t.Skipf("no weaviate at %s: %v (run: spore recall setup)", url, err)
	}
	return b
}

func TestLiveRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := liveBackend(t)

	if err := b.DropAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatal(err)
	}
	// Every daemon start calls this, so a second call must be a no-op.
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatalf("EnsureCollection is not idempotent: %v", err)
	}

	now := time.Now().UTC()
	chunks := []recall.Chunk{
		{ID: "1", Kind: recall.KindMessage, SessionID: "sess-a", CreatedAt: now,
			Text: "the retry logic with exponential backoff lives in the provider package"},
		{ID: "2", Kind: recall.KindMessage, SessionID: "sess-b", CreatedAt: now,
			Text: "the cat sat on a warm windowsill in the afternoon"},
		{ID: "coffee", Kind: recall.KindFact, CreatedAt: now,
			Text: "the user drinks their coffee black"},
	}
	if err := b.Index(ctx, chunks); err != nil {
		t.Fatal(err)
	}
	// Indexing is asynchronous server-side; wait rather than asserting into a
	// race.
	time.Sleep(3 * time.Second)

	hits, err := b.Search(ctx, recall.Query{Text: "how do we handle failed requests", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	// The point of this backend: a query sharing no words with the target
	// still finds it. sqlitefts cannot do this, and that is the whole reason
	// the stage exists.
	if hits[0].ID != "1" {
		t.Errorf("top hit is %q (%q), want the retry message", hits[0].ID, hits[0].Text)
	}
	if hits[0].Score <= 0 {
		t.Errorf("score %v, want a real certainty", hits[0].Score)
	}
	if hits[0].CreatedAt.IsZero() {
		t.Error("created_at did not survive the round trip")
	}

	// A different question must reach a different chunk, or the ranking above
	// proved nothing beyond "something came back".
	other, err := b.Search(ctx, recall.Query{Text: "a sleepy animal in the sunshine", K: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) == 0 || other[0].ID != "2" {
		t.Errorf("top hit for an unrelated question is %+v, want the windowsill message", other)
	}

	scoped, err := b.Search(ctx, recall.Query{Text: "anything at all", SessionID: "sess-b", K: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) == 0 {
		t.Error("the session filter matched nothing at all")
	}
	for _, h := range scoped {
		if h.SessionID != "sess-b" {
			t.Errorf("session filter leaked %q; this filter is what stops a bridge user reading another session", h.SessionID)
		}
	}

	noFacts, err := b.Search(ctx, recall.Query{Text: "coffee", Kinds: []string{recall.KindMessage}, K: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range noFacts {
		if h.Kind == recall.KindFact {
			t.Error("the kind filter leaked a fact")
		}
	}

	// Re-indexing the same chunks must overwrite rather than duplicate. This
	// is the property the mirror's crash safety rests on.
	if err := b.Index(ctx, chunks); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	st, err := b.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Degraded {
		t.Fatalf("status degraded against a live server: %s", st.Reason)
	}
	if st.Counts[recall.KindMessage] != 2 {
		t.Errorf("message count = %d after re-indexing, want 2 -- ids are not deduplicating",
			st.Counts[recall.KindMessage])
	}
	if st.Counts[recall.KindFact] != 1 {
		t.Errorf("fact count = %d, want 1", st.Counts[recall.KindFact])
	}
}

// The bug this whole feed exists to close, end to end against a real server:
// delete a fact and its vector goes with it. Every other test in the tree
// proves a part -- that a tombstone is written, that the mirror drains it,
// that a DELETE goes out. Only this one proves the vector is actually gone
// from a store that has really embedded it.
func TestDeletedFactLeavesNoVector(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	b := liveBackend(t)

	if err := b.DropAll(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.EnsureCollection(ctx); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := mirror.New(st, b, Name, slog.New(slog.NewTextHandler(io.Discard, nil)))

	const fact = "takes-coffee-black"
	if err := st.IndexFact(ctx, fact, "the user drinks their coffee black"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatalf("mirror pass: %v", err)
	}
	// Indexing is asynchronous server-side; wait rather than asserting into a
	// race.
	time.Sleep(3 * time.Second)

	hits, err := b.Search(ctx, recall.Query{Text: "what does the user drink", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !holds(hits, fact) {
		t.Fatalf("the fact never reached the vector store, so the deletion proves nothing: %v", hits)
	}

	if err := st.UnindexFact(ctx, fact); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Once(ctx); err != nil {
		t.Fatalf("mirror pass after the deletion: %v", err)
	}
	time.Sleep(3 * time.Second)

	hits, err = b.Search(ctx, recall.Query{Text: "what does the user drink", K: 5})
	if err != nil {
		t.Fatal(err)
	}
	if holds(hits, fact) {
		t.Fatalf("the deleted fact is still searchable: %v", hits)
	}
}

func holds(hits []recall.Hit, refID string) bool {
	for _, h := range hits {
		if h.ID == refID {
			return true
		}
	}
	return false
}
