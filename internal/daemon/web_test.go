package daemon

import (
	"io"
	"net/http"
	"strings"
	"testing"
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
	for _, want := range []string{"<title>spore</title>", `id="transcript"`, "/static/app.js", "/static/style.css"} {
		if !strings.Contains(page, want) {
			t.Errorf("index page is missing %q", want)
		}
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct{ path, contentType, needle string }{
		{"/static/app.js", "javascript", "EventSource"},
		{"/static/style.css", "css", "#transcript"},
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

func TestUnknownStaticFileIs404(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/static/../server.go")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("a path traversal out of the embedded assets returned 200")
	}
}
