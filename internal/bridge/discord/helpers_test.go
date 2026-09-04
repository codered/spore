package discord

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
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
// Passes nil for the learn callback (disables rule learning).
func buildTestGuard(cfg *config.Config, st *store.Store, approver policy.Approver) (*policy.Guard, error) {
	return buildTestGuardWithLearnCallback(cfg, st, approver, nil)
}

// buildTestGuardWithLearnCallback is like buildTestGuard but accepts a learn callback.
func buildTestGuardWithLearnCallback(cfg *config.Config, st *store.Store, approver policy.Approver, learn func(policy.Decision, string) error) (*policy.Guard, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(
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
	return policy.NewGuard(reg, engine, approver, st, learn), nil
}

// newDaemonWithScriptedProvider boots a real daemon over a real store, a real
// guard and a real policy engine, with provider.Script standing in for the
// model. The construction order is load-bearing and is the same order
// cmd/spore/wire.go uses: the guard needs the server's approver, and the
// server needs the guard, so the server is built first with no agent and the
// agent is attached once its tools exist.
func newDaemonWithScriptedProvider(t *testing.T, cfg *config.Config, turns []provider.ScriptTurn) (*daemon.Server, *store.Store) {
	t.Helper()
	return newDaemonWithScriptedProviderAndLearn(t, cfg, turns, nil)
}

// newDaemonWithScriptedProviderAndLearn is like newDaemonWithScriptedProvider but
// accepts a learn callback for recording rule learning attempts.
func newDaemonWithScriptedProviderAndLearn(t *testing.T, cfg *config.Config, turns []provider.ScriptTurn, learn func(policy.Decision, string) error) (*daemon.Server, *store.Store) {
	t.Helper()
	st := openTestStore(t)

	srv := daemon.New(daemon.Options{Store: st, Cfg: cfg})

	preg := provider.NewRegistry()
	preg.Register("fake", provider.NewScript(turns...), provider.ProviderPrice{})
	rt, err := router.New(cfg.Routes, cfg.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := buildTestGuardWithLearnCallback(cfg, st, srv.Approver(), learn)
	if err != nil {
		t.Fatal(err)
	}
	srv.Attach(agent.New(st, preg, rt, cfg, guard), guard)
	t.Cleanup(srv.Close)
	return srv, st
}

// scriptShellThenText replays one shell_exec call, then a closing sentence
// once its result comes back.
var scriptShellThenText = []provider.ScriptTurn{
	{ToolCalls: []provider.Block{{
		Type: provider.BlockToolUse, ID: "tu1", Name: "shell_exec",
		Input: []byte(`{"cmd":"ls"}`),
	}}},
	{Text: "done"},
}

// writeFile is a test helper that writes content to a file and fails the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
