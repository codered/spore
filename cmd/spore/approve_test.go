package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/policy"
)

func ask(t *testing.T, input string) (policy.Answer, string, error) {
	t.Helper()
	var out bytes.Buffer
	ap := terminalApprover{lines: bufio.NewScanner(strings.NewReader(input)), out: &out}
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
	// Closing input must deny, never allow.
	got, _, err := ask(t, "")
	if err == nil && got.Allow {
		t.Error("EOF was treated as approval")
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
