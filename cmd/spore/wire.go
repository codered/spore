package main

import (
	"fmt"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/anthropic"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// buildAgent turns configuration into a wired agent. Plan 1 registers no
// tools, so the agent runs text-only turns; Plan 2 passes a real ToolRunner.
func buildAgent(cfg *config.Config, st *store.Store) (*agent.Agent, error) {
	reg := provider.NewRegistry()
	for name, pc := range cfg.Providers {
		price := provider.ProviderPrice{In: pc.PriceIn, Out: pc.PriceOut}
		switch pc.Kind {
		case "anthropic":
			reg.Register(name, anthropic.New(pc.BaseURL, pc.APIKey, nil), price)
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
	return agent.New(st, reg, rt, cfg, nil), nil
}
