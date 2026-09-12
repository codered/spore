package main

import (
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// The supervisor the tools launch through must also reach the daemon, or
// /agents lists nothing and the startup sweep has nothing to run on: the
// subsystem would be built and then be unreachable.
func TestBuildServerHandsTheDaemonTheSupervisor(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-x"},
	}

	srv, host, _, err := buildServer(cfg, st)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	defer host.Close()
	if srv.Subagents() == nil {
		t.Fatal("buildServer left the daemon without the sub-agent supervisor")
	}
}
