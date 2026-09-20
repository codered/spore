package weaviate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/recall"
)

// recorder is a stand-in Weaviate that remembers the one request it was sent
// and answers with the status the test is about. It exercises the real client
// over real HTTP, which is the only way to see what spore actually puts on the
// wire: a unit test of objectID would pass while the request went elsewhere.
type recorder struct {
	method string
	path   string
	status int
}

func (r *recorder) start(t *testing.T) *Backend {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.method, r.path = req.Method, req.URL.Path
		w.WriteHeader(r.status)
	}))
	t.Cleanup(srv.Close)
	b, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestDeleteRemovesObjectByID(t *testing.T) {
	rec := &recorder{status: http.StatusNoContent}
	b := rec.start(t)

	if err := b.Delete(context.Background(), recall.KindFact, "prefers-tabs"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if rec.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", rec.method)
	}
	// The id is derived rather than pasted: a test carrying its own literal
	// UUID would keep passing if objectID's namespace or key order changed,
	// which is exactly the drift that would orphan every existing vector.
	want := string(objectID(recall.KindFact, "prefers-tabs"))
	if !strings.Contains(rec.path, want) {
		t.Errorf("path = %q, want it to carry the object id %s", rec.path, want)
	}
	// The path carries no collection name. The client drops WithClassName
	// unless it knows the server's version, and spore builds this backend with
	// client.New, which deliberately never probes for it -- a sidecar that is
	// down must not delay daemon startup. Deleting by bare id is the older
	// route and still finds the object, because objectID is a namespaced UUID
	// that no other collection can hold.
	if strings.Contains(rec.path, Collection) {
		t.Logf("path = %q: the client now names the collection; nothing here breaks", rec.path)
	}
}

func TestDeleteMissingObjectIsSuccess(t *testing.T) {
	rec := &recorder{status: http.StatusNotFound}
	b := rec.start(t)

	// The mirror can hold a tombstone for a row whose insert never reached the
	// collection. Treating "already absent" as a failure would stall the
	// delete cursor on a tombstone that can never succeed, and every tombstone
	// behind it with it.
	if err := b.Delete(context.Background(), recall.KindFact, "never-indexed"); err != nil {
		t.Fatalf("a 404 must be success, got: %v", err)
	}
}

func TestDeleteReportsARealFailure(t *testing.T) {
	rec := &recorder{status: http.StatusInternalServerError}
	b := rec.start(t)

	err := b.Delete(context.Background(), recall.KindFact, "prefers-tabs")
	if err == nil {
		t.Fatal("a 500 was reported as success; the tombstone would be dropped")
	}
	if !strings.Contains(err.Error(), Name) {
		t.Errorf("error %q does not say which backend failed", err)
	}
}
