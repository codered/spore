package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

func TestClientSendPostsToMessagesEndpoint(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	err := c.send(context.Background(), "s1", "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/sessions/s1/messages" {
		t.Fatalf("request = %s %s, want POST /api/sessions/s1/messages", method, path)
	}
}

func TestClientResolvePostsToApprovalsEndpoint(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	err := c.resolve(context.Background(), "s1", 42, policy.Answer{Allow: true, Scope: policy.ScopeSession})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := "/api/sessions/s1/approvals/42"
	if method != http.MethodPost || path != wantPath {
		t.Fatalf("request = %s %s, want POST %s", method, path, wantPath)
	}
}

func TestClientCompactPostsAndReturnsCompactJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/sessions/s1/compact" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(daemon.CompactJSON{Folded: 2, Before: 3, After: 4})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	out, err := c.compact(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Folded != 2 || out.Before != 3 || out.After != 4 {
		t.Errorf("CompactJSON = {Folded:%d,Before:%d,After:%d}, want {Folded:2,Before:3,After:4}", out.Folded, out.Before, out.After)
	}
}

func TestClientGetTranscriptGetsSessionEndpoint(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "active"})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	out, err := c.getTranscript(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || path != "/api/sessions/s1" {
		t.Fatalf("request = %s %s, want GET /api/sessions/s1", method, path)
	}
	if out["status"] != "active" {
		t.Errorf("transcript = %v, want status=active", out)
	}
}

func TestClientStreamFromReturnsErrorOnNonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	err := c.streamFrom(context.Background(), "s1", nil, func(daemon.WireEvent) error { return nil })
	if err == nil {
		t.Fatal("expected error for non-OK status")
	}
}
