package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRunPlainSlashListsSkillsWithoutPostingAMessage(t *testing.T) {
	var methods []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/api/sessions/s1/skills" {
			t.Errorf("path = %q, want skills endpoint", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(skillListJSON{Skills: []skillJSON{{
			Name: "review", Description: "review a change", BodyTokens: 42,
		}}})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/skills", &out)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("/skills was not intercepted")
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("methods = %v, want one GET and no message POST", methods)
	}
	if !bytes.Contains(out.Bytes(), []byte("review")) {
		t.Fatalf("output = %q, want skill name", out.String())
	}
}

func TestClearPostsToDedicatedEndpoint(t *testing.T) {
	var method, path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]int{"summary_through": 4})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}
	if err := c.clear(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/api/sessions/s1/clear" {
		t.Fatalf("request = %s %s, want POST /api/sessions/s1/clear", method, path)
	}
}
func TestRunPlainSlashLeavesOrdinaryMessagesAlone(t *testing.T) {
	handled, err := runPlainSlash(context.Background(), nil, "s1", "hello", &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("ordinary text was intercepted as a command")
	}
}
