package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/bridge/discord"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	mcphost "github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/anthropic"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/mirror"
	"github.com/codered/spore/internal/recall/sqlitefts"
	weaviaterecall "github.com/codered/spore/internal/recall/weaviate"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
	"github.com/codered/spore/internal/tool/mem"
	"github.com/codered/spore/internal/tool/schedule"
	"github.com/codered/spore/internal/tool/shell"
	"github.com/codered/spore/internal/tool/web"
	"github.com/codered/spore/internal/workspace"
)

// buildTools assembles the registry, the policy engine and the guard that
// wraps them. It also builds the MCP host and attaches it to the registry as
// a dynamic source; the host is returned because its lifecycle belongs to the
// caller — serve supervises it, and everything else closes it. The fact
// cache is built by the caller (buildAgent needs it for Agent.Facts too) and
// passed in here just to register the two memory tools around it.
func buildTools(cfg *config.Config, st *store.Store, facts *memory.Cache, recallBackend recall.Recall, approver policy.Approver) (*policy.Guard, *mcphost.Host, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(
		time.Duration(cfg.Shell.TimeoutSeconds)*time.Second, cfg.Policy.MaxOutput))
	tools = append(tools, web.New(cfg.Web, cfg.Policy.MaxOutput)...)
	tools = append(tools, schedule.New(st)...)
	tools = append(tools, mem.NewRecallSearch(recallBackend), mem.NewMemory(facts, st))
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return nil, nil, err
		}
	}
	host := mcphost.New(cfg.MCP, cfg.Policy.Workspace, slog.Default())
	if host.Configured() {
		reg.AddSource(host)
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return nil, nil, err
	}
	learn := func(d policy.Decision, rule string) error {
		return config.LearnRule(cfg.Path, string(d), rule)
	}
	return policy.NewGuard(reg, engine, approver, st, learn), host, nil
}

// buildRecall chooses the search backend. sqlitefts is always constructed:
// with weaviate configured it becomes the fallback, so a vector store that is
// down costs semantic ranking and never costs search. The mirror is nil for
// the default backend, because there is nothing to mirror to.
func buildRecall(cfg *config.Config, st *store.Store, log *slog.Logger) (recall.Recall, *mirror.Mirror, error) {
	keyword := sqlitefts.New(st.DB())
	if cfg.Recall.Backend != config.RecallWeaviate {
		return keyword, nil, nil
	}
	vector, err := weaviaterecall.New(cfg.WeaviateURL())
	if err != nil {
		return nil, nil, err
	}
	return recall.NewFallback(vector, keyword, log),
		mirror.New(st, vector, weaviaterecall.Name, log), nil
}

