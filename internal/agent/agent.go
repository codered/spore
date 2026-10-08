package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/kernel"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/persona"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

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
	// EvStopped ends a turn a human stopped. It carries Err: ErrStopped so a
	// consumer that only checks Err (the sub-agent supervisor) still sees a
	// turn that did not finish.
	EvStopped EventType = "stopped"
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

// ErrStopped is the cancellation cause that marks a turn as stopped by a
// human. A turn cancelled for any other reason -- the daemon shutting down --
// is an error, not a stop.
var ErrStopped = errors.New("stopped by user")

// stopMarker ends the text of a reply that was cut off, so the model can see
// on its next turn where it was interrupted.
const stopMarker = "\n\n[stopped by you]"

// stopped reports whether ctx was cancelled with ErrStopped. The cause
// propagates to derived contexts, so this holds inside tools and sub-agents
// launched from the stopped turn too.
func stopped(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrStopped)
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

// RefineHook is told when compaction folds messages and when a turn ends, so
// continual refinement can review them. internal/refine implements it; the
// agent does not import that package.
type RefineHook interface {
	AfterCompact(sessionID string, through int)
	AfterTurn(sessionID string)
}

type Agent struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	Tools    ToolRunner
	// Env renders the environment section of the system prompt for one root:
	// the session's working directory and its files. Nil means no environment
	// section, which is what a test that builds an Agent with New gets.
	Env func(root string) string
	// Facts is the loaded fact set. Nil means no memory layer attached; every
	// production construction path (buildAgent) sets it, so nil in practice
	// means a test built the Agent directly with New and never attached one.
	Facts *memory.Cache
	// Skills is the skills cache set shared with the skill tools: the tools
	// and the prompt index read the same caches, with the same reload
	// pattern. Nil means a test built the Agent directly with New; buildAgent
	// attaches the real one.
	Skills *skill.Caches
	// Refine receives the compaction and end-of-turn hooks. Nil means no
	// refinement attached, which is what a test built with New gets.
	Refine RefineHook
}

// onlyKernel keeps the go_run spec. In code mode every other tool is reached
// from inside a program, so offering them directly would invite the one-hop-
// per-tool pattern code mode exists to replace.
func onlyKernel(specs []provider.ToolSpec) []provider.ToolSpec {
	for _, s := range specs {
		if s.Name == kernel.ToolName {
			return []provider.ToolSpec{s}
		}
	}
	return nil
}

func New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, tools ToolRunner) *Agent {
	return &Agent{Store: st, Registry: reg, Router: rt, Cfg: cfg, Tools: tools}
}

// Snapshot reads the session's persisted state into the value context
// assembly consumes. Facts come from the fact cache when one is attached;
// a nil cache means no facts, which only happens when an Agent is built
// directly (as tests do) rather than through buildAgent.
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
	// The skills directory follows the session's root under workspace scope,
	// so the layout is resolved per turn from the same root the environment
	// section uses.
	snap.Self = selfSection(a.Cfg, policy.WorkspaceFrom(ctx))
	// The persona files are read per turn rather than cached: they are a
	// couple of kilobytes, this method already reads SQLite, and reading
	// them here means there is no staleness to reason about when the user
	// edits one mid-session. A read error is logged and dropped -- a
	// misconfigured soul.md must not fail every turn.
	if body, err := persona.Load(a.Cfg.SoulPath()); err != nil {
		slog.Warn("read soul.md", "path", a.Cfg.SoulPath(), "err", err)
	} else {
		snap.Soul = body
	}
	agentPath := a.Cfg.AgentPath(policy.WorkspaceFrom(ctx))
	if body, err := persona.Load(agentPath); err != nil {
		slog.Warn("read agent.md", "path", agentPath, "err", err)
	} else {
		snap.Agent = body
	}
	if a.Env != nil {
		// The root comes from the turn context, not from the agent: one agent
		// serves every session, and each is rooted somewhere of its own.
		snap.Environment = a.Env(policy.WorkspaceFrom(ctx))
	}
	if a.Facts != nil {
		snap.Facts = a.Facts.Facts()
	}
	if a.Tools != nil && a.Cfg.Kernel.Mode == config.KernelModeCode {
		snap.Kernel = kernel.Reference(a.Tools.Specs())
	}
	if a.Skills != nil {
		// The directory is resolved the same way the skill tools resolve it,
		// so the index and skill_load agree about what exists. An empty dir
		// -- a workspace-scope session with no root of its own -- means no
		// skills, not an error.
		dir := a.Cfg.SkillsDir(policy.WorkspaceFrom(ctx))
		snap.Skills = a.Skills.Skills(dir)
	}
	// request is the latest user message folded into the summary, kept in
	// case the live window has none of its own.
	var request *store.Message
	for i, r := range rows {
		if r.Seq <= through {
			if r.Role == string(provider.RoleUser) {
				request = &rows[i]
			}
			continue // folded into the summary already
		}
		if r.Role == store.RoleNote {
			continue // written for the person, not the model
		}
		var blocks []provider.Block
		if err := json.Unmarshal(r.BlocksJSON, &blocks); err != nil {
			return Snapshot{}, fmt.Errorf("decode message %d: %w", r.ID, err)
		}
		snap.Messages = append(snap.Messages, provider.Message{Role: provider.Role(r.Role), Blocks: blocks})
	}

	// Compacting mid-turn can fold the turn's only user message: a long
	// agentic turn is one request followed by nothing but tool traffic. The
	// summary sits in the system prompt, so the request would then open on an
	// assistant message, which providers reject -- Qwen's chat template with
	// "No user query found in messages". The folded request is restated
	// verbatim at the head instead. It is assembled here and never stored.
	if len(snap.Messages) > 0 && snap.Messages[0].Role != provider.RoleUser && request != nil {
		var blocks []provider.Block
		if err := json.Unmarshal(request.BlocksJSON, &blocks); err != nil {
			return Snapshot{}, fmt.Errorf("decode message %d: %w", request.ID, err)
		}
		snap.Messages = append([]provider.Message{{Role: provider.RoleUser, Blocks: blocks}}, snap.Messages...)
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
		Model: model, CallSite: site, TokensIn: u.InputTokens, TokensOut: u.OutputTokens,
		TokensCacheWrite: u.CacheWriteTokens, TokensCacheRead: u.CacheReadTokens,
		CostUSD: cost,
	})
	return err
}

