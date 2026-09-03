package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func TestBuildAgentRegistersConfiguredProviders(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-x", PriceIn: 5, PriceOut: 25},
		"ollama":    {Kind: "openai", BaseURL: "http://localhost:11434/v1"},
	}
	cfg.Routes = []config.Route{{When: "compaction|title|classify", Model: "ollama/qwen3:8b"}}

	a, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if _, model, price, err := a.Registry.Resolve("anthropic/claude-opus-5"); err != nil || model != "claude-opus-5" || price.In != 5 {
		t.Errorf("anthropic not registered correctly: model=%q price=%+v err=%v", model, price, err)
	}
	if _, _, _, err := a.Registry.Resolve("ollama/qwen3:8b"); err != nil {
		t.Errorf("ollama not registered: %v", err)
	}
	if got := a.Router.Model("compaction"); got != "ollama/qwen3:8b" {
		t.Errorf("router not wired: compaction -> %q", got)
	}
}

func TestBuildAgentRejectsUnknownProviderKind(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	defer st.Close()

	cfg := config.Default()
	cfg.DefaultModel = "weird/model"
	cfg.Providers = map[string]config.ProviderConfig{"weird": {Kind: "telepathy"}}

	if _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout}); err == nil {
		t.Fatal("buildAgent accepted an unknown provider kind")
	}
}

// The two memory builtins must actually reach the registry, and the policy
// engine must judge them the way the defaults say. This is built through
// config.Load on a real file because Default() carries no baseline deny.
func TestMemoryToolsAreRegisteredAndGated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	facts := memory.NewCache(filepath.Join(dir, "memory"))
	facts.Reload()

	guard, host, err := buildTools(cfg, st, facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != nil {
		defer host.Close()
	}
	specs := guard.Specs()
	var haveRecall, haveMemory bool
	for _, s := range specs {
		switch s.Name {
		case "recall_search":
			haveRecall = true
		case "memory":
			haveMemory = true
		}
	}
	if !haveRecall || !haveMemory {
		t.Fatalf("memory builtins missing from the registry: %+v", specs)
	}

	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(p policy.Profile, name string) policy.Decision {
		return engine.Evaluate(p, policy.Call{Tool: name, Args: json.RawMessage(`{}`)}).Decision
	}
	if d := decide(policy.ProfileRemote, "memory"); d != policy.DecisionDeny {
		t.Fatalf("remote memory decision = %v, want deny", d)
	}
	if d := decide(policy.ProfileLocal, "memory"); d != policy.DecisionAsk {
		t.Fatalf("local memory decision = %v, want ask", d)
	}
	if d := decide(policy.ProfileLocal, "recall_search"); d != policy.DecisionAllow {
		t.Fatalf("local recall_search decision = %v, want allow", d)
	}
}