// buildAgent turns configuration into a wired agent. Plan 1 registers no
// tools, so the agent runs text-only turns; Plan 2 passes a real ToolRunner.
func buildAgent(cfg *config.Config, st *store.Store, approver policy.Approver) (*agent.Agent, *mcphost.Host, *mirror.Mirror, error) {
	reg := provider.NewRegistry()
	for name, pc := range cfg.Providers {
		price := provider.ProviderPrice{In: pc.PriceIn, Out: pc.PriceOut}
		switch pc.Kind {
		case "anthropic":
			ws := pc.WorkspaceID
			if ws == "" {
				ws = os.Getenv("ANTHROPIC_WORKSPACE_ID")
			}
			reg.Register(name, anthropic.New(pc.BaseURL, pc.APIKey, ws, nil), price)
		case "openai", "openai-compatible":
			if pc.BaseURL == "" {
				return nil, nil, nil, fmt.Errorf("provider %q: base_url is required for kind %q", name, pc.Kind)
			}
			reg.Register(name, openaicompat.New(pc.BaseURL, pc.APIKey, nil), price)
		default:
			return nil, nil, nil, fmt.Errorf("provider %q: unknown kind %q (want anthropic or openai)", name, pc.Kind)
		}
	}
	rt, err := router.New(cfg.Routes, cfg.DefaultModel)
	if err != nil {
		return nil, nil, nil, err
	}

	// The fact cache is loaded once here; the memory tool reloads it after
	// each write, which is the only way the set changes while spore runs.
	factsDir := filepath.Join(cfg.DataDir, "memory")
	facts := memory.NewCache(factsDir)
	dirUnreadable := false
	for _, err := range facts.Reload() {
		if errors.Is(err, memory.ErrReadDir) {
			// The whole reload failed (permission denied, an unmounted volume);
			// nothing was skipped, and Reload deliberately kept the previously
			// cached set rather than blanking it. The index gets the same
			// treatment below: an unreadable directory is not evidence the
			// facts are gone, so it must not be treated as "the directory is
			// empty" and clear rows that are still valid on disk.
			dirUnreadable = true
			slog.Default().Warn("fact directory unreadable, leaving fact index as it is", "error", err)
			continue
		}
		// A hand-edited fact that will not parse costs one fact and a warning,
		// never a failed startup.
		slog.Default().Warn("skipping malformed fact", "error", err)
	}
	if !dirUnreadable {
		// Drop whatever facts the index still remembers before re-indexing the
		// set just loaded from disk. Facts are file-owned and nothing else
		// removes a row when a file is deleted by hand, so without this a fact
		// deleted while spore was not running -- including a sensitive one --
		// stays searchable across a restart forever.
		if err := st.ClearFactIndex(context.Background()); err != nil {
			slog.Default().Warn("clearing fact index failed", "error", err)
		}
		// Index what was just loaded so a fresh install can search facts before
		// anything is written through the memory tool.
		for _, f := range facts.Facts() {
			if err := st.IndexFact(context.Background(), f.Name, f.Description+"\n"+f.Body); err != nil {
				slog.Default().Warn("indexing fact failed", "fact", f.Name, "error", err)
			}
		}
	}

	recallBackend, mir, err := buildRecall(cfg, st, slog.Default())
	if err != nil {
		return nil, nil, nil, err
	}
	tools, host, err := buildTools(cfg, st, facts, recallBackend, approver)
	if err != nil {
		return nil, nil, nil, err
	}
	a := agent.New(st, reg, rt, cfg, tools)
	a.Facts = facts
	a.Env = workspace.NewDescribers().Describe
	return a, host, mir, nil
}

// buildServer wires the daemon. The ordering here is load-bearing: the guard
// needs the daemon's approver, and the daemon needs the guard, so the server
// is constructed first with no agent, and the agent is attached once its
// tools have been built around the server's broker.
func buildServer(cfg *config.Config, st *store.Store) (*daemon.Server, *mcphost.Host, *mirror.Mirror, error) {
	srv := daemon.New(daemon.Options{Store: st, Cfg: cfg})
	a, host, mir, err := buildAgent(cfg, st, srv.Approver())
	if err != nil {
		return nil, nil, nil, err
	}
	guard, ok := a.Tools.(*policy.Guard)
	if !ok {
		return nil, nil, nil, fmt.Errorf("internal: agent tools are %T, want *policy.Guard", a.Tools)
	}
	srv.Attach(a, guard)
	return srv, host, mir, nil
}

// buildBridge constructs the Discord bridge, or reports (nil, nil) when it is
// not configured. It must be built AFTER the server, because the bridge needs
// the broker and guard the server owns.
func buildBridge(cfg *config.Config, srv *daemon.Server) (*discord.Bridge, error) {
	d := cfg.Bridge.Discord
	if !d.Enabled {
		return nil, nil
	}
	client, err := discord.NewGatewayClient(d.Token)
	if err != nil {
		return nil, err
	}
	return discord.New(discord.Options{
		Cfg: d, Client: client, Turns: srv, Sessions: srv,
		Store: srv.Store(), Broker: srv.Broker(), Guard: srv.Guard(),
	})
}