// Run executes a turn as the session's own chat.
func (a *Agent) Run(ctx context.Context, sessionID, input string) (<-chan Event, error) {
	return a.RunSite(ctx, sessionID, input, router.SiteChat)
}

// RunSite is Run with an explicit call site. A sub-agent's turns name
// router.SiteSubagent, so they route -- and are costed in `/usage` -- as the
// delegated work they are rather than as the parent's chat.
func (a *Agent) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	if !router.ValidSite(site) {
		return nil, fmt.Errorf("unknown call site %q", site)
	}
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
		err := a.loop(ctx, sessionID, site, out)
		// Whatever the turn's outcome, a refine the model asked for during
		// it is started (or discarded) now, so the request never outlives
		// the turn that made it.
		if a.Refine != nil {
			a.Refine.AfterTurn(sessionID)
		}
		if err != nil {
			if stopped(ctx) {
				out <- Event{Type: EvStopped, Err: ErrStopped}
				return
			}
			turn.RecordError(err)
			out <- Event{Type: EvError, Err: err}
		}
	}()
	return out, nil
}

func (a *Agent) loop(ctx context.Context, sessionID, site string, out chan<- Event) error {
	limit := a.Cfg.Context.MaxRoundTrips
	for i := 0; limit == 0 || i < limit; i++ {
		if stopped(ctx) {
			return ErrStopped
		}
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
			if a.Cfg.Kernel.Mode == config.KernelModeCode {
				req.Tools = onlyKernel(req.Tools)
			}
		}
		ref := a.Router.Model(site)
		p, model, price, err := a.Registry.Resolve(ref)
		if err != nil {
			return err
		}
		req.Model = model

		llmCtx, llmSpan := sporetrace.StartLLM(ctx, site, ref)
		ch, err := p.Stream(llmCtx, req)
		if err != nil {
			llmSpan.RecordError(err)
			llmSpan.End()
			return fmt.Errorf("provider %s: %w", ref, err)
		}

		var blocks []provider.Block
		var text string
		var calls []provider.Block
		invalid := map[int]string{} // call index → why its arguments were refused
		var usage provider.Usage
		var hitMaxTokens bool
		var streamErr error
	stream:
		for ev := range ch {
			switch ev.Type {
			case provider.EventTextDelta:
				text += ev.Text
				out <- Event{Type: EvText, Text: ev.Text}
			case provider.EventToolCall:
				call := *ev.Block
				if why := invalidInput(call); why != "" {
					// Stored, sent and shown as {}: one invalid RawMessage
					// fails every later json.Marshal of the conversation.
					invalid[len(calls)] = why
					call.Input = json.RawMessage(`{}`)
				}
				calls = append(calls, call)
				out <- Event{Type: EvToolCall, Block: &call}
			case provider.EventDone:
				if ev.Usage != nil {
					usage = *ev.Usage
				}
				hitMaxTokens = ev.HitMaxTokens
			case provider.EventError:
				streamErr = ev.Err
				break stream
			}
		}

		// A stop is checked before a stream error: a provider cut off by a
		// cancelled context reports the cancellation as an error, and that
		// error is the stop, not a failure.
		if stopped(ctx) {
			llmSpan.End()
			if text != "" {
				pctx, cancelPersist := persistCtx(ctx)
				err := a.appendMessage(pctx, sessionID, provider.RoleAssistant,
					[]provider.Block{{Type: provider.BlockText, Text: text + stopMarker}},
					ref, site, usage, price.Cost(usage))
				cancelPersist()
				if err != nil {
					return err
				}
			}
			return ErrStopped
		}
		if streamErr != nil {
			llmSpan.RecordError(streamErr)
			llmSpan.End()
			return streamErr
		}

		if text != "" {
			blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: text})
		}
		blocks = append(blocks, calls...)
		cost := price.Cost(usage)
		var sysPrompt strings.Builder
		for _, b := range req.System {
			sysPrompt.WriteString(b.Text)
		}
		sporetrace.EndLLM(llmSpan, sysPrompt.String(), text, usage, cost)
		pctx, cancelPersist := persistCtx(ctx)
		err = a.appendMessage(pctx, sessionID, provider.RoleAssistant, blocks, ref, site, usage, cost)
		cancelPersist()
		if err != nil {
			return err
		}

		if len(calls) == 0 && text == "" && hitMaxTokens {
			// A reasoning model can spend the whole budget thinking. Ending
			// the turn as answered would show the user a blank reply.
			return fmt.Errorf("the model used all %d output tokens without replying; raise context.max_output_tokens in the config",
				req.MaxTokens)
		}
		if len(calls) == 0 {
			out <- Event{Type: EvTurnDone, Model: ref, Usage: usage, Cost: cost}
			return nil
		}
		if a.Tools == nil {
			return fmt.Errorf("model called tool %q but no tools are registered", calls[0].Name)
		}

		results := a.runTools(ctx, calls, invalid, out)
		pctx2, cancelPersist2 := persistCtx(ctx)
		err = a.appendMessage(pctx2, sessionID, provider.RoleTool, results, "", "", provider.Usage{}, 0)
		cancelPersist2()
		if err != nil {
			return err
		}
	}
	return fmt.Errorf("the turn used all %d round trips to the model without finishing; raise context.max_round_trips in the config (0 means no cap)", limit)
}

