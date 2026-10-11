package companion

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// Channel delivers an outreach somewhere the user will see it. The Discord
// bridge is one; with none, the companion session itself is the terminal
// channel.
type Channel interface {
	// Name is the outreach channel recorded for a successful send.
	Name() string
	// Send delivers o, already recorded in sessionID, to the user.
	Send(ctx context.Context, sessionID string, o store.Outreach) error
}

// LLMReflector makes the reflection call on the companion call site.
type LLMReflector struct {
	Registry *provider.Registry
	Router   *router.Router
	// MaxTokens caps the reply. A thinking model spends part of it before
	// the JSON, so it is the chat reply cap, not a small fixed number.
	MaxTokens int
}

// Reflect sends one system prompt and one user message and returns the
// reply's text.
func (r LLMReflector) Reflect(ctx context.Context, system, input string) (string, error) {
	ref := r.Router.Model(router.SiteCompanion)
	p, model, price, err := r.Registry.Resolve(ref)
	if err != nil {
		return "", err
	}
	_, span := sporetrace.StartLLM(ctx, router.SiteCompanion, ref)
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    []provider.Block{{Type: provider.BlockText, Text: system}},
		MaxTokens: r.MaxTokens,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: input}},
		}},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return "", fmt.Errorf("companion provider %s: %w", ref, err)
	}
	var text string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			span.RecordError(ev.Err)
			span.End()
			return "", ev.Err
		}
	}
	sporetrace.EndLLM(span, input, text, usage, price.Cost(usage))
	return text, nil
}
