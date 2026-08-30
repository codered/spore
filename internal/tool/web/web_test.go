package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
)

const braveFixture = `{"web":{"results":[
{"title":"Go","url":"https://go.dev","description":"The Go <strong>language</strong>"},
{"title":"SQLite","url":"https://sqlite.org","description":"Embedded database"}]}}`

func TestBraveSearchParsesResults(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(braveFixture))
	}))
	defer srv.Close()

	b := NewBrave("test-key", srv.Client())
	b.BaseURL = srv.URL
	hits, err := b.Search(context.Background(), "go language", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotKey != "test-key" || gotQuery != "go language" {
		t.Errorf("request carried key %q query %q", gotKey, gotQuery)
	}
	if len(hits) != 2 || hits[0].URL != "https://go.dev" {
		t.Fatalf("hits = %+v", hits)
	}
	// Brave marks matched terms with HTML; the model should see plain text.
	if strings.Contains(hits[0].Snippet, "<strong>") {
		t.Errorf("snippet still contains markup: %q", hits[0].Snippet)
	}
}

func TestBraveSurfacesUpstreamErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()
	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	if _, err := b.Search(context.Background(), "x", 3); err == nil {
		t.Fatal("Search swallowed a 429")
	}
}

type fakeSearch struct{ hits []Hit }

func (f fakeSearch) Search(context.Context, string, int) ([]Hit, error) { return f.hits, nil }

func TestSearchToolRendersHits(t *testing.T) {
	tl := NewSearchTool(fakeSearch{hits: []Hit{{Title: "Go", URL: "https://go.dev", Snippet: "lang"}}})
	out, err := tl.Call(context.Background(), json.RawMessage(`{"query":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go", "https://go.dev", "lang"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
	if !tl.ReadOnly() {
		t.Error("web_search must be read-only")
	}
}

func TestSearchToolReportsNoResults(t *testing.T) {
	tl := NewSearchTool(fakeSearch{})
	out, _ := tl.Call(context.Background(), json.RawMessage(`{"query":"zzz"}`))
	if !strings.Contains(out, "no results") {
		t.Errorf("out = %q, want an explicit empty-result message", out)
	}
}

const pageFixture = `<!doctype html>
<html><head><title>A Page</title><style>body{color:red}</style><script>var x=1</script></head>
<body>
<nav>skip me</nav>
<h1>Heading</h1>
<p>First para with <a href="https://go.dev">a link</a>.</p>
<pre>code block</pre>
<ul><li>one</li><li>two</li></ul>
</body></html>`

func TestFetchConvertsHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageFixture))
	}))
	defer srv.Close()

	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	out, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"A Page", "Heading", "First para", "a link", "code block", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"color:red", "var x=1"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output leaked %q:\n%s", unwanted, out)
		}
	}
}

func TestFetchRejectsNonHTTPSchemes(t *testing.T) {
	tl := NewFetchTool(http.DefaultClient, "spore-test", 1<<20)
	for _, u := range []string{"file:///etc/passwd", "ftp://x/y", "notaurl"} {
		args, _ := json.Marshal(map[string]string{"url": u})
		if _, err := tl.Call(context.Background(), args); err == nil {
			t.Errorf("web_fetch accepted %q", u)
		}
	}
}

func TestFetchReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	if _, err := tl.Call(context.Background(), args); err == nil {
		t.Fatal("web_fetch swallowed a 404")
	}
}

func TestStripTagsKeepsLiteralAngleBrackets(t *testing.T) {
	// Brave wraps matched terms in <strong>, but a snippet is arbitrary text
	// from a web page: "x < y" must survive intact rather than losing its tail.
	cases := map[string]string{
		"The Go <strong>language</strong>": "The Go language",
		"Node: x < y comparisons":          "Node: x < y comparisons",
		"a < b and c > d":                  "a < b and c > d",
		"unterminated <tag":                "unterminated <tag",
		"AT&amp;T":                         "AT&T",
		"<em>only</em>":                    "only",
	}
	for in, want := range cases {
		if got := stripTags(in); got != want {
			t.Errorf("stripTags(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchRefusesARedirectAwayFromHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A server that answers a plain fetch with a redirect into the local
		// filesystem. The scheme check on the first URL cannot see this.
		w.Header().Set("Location", "file:///etc/passwd")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	out, err := tl.Call(context.Background(), args)
	if err == nil {
		t.Fatalf("web_fetch followed a redirect out of http(s) and returned %.80q", out)
	}
}

func TestNewOmitsSearchWithoutAKey(t *testing.T) {
	names := func(cfg config.WebConfig) []string {
		var out []string
		for _, tl := range New(cfg, 1<<20) {
			out = append(out, tl.Name())
		}
		return out
	}
	got := names(config.WebConfig{SearchProvider: "brave"})
	if len(got) != 1 || got[0] != "web_fetch" {
		t.Errorf("without a key, tools = %v, want only web_fetch", got)
	}
	got = names(config.WebConfig{SearchProvider: "brave", BraveAPIKey: "k"})
	if len(got) != 2 {
		t.Errorf("with a key, tools = %v, want both", got)
	}
}