// runTools dispatches a batch. Calls run concurrently only when every call in
// the batch is read-only; any mutating call forces strict sequential order.
// A call listed in invalid is not dispatched: its result is the reason.
func (a *Agent) runTools(ctx context.Context, calls []provider.Block, invalid map[int]string, out chan<- Event) []provider.Block {
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
	runAt := func(i int) {
		if why, ok := invalid[i]; ok {
			results[i] = provider.Block{Type: provider.BlockToolResult, ID: calls[i].ID, Content: why, IsError: true}
			return
		}
		results[i] = run(calls[i])
	}
	if allReadOnly && len(calls) > 1 {
		sem := make(chan struct{}, maxParallelTools)
		var wg sync.WaitGroup
		for i := range calls {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				runAt(i)
			}(i)
		}
		wg.Wait()
	} else {
		for i := range calls {
			runAt(i)
		}
	}

	for i := range results {
		b := results[i]
		out <- Event{Type: EvToolResult, Block: &b}
	}
	return results
}

// invalidInput explains why a call's arguments cannot be used, or returns ""
// when they are a JSON object. Local models produce both kinds of failure:
// a stream cut off by the output token limit, and plain malformed JSON.
func invalidInput(call provider.Block) string {
	if len(call.Input) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	err := json.Unmarshal(call.Input, &obj)
	if err == nil {
		return ""
	}
	if !json.Valid(call.Input) && strings.Contains(err.Error(), "unexpected end of JSON input") {
		return fmt.Sprintf("your arguments to %s were not valid JSON: they stop after %d bytes, so they were probably cut off by the output token limit. The call did not run. Send a shorter call: for go_run, a smaller program.", call.Name, len(call.Input))
	}
	return fmt.Sprintf("your arguments to %s were not valid JSON (%v), so the call did not run. Send the arguments as one JSON object.", call.Name, err)
}
