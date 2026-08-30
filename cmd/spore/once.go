package main

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// stream prints one turn's events and returns the turn error, if any. When
// showCost is set the per-turn footer also carries the attributed USD cost.
func stream(ch <-chan agent.Event, showCost bool) error {
	for ev := range ch {
		switch ev.Type {
		case agent.EvText:
			fmt.Print(ev.Text)
		case agent.EvToolCall:
			fmt.Printf("\n  → %s %s\n", ev.Block.Name, string(ev.Block.Input))
		case agent.EvToolResult:
			fmt.Printf("  ← %d bytes\n", len(ev.Block.Content))
		case agent.EvTurnDone:
			cost := ""
			if showCost {
				cost = fmt.Sprintf(" · $%.4f", ev.Cost)
			}
			fmt.Printf("\n\n[%s · %d in / %d out%s]\n",
				ev.Model, ev.Usage.InputTokens, ev.Usage.OutputTokens, cost)
		case agent.EvError:
			return ev.Err
		}
	}
	return nil
}

func cmdOnce(ctx context.Context, cfg *config.Config, st *store.Store, prompt string) error {
	a, err := buildAgent(cfg, st)
	if err != nil {
		return err
	}
	sid, err := st.CreateSession(ctx, prompt)
	if err != nil {
		return err
	}
	ch, err := a.Run(ctx, sid, prompt)
	if err != nil {
		return err
	}
	return stream(ch, cfg.ShowCost)
}
