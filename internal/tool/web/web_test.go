package web

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/tool"
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

	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20, true)
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
	tl := NewFetchTool(http.DefaultClient, "spore-test", 1<<20, false)
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
	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20, true)
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
	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20, true)
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

func TestFetchBodyLimitFollowsTheContext(t *testing.T) {
	body := strings.Repeat("a", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	tl := NewFetchTool(srv.Client(), "spore-test", 100, true)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	out, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 100 {
		t.Errorf("without a context limit the body is read to maxBytes: got %d bytes, want 100", len(out))
	}
	out, err = tl.Call(tool.WithOutputLimit(context.Background(), 10_000), args)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 5000 {
		t.Errorf("with a 10000-byte context limit the whole body arrives: got %d bytes, want 5000", len(out))
	}
}

func TestFetchBlocksLoopback(t *testing.T) {
	// Start a server so we have a live loopback port to target.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secret"))
	}))
	defer srv.Close()

	tl := NewFetchTool(nil, "spore-test", 1<<20, false)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	_, err := tl.Call(context.Background(), args)
	if err == nil {
		t.Fatal("web_fetch did not block loopback address")
	}
	if !strings.Contains(err.Error(), "private") && !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "refusing") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestFetchBlocksRFC1918(t *testing.T) {
	tl := NewFetchTool(nil, "spore-test", 1<<20, false)
	for _, u := range []string{"http://10.0.0.1/", "http://192.168.1.1/", "http://172.16.0.1/"} {
		args, _ := json.Marshal(map[string]string{"url": u})
		_, err := tl.Call(context.Background(), args)
		if err == nil {
			t.Errorf("web_fetch did not block private address %s", u)
		}
	}
}

func TestFetchAllowsPrivateWhenConfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20, true)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	out, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("web_fetch with allowPrivate=true rejected loopback server: %v", err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("unexpected output: %q", out)
	}
}

func TestIsPrivateIP(t *testing.T) {
	for _, c := range []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.100.100.100", true}, // carrier-grade NAT / Tailscale
		{"0.0.0.0", true},
		{"::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"::ffff:127.0.0.1", true}, // IPv4-mapped loopback
		{"93.184.216.34", false},
		{"2606:4700::1111", false},
	} {
		if got := isPrivateIP(net.ParseIP(c.ip)); got != c.want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", c.ip, got, c.want)
		}
	}
}

// The guard must dial the address it checked. Dialing the name would make
// the transport resolve it again, and a rebinding server answers that second
// lookup with a private address.
func TestGuardedDialerDialsTheCheckedAddress(t *testing.T) {
	lookups := 0
	var dialed string
	g := guardedDialer{
		lookup: func(_ context.Context, host string) ([]net.IPAddr, error) {
			lookups++
			if lookups > 1 {
				return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
			}
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
		},
		dial: func(_ context.Context, _, addr string) (net.Conn, error) {
			dialed = addr
			return nil, errors.New("test: no network")
		},
	}
	_, _ = g.DialContext(context.Background(), "tcp", "rebind.test:80")
	if lookups != 1 {
		t.Errorf("host resolved %d times, want 1", lookups)
	}
	if dialed != "93.184.216.34:80" {
		t.Errorf("dialed %q, want the checked address 93.184.216.34:80", dialed)
	}
}

func TestGuardedDialerRefusesAnyPrivateAddress(t *testing.T) {
	g := guardedDialer{
		lookup: func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}, {IP: net.ParseIP("10.0.0.1")}}, nil
		},
		dial: func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialed despite a private address in the answer")
			return nil, nil
		},
	}
	if _, err := g.DialContext(context.Background(), "tcp", "mixed.test:80"); err == nil {
		t.Fatal("a host with one private address was allowed")
	}
}

// A public site that redirects to a private address must be refused at the
// redirect hop. The fake resolver gives public.test a public address and the
// fake dialer routes every connection to the local test server.
func TestFetchRefusesRedirectToPrivate(t *testing.T) {
	var target string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "public.test" && r.URL.Path == "/hop" {
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("reached " + r.Host))
	}))
	defer srv.Close()
	target = srv.URL + "/secret"
	srvAddr := srv.Listener.Addr().String()

	var d net.Dialer
	g := guardedDialer{
		lookup: func(ctx context.Context, host string) ([]net.IPAddr, error) {
			if host == "public.test" {
				return []net.IPAddr{{IP: net.ParseIP("93.184.216.34")}}, nil
			}
			return net.DefaultResolver.LookupIPAddr(ctx, host)
		},
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, srvAddr)
		},
	}
	hc := &http.Client{Transport: &http.Transport{DialContext: g.DialContext}}
	tl := NewFetchTool(hc, "spore-test", 1<<20, false)

	// The wiring itself works: a public page with no redirect is fetched.
	args, _ := json.Marshal(map[string]string{"url": "http://public.test/page"})
	if out, err := tl.Call(context.Background(), args); err != nil || !strings.Contains(out, "reached public.test") {
		t.Fatalf("plain fetch through the fake resolver = %q, %v", out, err)
	}

	args, _ = json.Marshal(map[string]string{"url": "http://public.test/hop"})
	out, err := tl.Call(context.Background(), args)
	if err == nil {
		t.Fatalf("redirect to %s was followed: %q", target, out)
	}
	if !strings.Contains(err.Error(), "private address") {
		t.Errorf("error = %v, want the private-address refusal", err)
	}
}
