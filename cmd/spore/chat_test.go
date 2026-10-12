package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	handled, err := runPlainSlash(context.Background(), c, "s1", "/skills", false, &out)
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
	handled, err := runPlainSlash(context.Background(), nil, "s1", "hello", false, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("ordinary text was intercepted as a command")
	}
}

func TestRunPlainSlashRunsCompanionWithoutPostingAMessage(t *testing.T) {
	var got struct {
		Args []string `json:"args"`
	}
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "paused until Mon\n"})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/companion pause 2h", false, &out)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(paths) != 1 || paths[0] != "POST /api/companion/command" {
		t.Fatalf("requests = %v, want one command POST and no message", paths)
	}
	if strings.Join(got.Args, " ") != "pause 2h" {
		t.Fatalf("args = %q", got.Args)
	}
	if out.String() != "paused until Mon\n" {
		t.Fatalf("out = %q", out.String())
	}
	// Bare /companion sends no arguments, which the daemon reads as status.
	handled, err = runPlainSlash(context.Background(), c, "s1", "/companion", false, &out)
	if err != nil || !handled || len(got.Args) != 0 {
		t.Fatalf("bare /companion: handled=%v err=%v args=%q", handled, err, got.Args)
	}
	// A word that only starts with /companion is an ordinary message.
	if handled, _ := runPlainSlash(context.Background(), c, "s1", "/companionship", false, &out); handled {
		t.Fatal("/companionship was taken as a command")
	}
}
