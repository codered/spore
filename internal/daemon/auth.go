package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// TokenFile is the daemon's credential, in the data directory. Every /api
// route requires it: without one, anything that can reach the loopback
// address -- including a model's approved shell, or web_fetch -- could
// answer approvals, accept policy proposals or revoke learned rules.
const TokenFile = "daemon.token"

const tokenCookie = "spore_token"

// LoadOrCreateToken returns the daemon token, creating it on first start.
// It persists across restarts so a signed-in browser tab survives one;
// deleting the file rotates it.
func LoadOrCreateToken(dataDir string) (string, error) {
	tok, err := ReadToken(dataDir)
	if err == nil {
		return tok, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok = hex.EncodeToString(buf)
	path := filepath.Join(dataDir, TokenFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: dataDir is from the validated config
	if errors.Is(err, os.ErrExist) {
		// Another process created it between the read and the open.
		return ReadToken(dataDir)
	}
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(tok + "\n"); err != nil {
		_ = f.Close()
		return "", err
	}
	return tok, f.Close()
}

// ReadToken reads the token a client sends. An empty or malformed file is
// an error rather than an empty token, so a client never sends "Bearer ".
func ReadToken(dataDir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dataDir, TokenFile)) //nolint:gosec // G304: dataDir is from the validated config
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if len(tok) != 64 {
		return "", fmt.Errorf("%s is malformed; delete it and restart spore", filepath.Join(dataDir, TokenFile))
	}
	return tok, nil
}

// authorized reports whether the request carries the token, by bearer
// header or by the cookie spore web sets. An empty server token authorises
// nothing: a daemon wired without one fails closed.
func (s *Server) authorized(r *http.Request) bool {
	if s.token == "" {
		return false
	}
	got := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		got = strings.TrimPrefix(h, "Bearer ")
	} else if c, err := r.Cookie(tokenCookie); err == nil {
		got = c.Value
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

// checkOrigin refuses requests with an Origin header that doesn't match the
// request's Host. SameSite=Strict is per-site, not per-origin: localhost:3000
// and localhost:7777 are the same site, so a page on one port can get the
// cookie from another and send state-changing requests. Origin check adds
// per-origin CSRF defense. A missing Origin (CLI, curl, same-origin GETs) is
// allowed.
func checkOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		// No Origin header: allowed (CLI, curl, browser same-origin GETs).
		// "null" Origin is used by browsers in sandboxed contexts and
		// cross-origin requests; reject it as it's not a legitimate origin.
		if origin == "null" {
			writeError(w, http.StatusForbidden, "cross-origin request forbidden")
			return false
		}
		return true
	}
	// Origin is present and non-empty; it must match the request host.
	// r.Host is "host:port"; we need to construct the expected origin.
	expectedOrigin := "http://" + r.Host
	if origin != expectedOrigin {
		writeError(w, http.StatusForbidden, "cross-origin request forbidden")
		return false
	}
	return true
}

// requireToken wraps an /api handler.
func (s *Server) requireToken(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkOrigin(w, r) {
			return
		}
		if !s.authorized(r) {
			writeError(w, http.StatusUnauthorized, "missing or wrong daemon token: run `spore web` to sign in a browser")
			return
		}
		h(w, r)
	}
}

// checkHost refuses a request addressed to any host but this machine's
// loopback names or the configured address. DNS rebinding reaches a local
// daemon through a hostname the attacker controls; the port does not
// matter, the hostname does.
func (s *Server) checkHost(next http.Handler) http.Handler {
	configured := ""
	if s.cfg != nil {
		if h, _, err := net.SplitHostPort(s.cfg.Daemon.Addr); err == nil {
			configured = h
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		switch {
		case host == "localhost", host == "127.0.0.1", host == "::1":
		case configured != "" && host == configured:
		default:
			writeError(w, http.StatusForbidden, "host %q is not this daemon", r.Host)
			return
		}
		next.ServeHTTP(w, r)
	})
}
