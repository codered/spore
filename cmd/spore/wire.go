package main

import (
	"fmt"
	"os"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/anthropic"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
	"github.com/codered/spore/internal/tool/shell"
	"github.com/codered/spore/internal/tool/web"
)

// buildTools assembles the registry, the policy engine and the guard that
// wraps them. The agent receives the guard, so no tool call reaches a builtin
// without a policy decision behind it.
func buildTools(cfg *config.Config, st *store.Store) (*policy.Guard, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.Workspace, cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(cfg.Policy.Workspace,
		time.Duration(cfg.Shell.TimeoutSeconds)*time.Second, cfg.Policy.MaxOutput))
	tools = append(tools, web.New(cfg.Web, cfg.Policy.MaxOutput)...)
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return nil, err
		}
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return nil, err
	}
	approver := terminalApprover{lines: stdinLines, out: os.Stdout}
	learn := func(d policy.Decision, rule string) error {
		return config.LearnRule(cfg.Path, string(d), rule)
	}
	return policy.NewGuard(reg, engine, approver, st, learn), nil
}

// buildAgent turns configuration into a wired agent. Plan 1 registers no
// tools, so the agent runs text-only turns; Plan 2 passes a real ToolRunner.
func buildAgent(cfg *config.Config, st *store.Store) (*agent.Agent, error) {
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
				return nil, fmt.Errorf("provider %q: base_url is required for kind %q", name, pc.Kind)
			}
			reg.Register(name, openaicompat.New(pc.BaseURL, pc.APIKey, nil), price)
		default:
			return nil, fmt.Errorf("provider %q: unknown kind %q (want anthropic or openai)", name, pc.Kind)
		}
	}
	rt, err := router.New(cfg.Routes, cfg.DefaultModel)
	if err != nil {
		return nil, err
	}
	tools, err := buildTools(cfg, st)
	if err != nil {
		return nil, err
	}
	return agent.New(st, reg, rt, cfg, tools), nil
}
