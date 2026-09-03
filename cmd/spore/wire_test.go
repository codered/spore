package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// allowApprover is the simplest Approver stub: every ask is approved once,
// with no scope persisted beyond the single call. It exists only so the
// end-to-end wiring test below can get past the "memory" tool's `ask`
// policy without a terminal attached.
type allowApprover struct{}

func (allowApprover) Ask(context.Context, policy.Ask) (policy.Answer, error) {
	return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
}

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

// The whole feature rests on buildAgent handing ONE *memory.Cache instance to
// both the memory tool and Agent.Facts. This test proves that end to end
// through the real construction path, not by attaching a cache by hand: it
// writes a fact through the actual "memory" tool the agent's guard runs, then
// asserts the very next Snapshot sees it. If a future change gave the tool
// and the agent two different cache instances, the write would still land on
// disk and the tool would still report success — the fact just would not
// reach the model until a process restart — and every other test in this
// package or internal/agent would keep passing, because none of them go
// through buildAgent's own construction of the cache.
func TestFactWrittenThroughMemoryToolReachesNextSnapshot(t *testing.T) {
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

	a, host, err := buildAgent(cfg, st, allowApprover{})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if host != nil {
		defer host.Close()
	}

	ctx := context.Background()
	sid, err := a.Store.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}

	// "memory" is `ask` under the local profile in the default policy, so the
	// call needs a session and profile on the context the way a real turn's
	// dispatch would attach one, and it needs the allowApprover above to get
	// past the ask.
	runCtx := policy.WithSession(ctx, sid, policy.ProfileLocal)
	call := provider.Block{
		Type: provider.BlockToolUse,
		ID:   "call-1",
		Name: "memory",
		Input: json.RawMessage(`{"op":"write","name":"prefers-tabs","description":"formatting preference",` +
			`"type":"user","body":"written through the memory tool"}`),
	}
	res := a.Tools.Run(runCtx, call)
	if res.IsError {
		t.Fatalf("memory tool call failed: %s", res.Content)
	}

	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var found bool
	for _, f := range snap.Facts {
		if f.Name == "prefers-tabs" && f.Body == "written through the memory tool" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fact written through the memory tool did not reach the next Snapshot: %+v", snap.Facts)
	}
}
