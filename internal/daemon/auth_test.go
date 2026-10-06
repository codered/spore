package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func bareServer(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	return New(Options{Cfg: cfg, Token: testToken})
}

func TestTokenFileIsCreatedPrivateAndReused(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadOrCreateToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 64 {
		t.Errorf("token length %d, want 64 hex chars", len(a))
	}
	fi, err := os.Stat(filepath.Join(dir, TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	b, _ := LoadOrCreateToken(dir)
	if a != b {
		t.Error("a second load made a new token; it must persist")
	}
	got, err := ReadToken(dir)
	if err != nil || got != a {
		t.Errorf("ReadToken = %q, %v", got, err)
	}
	_ = os.Remove(filepath.Join(dir, TokenFile))
	c, _ := LoadOrCreateToken(dir)
	if c == a || len(c) != 64 {
		t.Error("deleting the file must rotate the token")
	}
}

// Every /api route registered in Handler refuses a request without the
// token and accepts one with it, by header or by cookie.
func TestEveryAPIRouteNeedsTheToken(t *testing.T) {
	s := bareServer(t)
	h := s.Handler()
	if len(s.apiPatterns) < 30 {
		t.Fatalf("only %d api patterns recorded; Handler must register /api routes through api()", len(s.apiPatterns))
	}
	for _, p := range s.apiPatterns {
		method, path, _ := strings.Cut(p, " ")
		path = regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "1")
		for _, auth := range []string{"none", "header", "cookie", "wrong"} {
			// A short deadline: the event-stream routes hold the request open
			// until the client goes away.
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			req := httptest.NewRequest(method, "http://127.0.0.1:7777"+path, nil).WithContext(ctx)
			switch auth {
			case "header":
				req.Header.Set("Authorization", "Bearer "+testToken)
			case "cookie":
				req.AddCookie(&http.Cookie{Name: "spore_token", Value: testToken})
			case "wrong":
				req.Header.Set("Authorization", "Bearer nope")
			}
			rec := httptest.NewRecorder()
			func() {
				// Handlers behind the guard may panic on a bare server with
				// no store; only the guard's answer matters here.
				defer func() { _ = recover() }()
				h.ServeHTTP(rec, req)
			}()
			cancel()
			unauth := rec.Code == http.StatusUnauthorized
			want := auth == "none" || auth == "wrong"
			if unauth != want {
				t.Errorf("%s with %s: status %d", p, auth, rec.Code)
			}
		}
	}
}

// A route added later with mux.HandleFunc directly would skip the guard.
func TestNoAPIRouteIsRegisteredOutsideTheGuard(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	for i, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, " /api/") && strings.Contains(line, "mux.Handle") {
			t.Errorf("server.go:%d registers an /api route outside api(): %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestOpenRoutesStayOpen(t *testing.T) {
	h := bareServer(t).Handler()
	for _, path := range []string{"/healthz", "/static/app.js", "/"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777"+path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d, want 200", path, rec.Code)
		}
	}
}

func TestIndexWithoutACookieSaysRunSporeWeb(t *testing.T) {
	h := bareServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/", nil))
	if !strings.Contains(rec.Body.String(), "spore web") || strings.Contains(rec.Body.String(), `id="transcript"`) {
		t.Errorf("body:\n%s", rec.Body.String())
	}
}

func TestATokenQuerySetsTheCookieAndRedirects(t *testing.T) {
	h := bareServer(t).Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/?token="+testToken, nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("status %d location %q", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || c[0].Name != "spore_token" || c[0].Value != testToken || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Path != "/" {
		t.Errorf("cookie = %+v", c)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://127.0.0.1:7777/?token=wrong", nil))
	if len(rec.Result().Cookies()) != 0 {
		t.Error("a wrong token set a cookie")
	}
}

func TestAForeignHostIsRefused(t *testing.T) {
	h := bareServer(t).Handler()
	for host, want := range map[string]int{
		"evil.example:7777": http.StatusForbidden,
		"evil.example":      http.StatusForbidden,
		"localhost:7777":    http.StatusOK,
		"127.0.0.1:9999":    http.StatusOK,
		"[::1]:7777":        http.StatusOK,
	} {
		req := httptest.NewRequest("GET", "http://x/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %s: %d, want %d", host, rec.Code, want)
		}
	}
}

func TestAnEmptyServerTokenRefusesEverything(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	h := New(Options{Cfg: cfg}).Handler()
	req := httptest.NewRequest("GET", "http://127.0.0.1:7777/api/sessions", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", rec.Code)
	}
}
