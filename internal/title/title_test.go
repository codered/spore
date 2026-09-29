package title

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
)

func newTitler(t *testing.T, turns ...provider.ScriptTurn) (*Titler, *provider.Script) {
	t.Helper()
	sc := provider.NewScript(turns...)
	reg := provider.NewRegistry()
	reg.Register("script", sc, provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, "script/fake")
	if err != nil {
		t.Fatal(err)
	}
	return New(reg, rt), sc
}

func TestTitleCleansTheModelReply(t *testing.T) {
	tr, sc := newTitler(t, provider.ScriptTurn{Text: "<think>the user wants a fix</think>\n  Title: \"Fix flaky scheduler test.\"\nsecond line"})
	got, err := tr.Title(context.Background(), "the scheduler test fails one run in ten, can you look?")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Fix flaky scheduler test" {
		t.Errorf("Title = %q, want %q", got, "Fix flaky scheduler test")
	}
	reqs := sc.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Messages[0].Blocks[0].Text, "scheduler test fails") {
		t.Errorf("the request did not carry the message: %+v", reqs)
	}
}

func TestTitleRejectsAnEmptyReply(t *testing.T) {
	tr, _ := newTitler(t, provider.ScriptTurn{Text: "<think>hmm</think>  \n"})
	if _, err := tr.Title(context.Background(), "hello"); err == nil {
		t.Error("an empty reply must be an error, so the caller falls back")
	}
}

func TestTitlePassesProviderErrors(t *testing.T) {
	tr, _ := newTitler(t, provider.ScriptTurn{Err: errors.New("offline")})
	if _, err := tr.Title(context.Background(), "hello"); err == nil {
		t.Error("a provider error must reach the caller")
	}
}

func TestClean(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Fix login bug", "Fix login bug"},
		{"**Fix login bug**", "Fix login bug"},
		{"# Weather in Fremont", "Weather in Fremont"},
		{"'Plan a trip'", "Plan a trip"},
		{"title: Plan a trip.", "Plan a trip"},
		{"<think>\nlong\n</think>\n\nPlan a trip", "Plan a trip"},
		{"<think>unterminated reasoning", ""},
		{strings.Repeat("word ", 30), strings.TrimSpace(strings.Repeat("word ", 12)) + "…"},
	} {
		if got := Clean(tc.in); got != tc.want {
			t.Errorf("Clean(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFallback(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  what's the weather in fremont?\nand tomorrow", "what's the weather in fremont?"},
		{"", ""},
		{strings.Repeat("x", 80), strings.Repeat("x", 49) + "…"},
	} {
		if got := Fallback(tc.in); got != tc.want {
			t.Errorf("Fallback(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlaceholder(t *testing.T) {
	for _, s := range []string{"", "chat", "web", " Chat ", "new chat"} {
		if !Placeholder(s) {
			t.Errorf("Placeholder(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"Fix login bug", "chatting about go"} {
		if Placeholder(s) {
			t.Errorf("Placeholder(%q) = true, want false", s)
		}
	}
}
