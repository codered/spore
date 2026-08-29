package router

import (
	"testing"

	"github.com/codered/spore/internal/config"
)

func TestFirstMatchWinsAndDefaultApplies(t *testing.T) {
	r, err := New([]config.Route{
		{When: "compaction|title|classify", Model: "ollama/qwen3:8b"},
		{When: "chat", Model: "anthropic/claude-opus-5"},
	}, "anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]string{
		SiteCompaction: "ollama/qwen3:8b",
		SiteTitle:      "ollama/qwen3:8b",
		SiteClassify:   "ollama/qwen3:8b",
		SiteChat:       "anthropic/claude-opus-5",
		"unmatched":    "anthropic/claude-sonnet-5",
	}
	for site, want := range cases {
		if got := r.Model(site); got != want {
			t.Errorf("Model(%q) = %q, want %q", site, got, want)
		}
	}
}

func TestPatternsAreAnchored(t *testing.T) {
	r, err := New([]config.Route{{When: "chat", Model: "ollama/qwen3:8b"}}, "anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// "chatty" must not match a rule written for "chat".
	if got := r.Model("chatty"); got != "anthropic/claude-opus-5" {
		t.Errorf("Model(\"chatty\") = %q, want the default", got)
	}
}

func TestNewRejectsBadPattern(t *testing.T) {
	if _, err := New([]config.Route{{When: "(unclosed", Model: "ollama/q"}}, "anthropic/m"); err == nil {
		t.Fatal("New accepted an invalid regexp")
	}
}

func TestValidSite(t *testing.T) {
	if !ValidSite(SiteChat) || ValidSite("embed") {
		t.Error("ValidSite is wrong: chat must be valid, embed must not")
	}
}
