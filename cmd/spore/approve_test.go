package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

func ask(t *testing.T, input string) (policy.Answer, string, error) {
	t.Helper()
	var out bytes.Buffer
	sc := bufio.NewScanner(strings.NewReader(input))
	ap := terminalApprover{lines: scannerLines{sc: sc}, out: &out}
	ans, err := ap.Ask(context.Background(), policy.Ask{
		SessionID: "s1",
		Tool:      "fs_write",
		Args:      json.RawMessage(`{"path":"/ws/a.go"}`),
		Rule:      "fs_write",
		Pattern:   "fs_write(path matches /ws/**)",
	})
	return ans, out.String(), err
}

func TestTerminalApproverReadsAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  policy.Answer
	}{
		{"y\n", policy.Answer{Allow: true, Scope: policy.ScopeOnce}},
		{"n\n", policy.Answer{Allow: false, Scope: policy.ScopeOnce}},
		{"s\n", policy.Answer{Allow: true, Scope: policy.ScopeSession}},
		{"p\n", policy.Answer{Allow: true, Scope: policy.ScopePattern}},
	}
	for _, c := range cases {
		got, _, err := ask(t, c.input)
		if err != nil {
			t.Fatalf("input %q: %v", c.input, err)
		}
		if got != c.want {
			t.Errorf("input %q = %+v, want %+v", c.input, got, c.want)
		}
	}
}

func TestTerminalApproverShowsTheCallAndTheRule(t *testing.T) {
	_, out, err := ask(t, "y\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fs_write", "/ws/a.go", "fs_write(path matches /ws/**)"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt is missing %q:\n%s", want, out)
		}
	}
}

func TestTerminalApproverDeniesOnEOF(t *testing.T) {
	// A non-interactive run (spore once in a pipeline) has no one to ask.
	// Closing input must deny, never allow, and the deny must be an answer
	// that can be sent: an error left the approval pending until it timed out.
	got, _, err := ask(t, "")
	if err != nil {
		t.Fatalf("EOF returned an error instead of a deny: %v", err)
	}
	if want := (policy.Answer{Allow: false, Scope: policy.ScopeOnce}); got != want {
		t.Errorf("EOF = %+v, want %+v", got, want)
	}
}

// With no input, approve must post the deny to the daemon at once. It used
// to print "denying" and post nothing, so the turn sat blocked for the whole
// approval_timeout (5 minutes by default) before the daemon denied it.
func TestApproveWithoutInputPostsADeny(t *testing.T) {
	var path string
	var body struct {
		Allow bool   `json:"allow"`
		Scope string `json:"scope"`
	}
	posted := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, posted = r.URL.Path, true
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}
	ap := terminalApprover{lines: scannerLines{sc: bufio.NewScanner(strings.NewReader(""))}, out: io.Discard}

	approve(context.Background(), c, ap, "s1", daemon.WireEvent{Type: daemon.WireApproval, Tool: "shell_exec", PendingID: 7})

	if !posted {
		t.Fatal("no answer was posted; the approval would wait for its timeout")
	}
	if path != "/api/sessions/s1/approvals/7" || body.Allow {
		t.Errorf("posted %s allow=%v, want a deny to /api/sessions/s1/approvals/7", path, body.Allow)
	}
}

func TestTerminalApproverRepromptsOnGarbage(t *testing.T) {
	got, out, err := ask(t, "what?\nmaybe\ny\n")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Error("a valid answer after two invalid ones was not accepted")
	}
	if strings.Count(out, "[y]es") < 3 {
		t.Errorf("the prompt was not repeated for each invalid answer:\n%s", out)
	}
}

func TestTerminalApproverWithChanLines(t *testing.T) {
	// Verify that chanLines works as a lineSource for the approver.
	// This is the plumbing used by chat to answer approvals on the main
	// goroutine without races.
	ch := make(chan string, 1)
	ch <- "y"
	close(ch)

	var out bytes.Buffer
	ap := terminalApprover{lines: chanLines{ch: ch}, out: &out}
	ans, err := ap.Ask(context.Background(), policy.Ask{
		SessionID: "s1",
		Tool:      "fs_write",
		Args:      json.RawMessage(`{"path":"/ws/a.go"}`),
		Rule:      "fs_write",
		Pattern:   "fs_write(path matches /ws/**)",
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if !ans.Allow {
		t.Error("answer from chanLines was not accepted")
	}
}
