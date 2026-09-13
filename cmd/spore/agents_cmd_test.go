package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/subagent"
)

func TestFormatAgentsShowsStateAndCost(t *testing.T) {
	got := formatAgents(agentListJSON{Agents: []subagent.Status{
		{ID: "abc123", Prompt: "audit the policy tests", State: "running", CostUSD: 0.021, Started: time.Now()},
		{ID: "def456", Prompt: "summarise the backlog", State: "done", CostUSD: 0.004, Started: time.Now()},
	}})
	for _, want := range []string{"abc123", "audit the policy tests", "running", "def456", "done", "0.0210"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatAgents output is missing %q:\n%s", want, got)
		}
	}
}

func TestFormatAgentsWhenThereAreNone(t *testing.T) {
	got := formatAgents(agentListJSON{})
	if !strings.Contains(got, "no sub-agents") {
		t.Errorf("empty listing = %q, want it to say there are none", got)
	}
}

// /agents must be consumed in the non-TTY loop too: a command that worked
// only in the TUI was one of the defects found after the last commands
// shipped.
func TestRunPlainSlashListsAgentsWithoutPostingAMessage(t *testing.T) {
	var methods []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method)
		if r.URL.Path != "/api/sessions/s1/agents" {
			t.Errorf("path = %q, want the agents endpoint", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(agentListJSON{Agents: []subagent.Status{{
			ID: "abc123", Prompt: "audit", State: "done", Started: time.Now(),
		}}})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/agents", &out)
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("/agents was not intercepted")
	}
	if len(methods) != 1 || methods[0] != http.MethodGet {
		t.Fatalf("methods = %v, want one GET and no message POST", methods)
	}
	if !strings.Contains(out.String(), "abc123") {
		t.Fatalf("output = %q, want the sub-agent id", out.String())
	}
}

func TestSlashHintOffersAgents(t *testing.T) {
	ui := newChatUI("s1", "http://127.0.0.1/#s1", false)
	if got := ui.renderSlashHint("/ag"); !strings.Contains(got, "/agents") {
		t.Errorf("hint for /ag = %q, want it to offer /agents", got)
	}
}
