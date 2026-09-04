package weaviate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/recall"
)

// unreachable is an address nothing listens on. It exercises the paths that
// matter without a container: every method must fail fast and say which
// backend failed, because that message is what a degraded status reports.
const unreachable = "http://127.0.0.1:1"

func newUnreachable(t *testing.T) *Backend {
	t.Helper()
	b, err := New(unreachable)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func TestNewRejectsAnUnusableURL(t *testing.T) {
	if _, err := New("::not a url"); err == nil {
		t.Error("an unparseable url was accepted")
	}
	if _, err := New(""); err == nil {
		t.Error("an empty url was accepted")
	}
	if _, err := New("127.0.0.1:8080"); err == nil {
		t.Error("a url with no scheme was accepted")
	}
}

func TestNewSplitsSchemeAndHost(t *testing.T) {
	// The host deliberately does not resolve: construction must not touch the
	// network, because spore builds this backend while wiring the daemon and
	// a sidecar that is down cannot be allowed to delay startup.
	start := time.Now()
	b, err := New("http://box.local.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("New took %s; it dialled the server", elapsed)
	}
	if b.host != "box.local.invalid:8080" || b.scheme != "http" {
		t.Errorf("host %q scheme %q, want box.local.invalid:8080 and http", b.host, b.scheme)
	}
}

func TestSearchFailsLoudlyWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := newUnreachable(t).Search(ctx, recall.Query{Text: "anything"})
	if err == nil {
		t.Fatal("search against a dead address returned no error")
	}
	// The fallback wrapper decides on this error, and an operator reads it in
	// `recall status`, so it must name the backend.
	if !strings.Contains(err.Error(), Name) {
		t.Errorf("error %q does not name the backend", err)
	}
}

func TestEmptyQueryTextSearchesNothing(t *testing.T) {
	hits, err := newUnreachable(t).Search(context.Background(), recall.Query{Text: "   "})
	if err != nil {
		t.Fatalf("an empty query reached the network: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits for an empty query", len(hits))
	}
}

func TestIndexOfNothingIsNotARequest(t *testing.T) {
	if err := newUnreachable(t).Index(context.Background(), nil); err != nil {
		t.Errorf("indexing an empty batch reached the network: %v", err)
	}
}

func TestStatusReportsDegradedWhenUnreachable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := newUnreachable(t).Status(ctx)
	if err != nil {
		t.Fatalf("Status returned an error rather than a degraded status: %v", err)
	}
	if st.Backend != Name {
		t.Errorf("backend %q, want %q", st.Backend, Name)
	}
	if !st.Degraded {
		t.Error("an unreachable backend reported healthy")
	}
	if st.Reason == "" {
		t.Error("degraded with no reason")
	}
}

// The interface assertion is the contract this package exists to satisfy.
var _ recall.Recall = (*Backend)(nil)

func TestErrorsDropTheClientsRepeatedBoilerplate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := newUnreachable(t).Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// This string is what `recall status` prints as the reason search
	// degraded, so it has to read like a diagnosis.
	if strings.Contains(st.Reason, "DerivedFromError") {
		t.Errorf("reason carries the client's boilerplate:\n%s", st.Reason)
	}
	if !strings.Contains(st.Reason, "connection refused") {
		t.Errorf("reason %q drops the actual cause", st.Reason)
	}
}
