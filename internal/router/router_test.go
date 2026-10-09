package router

import (
	"slices"
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

func TestSubagentIsAValidSite(t *testing.T) {
	if !ValidSite(SiteSubagent) {
		t.Error("ValidSite(SiteSubagent) = false, want true")
	}
}

func TestRouteSubagentToItsOwnModel(t *testing.T) {
	r, err := New([]config.Route{{When: "subagent", Model: "small"}}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteSubagent); got != "small" {
		t.Errorf("Model(subagent) = %q, want small", got)
	}
	if got := r.Model(SiteChat); got != "big" {
		t.Errorf("Model(chat) = %q, want big -- the subagent rule must not match chat", got)
	}
}

func TestOverrideWinsAndDefaultIgnoresIt(t *testing.T) {
	r, err := New([]config.Route{{When: "title", Model: "a/routed"}}, "a/default")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetOverride(SiteTitle, "b/picked"); err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteTitle); got != "b/picked" {
		t.Fatalf("Model(title) = %q, want the override", got)
	}
	if got := r.Default(SiteTitle); got != "a/routed" {
		t.Fatalf("Default(title) = %q, want the hand-written route", got)
	}
	if err := r.SetOverride(SiteTitle, ""); err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteTitle); got != "a/routed" {
		t.Fatalf("after clearing, Model(title) = %q, want the route", got)
	}
}

func TestOverrideRefusesPerSessionSites(t *testing.T) {
	r, _ := New(nil, "a/default")
	for _, site := range []string{SiteChat, SiteSubagent, "nope"} {
		if err := r.SetOverride(site, "b/x"); err == nil {
			t.Errorf("SetOverride(%q) succeeded, want an error", site)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	r, _ := New([]config.Route{{When: "compaction|subagent", Model: "a/x"}}, "a/default")
	if !r.RuleMatches(SiteSubagent) {
		t.Error("RuleMatches(subagent) = false, want true")
	}
	if r.RuleMatches(SiteChat) {
		t.Error("RuleMatches(chat) = true, want false")
	}
}

func TestOverridesAreSafeConcurrently(t *testing.T) {
	r, _ := New(nil, "a/default")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = r.SetOverride(SiteTitle, "b/x")
			_ = r.SetOverride(SiteTitle, "")
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = r.Model(SiteTitle)
	}
	<-done
}

func TestSitesOrder(t *testing.T) {
	want := []string{SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteSubagent}
	if !slices.Equal(Sites, want) {
		t.Fatalf("Sites = %v, want %v", Sites, want)
	}
	for _, s := range GlobalSites {
		if !IsGlobalSite(s) {
			t.Errorf("IsGlobalSite(%q) = false", s)
		}
	}
	if IsGlobalSite(SiteChat) || IsGlobalSite(SiteSubagent) {
		t.Error("chat and subagent are per-session, not global")
	}
}
