package discord

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
	"github.com/codered/spore/internal/tool/shell"
)

// newLoadedGuard builds a real policy guard over a real store, from config
// read off disk. config.Load, never config.Default: Load is what appends the
// baseline deny rules, and a guard built without them will happily allow a
// read of /etc/passwd through an fs_read allow rule — so a security test
// written on Default passes while proving nothing.
func newLoadedGuard(t *testing.T) (*store.Store, *policy.Guard) {
	t.Helper()
	cfg := loadPolicyConfig(t, t.TempDir())
	st := openTestStore(t)
	guard, err := buildTestGuard(cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st, guard
}

// openTestStore creates a new test store in a temporary directory.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// loadPolicyConfig creates a temporary config file with policy settings
// and returns the loaded config.
func loadPolicyConfig(t *testing.T, workspace string) *config.Config {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	configContent := `default_model = "fake/model"

[providers.fake]
kind = "openai"
base_url = "http://localhost:9999"

[policy]
workspace = "` + workspace + `"
default = "ask"
allow = ["fs_read"]
ask = ["shell_exec"]
`
	if err := os.WriteFile(cfgPath, []byte(configContent), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// buildTestGuard mirrors cmd/spore/wire.go's buildTools. It exists because
// that function lives in package main and cannot be imported.
func buildTestGuard(cfg *config.Config, st *store.Store, approver policy.Approver) (*policy.Guard, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.Workspace, cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(cfg.Policy.Workspace,
		time.Duration(cfg.Shell.TimeoutSeconds)*time.Second, cfg.Policy.MaxOutput))
	for _, tl := range tools {
		if err := reg.Register(tl); err != nil {
			return nil, err
		}
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return nil, err
	}
	return policy.NewGuard(reg, engine, approver, st, nil), nil
}
