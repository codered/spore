package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/codered/spore/web"
)

func TestIndexRenders(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	page := string(body)
	for _, want := range []string{"<title>spore</title>", `id="transcript"`, `id="sidebar"`, `id="settings"`, "/static/app.js", "/static/style.css"} {
		if !strings.Contains(page, want) {
			t.Errorf("index page is missing %q", want)
		}
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct{ path, contentType, needle string }{
		{"/static/app.js", "javascript", "EventSource"},
		{"/static/app.js", "javascript", "spore.keys"},
		{"/static/style.css", "css", "#transcript"},
		{"/static/style.css", "css", "--selected"},
	} {
		res, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.contentType) {
			t.Errorf("%s content type = %q, want something containing %q", tc.path, ct, tc.contentType)
		}
		if !strings.Contains(string(body), tc.needle) {
			t.Errorf("%s does not contain %q; is the file actually embedded?", tc.path, tc.needle)
		}
	}
}

// The binary must work with no internet: an asset pulled from a CDN would
// leave the UI blank on an offline machine, which is the deployment target.
func TestUIReferencesNoExternalResources(t *testing.T) {
	_, ts := newTestServer(t)
	for _, path := range []string{"/", "/static/app.js", "/static/style.css"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		for _, bad := range []string{"http://", "https://", "//cdn.", "unpkg", "jsdelivr", "googleapis"} {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s references an external resource (%q)", path, bad)
			}
		}
	}
}

func TestStaticNotFoundIsError(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/static/nonexistent.js")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("nonexistent asset status = %d, want 404", res.StatusCode)
	}
}

// TestPathTraversalIsNotServed verifies that trying to traverse out of the
// static assets directory does not return file contents. The request
// /static/../server.go is cleaned and redirected by net/http to /server.go,
// which handleIndex 404s (it only serves exactly "/"). The important property
// is that a Go source file is never returned; the mechanism just happens to
// be path cleaning and 404, not an error from handleStatic itself.
func TestPathTraversalIsNotServed(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/static/../server.go")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), "package daemon") {
		t.Error("path traversal returned Go source code")
	}
}

var (
	// apiCallRe finds api("METHOD", "/api/...") calls. app.js writes every
	// call with a literal path template so this can read it.
	apiCallRe = regexp.MustCompile(`api\(\s*"([A-Z]+)"\s*,\s*"(/api/[^"]*)"`)
	// apiLiteralRe finds every /api/ string literal, such as EventSource URLs.
	apiLiteralRe = regexp.MustCompile(`["'\x60](/api/[^"'\x60]*)["'\x60]`)
	placeholder  = regexp.MustCompile(`\{[a-z_]+\}`)
)

// TestAppJSRoutesAreRegistered checks every /api path the web UI uses
// against the mux. A view wired to an endpoint that does not exist yet (the
// 2a-2 policy, MCP and memory routes) would otherwise fall through to the
// index handler's 404 and fail only in a browser.
func TestAppJSRoutesAreRegistered(t *testing.T) {
	src, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	mux := (&Server{}).buildMux()
	matches := func(method, tpl string) bool {
		path, _, _ := strings.Cut(tpl, "?")
		path = placeholder.ReplaceAllString(path, "1")
		if strings.Contains(path, "{") || strings.HasSuffix(path, "/") {
			return false // a concatenated or unfinished path is not checkable
		}
		_, pattern := mux.Handler(httptest.NewRequest(method, path, nil))
		return pattern != "" && pattern != "GET /"
	}

	withMethod := map[string]bool{}
	calls := apiCallRe.FindAllStringSubmatch(string(src), -1)
	for _, m := range calls {
		withMethod[m[2]] = true
		if !matches(m[1], m[2]) {
			t.Errorf("app.js calls %s %s, which no route serves", m[1], m[2])
		}
	}
	literals := apiLiteralRe.FindAllStringSubmatch(string(src), -1)
	for _, m := range literals {
		if withMethod[m[1]] {
			continue
		}
		if !matches(http.MethodGet, m[1]) {
			t.Errorf("app.js references %s, which no GET route serves", m[1])
		}
	}
	// A regex that silently matched nothing would pass everything.
	if len(calls) < 10 {
		t.Errorf("found only %d api(...) calls in app.js; is the pattern still right?", len(calls))
	}
}

func TestAppJSExplainsA401(t *testing.T) {
	body, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "res.status === 401") || !strings.Contains(string(body), "spore web") {
		t.Error("app.js must tell the user to run spore web when the daemon answers 401")
	}
}

func TestModelPanelIsWired(t *testing.T) {
	html, err := web.FS.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{`id="models"`, `id="models-tabs"`, `id="models-body"`, `id="models-note"`, `id="models-refresh"`} {
		if !strings.Contains(string(html), id) {
			t.Errorf("index.html lacks %s", id)
		}
	}
	js, err := web.FS.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"/model": showModels`, `"/api/models?session={id}"`, `"/api/sessions/{id}/model"`, `"/api/routing?session={id}"`} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}
