package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
