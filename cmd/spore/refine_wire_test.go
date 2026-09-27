package main

import (
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/refine"
)

// The tool, the hook and the Refiner are three pieces that each work alone
// and do nothing if buildAgent forgets one.
func TestBuildAgentWiresRefinement(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}
	u := buildAgentAt(t, cfg)
	if _, ok := u.a.Refine.(*refine.Refiner); !ok {
		t.Fatalf("Agent.Refine = %T, want *refine.Refiner", u.a.Refine)
	}
	found := false
	for _, s := range u.a.Tools.Specs() {
		found = found || s.Name == "refine"
	}
	if !found {
		t.Fatal("the refine tool is not registered")
	}
}
