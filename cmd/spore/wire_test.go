package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/sqlitefts"
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

	a, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
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

	if _, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout}); err == nil {
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

	guard, host, err := buildTools(cfg, st, facts, sqlitefts.New(st.DB()), nil)
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
		return engine.Evaluate(policy.Session{Profile: p}, policy.Call{Tool: name, Args: json.RawMessage(`{}`)}).Decision
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

	a, host, _, err := buildAgent(cfg, st, allowApprover{})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if host != nil {
		defer host.Close()
	}

	ctx := context.Background()
	sid, err := a.Store.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}

	// "memory" is `ask` under the local profile in the default policy, so the
	// call needs a session and profile on the context the way a real turn's
	// dispatch would attach one, and it needs the allowApprover above to get
	// past the ask.
	runCtx := policy.WithSession(ctx, policy.Session{ID: sid, Profile: policy.ProfileLocal})
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

// An unreadable fact directory (permission denied, an unmounted volume) is
// not evidence the facts are gone, so startup must neither fail nor wipe the
// index that a previous, successful load already populated.
func TestBuildAgentLeavesFactIndexAloneWhenFactDirIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permission bits, so the directory would still be readable")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "prefers-tabs.md"),
		[]byte("---\nname: prefers-tabs\ndescription: formatting\ntype: user\n---\n\nTabs, always.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
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

	// First startup: the directory is readable, so the fact gets indexed
	// the ordinary way.
	_, host, _, err := buildAgent(cfg, st, allowApprover{})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if host != nil {
		host.Close()
	}

	memDir := filepath.Join(dir, "memory")
	if err := os.Chmod(memDir, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore permissions so t.TempDir()'s own cleanup can remove the
	// directory; this Cleanup was registered after t.TempDir()'s, so it
	// runs first (Cleanup is LIFO).
	t.Cleanup(func() { os.Chmod(memDir, 0o700) })

	// Second startup: the directory is unreadable. Spore must still start.
	_, host2, _, err := buildAgent(cfg, st, allowApprover{})
	if err != nil {
		t.Fatalf("buildAgent failed to start with an unreadable fact directory: %v", err)
	}
	if host2 != nil {
		defer host2.Close()
	}

	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("fact index was cleared even though the fact directory could not be read:\n%s", out)
	}
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildRecallDefaultsToSQLiteFTSWithNoMirror(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	backend, m, err := buildRecall(config.Default(), st, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	if backend == nil {
		t.Fatal("no backend")
	}
	if m != nil {
		t.Error("the default backend started a mirror; there is nothing to mirror to")
	}
	status, err := backend.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Backend != "sqlitefts" {
		t.Errorf("backend %q, want sqlitefts", status.Backend)
	}
}

func TestBuildRecallWrapsWeaviateInAFallback(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Recall.Backend = config.RecallWeaviate
	cfg.Recall.URL = "http://127.0.0.1:1" // nothing listens here

	start := time.Now()
	backend, m, err := buildRecall(cfg, st, quietLogger())
	if err != nil {
		t.Fatal(err)
	}
	// Wiring happens while the daemon starts, so a sidecar that is down must
	// not delay startup.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("buildRecall took %s with the vector store down", elapsed)
	}
	if m == nil {
		t.Error("no mirror was built for a mirrored backend")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The whole point of the fallback: a dead vector store still searches.
	if _, err := backend.Search(ctx, recall.Query{Text: "anything"}); err != nil {
		t.Errorf("search failed with the vector store down: %v", err)
	}
	status, err := backend.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Degraded {
		t.Error("status is healthy with the vector store down")
	}
	if status.Reason == "" {
		t.Error("degraded with no reason")
	}
}

func TestBuildRecallRejectsAnUnusableWeaviateURL(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.Recall.Backend = config.RecallWeaviate
	cfg.Recall.URL = "not-a-url"
	if _, _, err := buildRecall(cfg, st, quietLogger()); err == nil {
		t.Fatal("an unusable url wired without error")
	}
}

func TestCreateSessionSendsTheWorkspace(t *testing.T) {
	var got struct {
		Title     string `json:"title"`
		Workspace string `json:"workspace"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]string{"id": "s1"})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}
	if _, err := c.createSession(context.Background(), "chat", "/ws/a"); err != nil {
		t.Fatal(err)
	}
	if got.Workspace != "/ws/a" {
		t.Fatalf("workspace sent = %q, want /ws/a", got.Workspace)
	}
}

func TestOpenStoreBackfillsEmptyWorkspaces(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	ceiling := filepath.Join(dir, "ceiling")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+ceiling+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Create a store and manually insert a session with empty workspace
	// (simulating a session written before stage 6).
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"
	now := time.Now().UTC().Format(timeFormat)
	_, err = st.DB().ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		"s1", "test", "", now, now)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	st.Close()

	// Verify the session has an empty workspace.
	st, err = store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	sess, found, err := st.Session(ctx, "s1")
	if err != nil || !found {
		t.Fatalf("Session not found after insert: %v", err)
	}
	if sess.Workspace != "" {
		t.Fatalf("session workspace before backfill = %q, want empty", sess.Workspace)
	}
	st.Close()

	// Call openStore, which should backfill the empty workspace.
	st, err = openStore(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// Verify the session now has the ceiling workspace.
	sess, found, err = st.Session(ctx, "s1")
	if err != nil || !found {
		t.Fatalf("Session not found after backfill: %v", err)
	}
	if sess.Workspace != ceiling {
		t.Fatalf("session workspace after backfill = %q, want %q", sess.Workspace, ceiling)
	}
}
