package mem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/recall"
)

// fakeRecall records the query it was given, which is the only way to prove
// scoping happened before the backend saw it.
type fakeRecall struct {
	got  recall.Query
	hits []recall.Hit
	err  error
}

func (f *fakeRecall) Index(context.Context, []recall.Chunk) error { return nil }
func (f *fakeRecall) Search(_ context.Context, q recall.Query) ([]recall.Hit, error) {
	f.got = q
	return f.hits, f.err
}
func (f *fakeRecall) Status(context.Context) (recall.Status, error) {
	return recall.Status{Backend: "fake"}, nil
}

func hit(kind, id, session, text, excerpt string) recall.Hit {
	return recall.Hit{
		Chunk:   recall.Chunk{ID: id, Kind: kind, SessionID: session, Text: text},
		Excerpt: excerpt,
	}
}

func localCtx(sid string) context.Context {
	return policy.WithSession(context.Background(), sid, policy.ProfileLocal)
}

func TestRecallSearchRendersHits(t *testing.T) {
	f := &fakeRecall{hits: []recall.Hit{
		hit(recall.KindMessage, "12", "sess", "full message text", "…excerpt…"),
	}}
	out, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"retry"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "message") || !strings.Contains(out, "…excerpt…") {
		t.Fatalf("hit not rendered: %q", out)
	}
}

// A fact is short and is the retrieval path for a fact that did not fit the
// context budget, so it comes back whole rather than as a snippet.
func TestRecallSearchReturnsWholeFactBodies(t *testing.T) {
	f := &fakeRecall{hits: []recall.Hit{
		hit(recall.KindFact, "prefers-tabs", "", "the entire fact body", "…snip…"),
	}}
	out, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"tabs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the entire fact body") {
		t.Fatalf("fact body not returned in full: %q", out)
	}
}

func TestRecallSearchNoHits(t *testing.T) {
	out, err := NewRecallSearch(&fakeRecall{}).Call(localCtx("sess"), json.RawMessage(`{"query":"nothing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches") {
		t.Fatalf("empty result should say so plainly: %q", out)
	}
}

func TestRecallSearchLocalProfileIsUnscoped(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.SessionID != "" || len(f.got.Kinds) != 0 {
		t.Fatalf("local search was scoped: %+v", f.got)
	}
}

// The policy engine gates tool names, not result scope, so the tool owns this:
// a remote session must never search another session's history or any fact.
func TestRecallSearchRemoteProfileIsScoped(t *testing.T) {
	f := &fakeRecall{}
	ctx := policy.WithSession(context.Background(), "remote-sess", policy.ProfileRemote)
	if _, err := NewRecallSearch(f).Call(ctx, json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.SessionID != "remote-sess" {
		t.Fatalf("remote search not pinned to its own session: %+v", f.got)
	}
	for _, k := range f.got.Kinds {
		if k == recall.KindFact {
			t.Fatal("remote search may read facts")
		}
	}
	if len(f.got.Kinds) == 0 {
		t.Fatal("remote search did not restrict kinds at all")
	}
}

// SessionFrom reports the least-trusted profile when nothing is attached, so a
// caller that forgot WithSession must get the scoped behaviour, not the open one.
func TestRecallSearchWithNoSessionOnContextIsScoped(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(context.Background(), json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.got.Kinds) == 0 {
		t.Fatal("a context with no session was treated as trusted")
	}
}

func TestRecallSearchClampsK(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(localCtx("s"), json.RawMessage(`{"query":"x","k":9999}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.K != recall.MaxK {
		t.Fatalf("K = %d, want %d", f.got.K, recall.MaxK)
	}
}

func TestRecallSearchRejectsAnEmptyQuery(t *testing.T) {
	if _, err := NewRecallSearch(&fakeRecall{}).Call(localCtx("s"), json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestRecallSearchIsReadOnly(t *testing.T) {
	if !NewRecallSearch(&fakeRecall{}).ReadOnly() {
		t.Fatal("recall_search must be read-only so it can dispatch concurrently")
	}
}
