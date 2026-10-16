package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tui"
)

func TestViewErrMaps503ToUnavailableAndSendsTheClientName(t *testing.T) {
	var gotClient string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClient = r.Header.Get("X-Spore-Client")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"no MCP servers configured"}`))
	}))
	defer ts.Close()
	c := newClient(strings.TrimPrefix(ts.URL, "http://"))
	c.name = "tui"
	_, err := tuiBackend{c: c}.MCP(context.Background())
	var u tui.Unavailable
	if !errors.As(err, &u) || u.Msg != "no MCP servers configured" {
		t.Fatalf("err = %#v, want Unavailable with the daemon's message", err)
	}
	if gotClient != "tui" {
		t.Errorf("X-Spore-Client = %q, want tui", gotClient)
	}
}

func TestViewErrKeepsARoutesOwn404Message(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"only rules added with p can be revoked here"}`))
	}))
	defer ts.Close()
	err := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}.Revoke(context.Background(), "allow", "x")
	if err == nil || errors.Is(err, tui.ErrOlderDaemon) || !strings.Contains(err.Error(), "only rules added with p can be revoked here") {
		t.Fatalf("err = %v", err)
	}
}

// The daemon answers a rollback 200 even when an edit could not be undone,
// listing it under "failed". A rule that is still in force must not be
// reported as rolled back.
func TestRollbackRoundReportsWhatWasNotUndone(t *testing.T) {
	body := `{"round_id":"approval-1","rolled_back":[],"stale":[],"failed":[{"kind":"policy.allow","target":"fs_write(path matches /ws/a/**)"}]}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()
	b := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}
	err := b.RollbackRound(context.Background(), "s1", "approval-1")
	if err == nil || !strings.Contains(err.Error(), "fs_write(path matches /ws/a/**)") {
		t.Fatalf("failed rollback: err = %v, want one naming the rule", err)
	}

	body = `{"round_id":"approval-1","rolled_back":[],"stale":[{"kind":"policy.allow","target":"fs_write(path matches /ws/a/**)"}],"failed":[]}`
	err = b.RollbackRound(context.Background(), "s1", "approval-1")
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("stale-only rollback: err = %v, want a note that nothing was left to undo", err)
	}

	body = `{"round_id":"approval-1","rolled_back":[{"kind":"policy.allow","target":"x"}],"stale":[],"failed":[]}`
	if err := b.RollbackRound(context.Background(), "s1", "approval-1"); err != nil {
		t.Fatalf("clean rollback: err = %v", err)
	}
}

// Only a 404 or 409 from the resolve endpoint means the approval is gone; a
// refusal or a server failure is a real failure and keeps the card.
func TestTUIBackendResolveGone(t *testing.T) {
	for _, tc := range []struct {
		status int
		gone   bool
	}{{http.StatusConflict, true}, {http.StatusNotFound, true}, {http.StatusInternalServerError, false}} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		}))
		c := newClient(strings.TrimPrefix(ts.URL, "http://"))
		err := tuiBackend{c: c}.Resolve(context.Background(), "s1", 1, policy.Answer{Allow: true, Scope: policy.ScopeOnce})
		ts.Close()
		if err == nil {
			t.Fatalf("status %d: no error", tc.status)
		}
		if got := errors.Is(err, tui.ErrApprovalGone); got != tc.gone {
			t.Fatalf("status %d: ErrApprovalGone = %v, want %v (err %v)", tc.status, got, tc.gone, err)
		}
	}
}

func TestTUIBackendStartGrantSendsTheRootAndReadsExpiry(t *testing.T) {
	var gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.Method + " " + r.URL.Path
		buf := new(strings.Builder)
		_, _ = io.Copy(buf, r.Body)
		gotBody = buf.String()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"expires_at":"2026-10-16T12:15:00Z"}`))
	}))
	defer ts.Close()
	b := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}
	at, err := b.StartGrant(context.Background(), "root1", "15m")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "POST /api/sessions/root1/grant" || !strings.Contains(gotBody, `"duration":"15m"`) {
		t.Errorf("request = %s %s, want POST to the root's grant with duration 15m", gotPath, gotBody)
	}
	if want := time.Date(2026, 10, 16, 12, 15, 0, 0, time.UTC); !at.Equal(want) {
		t.Errorf("expiry = %v, want %v", at, want)
	}
}

func TestTUIBackendStartGrantPassesTheDaemonsRefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"duration must be one of 5m, 15m, 30m, 1h"}`))
	}))
	defer ts.Close()
	b := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}
	_, err := b.StartGrant(context.Background(), "root1", "2h")
	if err == nil || !strings.Contains(err.Error(), "duration must be one of 5m, 15m, 30m, 1h") {
		t.Fatalf("err = %v, want the daemon's message", err)
	}
}

func TestTUIBackendGrantDurationsReadsPolicyAndOlderDaemonIsEmpty(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rules":[],"grant_durations":["5m","1h"]}`))
	}))
	defer ts.Close()
	b := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}
	got, err := b.GrantDurations(context.Background())
	if err != nil || strings.Join(got, ",") != "5m,1h" {
		t.Fatalf("durations = %v, %v, want 5m,1h", got, err)
	}

	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer old.Close()
	_, err = tuiBackend{c: newClient(strings.TrimPrefix(old.URL, "http://"))}.GrantDurations(context.Background())
	if !errors.Is(err, tui.ErrOlderDaemon) {
		t.Fatalf("err = %v, want ErrOlderDaemon from a daemon without /api/policy grants", err)
	}
}
