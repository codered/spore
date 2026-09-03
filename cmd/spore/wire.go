package main

import (
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/bridge/discord"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	mcphost "github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/anthropic"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
	"github.com/codered/spore/internal/tool/schedule"
	"github.com/codered/spore/internal/tool/shell"
	"github.com/codered/spore/internal/tool/web"
)

// buildTools assembles the registry, the policy engine and the guard that
// wraps them. It also builds the MCP host and attaches it to the registry as
// a dynamic source; the host is returned because its lifecycle belongs to the
// caller — serve supervises it, and everything else closes it.
func buildTools(cfg *config.Config, st *store.Store, approver policy.Approver) (*policy.Guard, *mcphost.Host, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.Workspace, cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(cfg.Policy.Workspace,
		time.Duration(cfg.Shell.TimeoutSeconds)*time.Second, cfg.Policy.MaxOutput))
	tools = append(tools, web.New(cfg.Web, cfg.Policy.MaxOutput)...)
	tools = append(tools, schedule.New(st)...)
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

// buildAgent turns configuration into a wired agent. Plan 1 registers no
// tools, so the agent runs text-only turns; Plan 2 passes a real ToolRunner.
func buildAgent(cfg *config.Config, st *store.Store, approver policy.Approver) (*agent.Agent, *mcphost.Host, error) {
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
				return nil, nil, fmt.Errorf("provider %q: base_url is required for kind %q", name, pc.Kind)
			}
			reg.Register(name, openaicompat.New(pc.BaseURL, pc.APIKey, nil), price)
		default:
			return nil, nil, fmt.Errorf("provider %q: unknown kind %q (want anthropic or openai)", name, pc.Kind)
		}
	}
	rt, err := router.New(cfg.Routes, cfg.DefaultModel)
	if err != nil {
		return nil, nil, err
	}
	tools, host, err := buildTools(cfg, st, approver)
	if err != nil {
		return nil, nil, err
	}
	return agent.New(st, reg, rt, cfg, tools), host, nil
}

// buildServer wires the daemon. The ordering here is load-bearing: the guard
// needs the daemon's approver, and the daemon needs the guard, so the server
// is constructed first with no agent, and the agent is attached once its
// tools have been built around the server's broker.
func buildServer(cfg *config.Config, st *store.Store) (*daemon.Server, *mcphost.Host, error) {
	srv := daemon.New(daemon.Options{Store: st, Cfg: cfg})
	a, host, err := buildAgent(cfg, st, srv.Approver())
	if err != nil {
		return nil, nil, err
	}
	guard, ok := a.Tools.(*policy.Guard)
	if !ok {
		return nil, nil, fmt.Errorf("internal: agent tools are %T, want *policy.Guard", a.Tools)
	}
	srv.Attach(a, guard)
	return srv, host, nil
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
		Cfg: d, Client: client, Turns: srv,
		Store: srv.Store(), Broker: srv.Broker(), Guard: srv.Guard(),
	})
}
