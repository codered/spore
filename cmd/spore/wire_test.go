package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
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
