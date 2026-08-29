package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// maxIterations bounds one turn's provider round trips so a model that keeps
// calling tools cannot spin forever.
const maxIterations = 12

type EventType string

const (
	EvText       EventType = "text"
	EvToolCall   EventType = "tool_call"
	EvToolResult EventType = "tool_result"
	EvTurnDone   EventType = "turn_done"
	EvError      EventType = "error"
)

type Event struct {
	Type  EventType
	Text  string
	Block *provider.Block
	Model string
	Usage provider.Usage
	Cost  float64
	Err   error
}

// ToolRunner is the seam Plan 2 fills. The loop knows only that tools have
// specs, can declare themselves read-only, and return a tool_result block.
type ToolRunner interface {
	Specs() []provider.ToolSpec
	ReadOnly(name string) bool
	Run(ctx context.Context, call provider.Block) provider.Block
}

type Agent struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	Tools    ToolRunner
}

func New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, tools ToolRunner) *Agent {
	return &Agent{Store: st, Registry: reg, Router: rt, Cfg: cfg, Tools: tools}
}

// Snapshot reads the session's persisted state into the value context
// assembly consumes. Facts stay empty until Plan 5 adds the memory layer.
func (a *Agent) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	rows, err := a.Store.Messages(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	summary, through, err := a.Store.Summary(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{System: a.Cfg.SystemPrompt, Summary: summary}
	for _, r := range rows {
		if r.Seq <= through {
			continue // folded into the summary already
		}
		var blocks []provider.Block
		if err := json.Unmarshal(r.BlocksJSON, &blocks); err != nil {
			return Snapshot{}, fmt.Errorf("decode message %d: %w", r.ID, err)
		}
		snap.Messages = append(snap.Messages, provider.Message{Role: provider.Role(r.Role), Blocks: blocks})
	}
	return snap, nil
}

func (a *Agent) appendMessage(ctx context.Context, sessionID string, role provider.Role, blocks []provider.Block, model, site string, u provider.Usage, cost float64) error {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	_, err = a.Store.AppendMessage(ctx, store.Message{
		SessionID: sessionID, Role: string(role), BlocksJSON: raw,
		Model: model, CallSite: site, TokensIn: u.InputTokens, TokensOut: u.OutputTokens, CostUSD: cost,
	})
	return err
}

// Run executes one user turn and returns a channel of events. The channel is
// closed when the turn finishes; the caller may abandon it only by cancelling
// ctx.
func (a *Agent) Run(ctx context.Context, sessionID, input string) (<-chan Event, error) {
	if err := a.appendMessage(ctx, sessionID, provider.RoleUser,
		[]provider.Block{{Type: provider.BlockText, Text: input}}, "", "", provider.Usage{}, 0); err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		if err := a.loop(ctx, sessionID, out); err != nil {
			out <- Event{Type: EvError, Err: err}
		}
	}()
	return out, nil
}

func (a *Agent) loop(ctx context.Context, sessionID string, out chan<- Event) error {
	for i := 0; i < maxIterations; i++ {
		if err := a.MaybeCompact(ctx, sessionID); err != nil {
			return fmt.Errorf("compaction: %w", err)
		}
		snap, err := a.Snapshot(ctx, sessionID)
		if err != nil {
			return err
		}

		req := Assemble(snap, a.Cfg.Context)
		if a.Tools != nil {
			req.Tools = a.Tools.Specs()
		}
		ref := a.Router.Model(router.SiteChat)
		p, model, price, err := a.Registry.Resolve(ref)
		if err != nil {
			return err
		}
		req.Model = model

		ch, err := p.Stream(ctx, req)
		if err != nil {
			return fmt.Errorf("provider %s: %w", ref, err)
		}

		var blocks []provider.Block
		var text string
		var calls []provider.Block
		var usage provider.Usage
		for ev := range ch {
			switch ev.Type {
			case provider.EventTextDelta:
				text += ev.Text
				out <- Event{Type: EvText, Text: ev.Text}
			case provider.EventToolCall:
				calls = append(calls, *ev.Block)
				out <- Event{Type: EvToolCall, Block: ev.Block}
			case provider.EventDone:
				if ev.Usage != nil {
					usage = *ev.Usage
				}
			case provider.EventError:
				return ev.Err
			}
		}

		if text != "" {
			blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: text})
		}
		blocks = append(blocks, calls...)
		cost := price.Cost(usage)
		if err := a.appendMessage(ctx, sessionID, provider.RoleAssistant, blocks, ref, router.SiteChat, usage, cost); err != nil {
			return err
		}

		if len(calls) == 0 {
			out <- Event{Type: EvTurnDone, Model: ref, Usage: usage, Cost: cost}
			return nil
		}
		if a.Tools == nil {
			return fmt.Errorf("model called tool %q but no tools are registered", calls[0].Name)
		}

		results, err := a.runTools(ctx, calls, out)
		if err != nil {
			return err
		}
		if err := a.appendMessage(ctx, sessionID, provider.RoleTool, results, "", "", provider.Usage{}, 0); err != nil {
			return err
		}
	}
	return fmt.Errorf("turn exceeded %d provider round trips without settling", maxIterations)
}

// runTools dispatches a batch. Calls run concurrently only when every call in
// the batch is read-only; any mutating call forces strict sequential order.
func (a *Agent) runTools(ctx context.Context, calls []provider.Block, out chan<- Event) ([]provider.Block, error) {
	allReadOnly := true
	for _, c := range calls {
		if !a.Tools.ReadOnly(c.Name) {
			allReadOnly = false
			break
		}
	}

	results := make([]provider.Block, len(calls))
	if allReadOnly && len(calls) > 1 {
		done := make(chan struct{}, len(calls))
		for i := range calls {
			go func(i int) {
				results[i] = a.Tools.Run(ctx, calls[i])
				done <- struct{}{}
			}(i)
		}
		for range calls {
			<-done
		}
	} else {
		for i := range calls {
			results[i] = a.Tools.Run(ctx, calls[i])
		}
	}

	for i := range results {
		b := results[i]
		out <- Event{Type: EvToolResult, Block: &b}
	}
	return results, nil
}
