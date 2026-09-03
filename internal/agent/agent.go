package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// maxIterations bounds one turn's provider round trips so a model that keeps
// calling tools cannot spin forever.
const maxIterations = 12

// maxParallelTools bounds a read-only batch's concurrency. A model can emit
// any number of tool calls in one message, and each may open a file or an
// HTTP connection; without a cap one turn can exhaust descriptors or sockets.
const maxParallelTools = 8

// persistCtx detaches a transcript write from the turn's cancellation. A turn
// can legitimately be abandoned mid-flight — the daemon shutting down, a
// suspended approval nobody answers — but it must never be abandoned
// half-recorded: an assistant message whose tool_use blocks have no matching
// tool_result is rejected by every provider on every subsequent turn, which
// breaks the session permanently. Values are preserved, cancellation is not.
func persistCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

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
//
// Run MUST be safe for concurrent use: the loop dispatches a batch of calls
// in parallel whenever every call in that batch reports ReadOnly. Batches
// containing any mutating call run sequentially, in order.
type ToolRunner interface {
	// Specs returns the schema for all available tools.
	Specs() []provider.ToolSpec
	// ReadOnly reports whether a tool call is read-only. Tools that report true
	// opt into concurrent dispatch when multiple calls arrive in a single batch;
	// those reporting false force sequential order.
	ReadOnly(name string) bool
	// Run executes a tool call and returns a tool_result block. It MUST be safe
	// for concurrent calls from the loop's dispatcher.
	Run(ctx context.Context, call provider.Block) provider.Block
}

type Agent struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	Tools    ToolRunner
	// Facts is the loaded fact set. Nil means no memory layer, which is what
	// a bare `spore once` runs with.
	Facts *memory.Cache
}

func New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, tools ToolRunner) *Agent {
	return &Agent{Store: st, Registry: reg, Router: rt, Cfg: cfg, Tools: tools}
}

// Snapshot reads the session's persisted state into the value context
// assembly consumes. Facts come from the fact cache when one is attached;
// a nil cache means no facts, which is what `spore once` runs with.
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
	if a.Facts != nil {
		snap.Facts = a.Facts.Facts()
	}
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

	// Drop any trailing assistant message whose blocks contain a tool_use with
	// no matching tool_result in the following message. A well-formed history
	// always has the tool results after it, so a trailing tool_use means the
	// turn was interrupted.
	if len(snap.Messages) >= 1 {
		lastMsg := snap.Messages[len(snap.Messages)-1]
		if lastMsg.Role == provider.RoleAssistant {
			hasToolUse := false
			for _, block := range lastMsg.Blocks {
				if block.Type == provider.BlockToolUse {
					hasToolUse = true
					break
				}
			}
			if hasToolUse {
				snap.Messages = snap.Messages[:len(snap.Messages)-1]
			}
		}
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
	pctx, cancelPersist := persistCtx(ctx)
	err := a.appendMessage(pctx, sessionID, provider.RoleUser,
		[]provider.Block{{Type: provider.BlockText, Text: input}}, "", "", provider.Usage{}, 0)
	cancelPersist()
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		ctx, turn := sporetrace.StartTurn(ctx, sessionID, "core")
		defer turn.End()
		if err := a.loop(ctx, sessionID, out); err != nil {
			turn.RecordError(err)
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

		llmCtx, llmSpan := sporetrace.StartLLM(ctx, router.SiteChat, ref)
		ch, err := p.Stream(llmCtx, req)
		if err != nil {
			llmSpan.RecordError(err)
			llmSpan.End()
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
				llmSpan.RecordError(ev.Err)
				llmSpan.End()
				return ev.Err
			}
		}

		if text != "" {
			blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: text})
		}
		blocks = append(blocks, calls...)
		cost := price.Cost(usage)
		sporetrace.EndLLM(llmSpan, req.System, text, usage, cost)
		pctx, cancelPersist := persistCtx(ctx)
		err = a.appendMessage(pctx, sessionID, provider.RoleAssistant, blocks, ref, router.SiteChat, usage, cost)
		cancelPersist()
		if err != nil {
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
		pctx2, cancelPersist2 := persistCtx(ctx)
		err = a.appendMessage(pctx2, sessionID, provider.RoleTool, results, "", "", provider.Usage{}, 0)
		cancelPersist2()
		if err != nil {
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

	run := func(call provider.Block) provider.Block {
		toolCtx, span := sporetrace.StartTool(ctx, call.Name, call.Input)
		defer span.End()
		res := a.Tools.Run(toolCtx, call)
		sporetrace.RecordToolResult(span, res.Content, res.IsError, res.Truncated)
		return res
	}

	results := make([]provider.Block, len(calls))
	if allReadOnly && len(calls) > 1 {
		sem := make(chan struct{}, maxParallelTools)
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				results[i] = run(calls[i])
			}(i)
		}
		wg.Wait()
	} else {
		for i := range calls {
			results[i] = run(calls[i])
		}
	}

	for i := range results {
		b := results[i]
		out <- Event{Type: EvToolResult, Block: &b}
	}
	return results, nil
}
