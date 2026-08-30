package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// Runner is the minimal shape the guard wraps. It is declared here rather
// than imported from internal/agent so the dependency runs one way: the agent
// knows nothing about policy, and policy knows nothing about the agent.
type Runner interface {
	Specs() []provider.ToolSpec
	ReadOnly(name string) bool
	Run(ctx context.Context, call provider.Block) provider.Block
}

// Scope is how long an approval answer lasts.
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session"
	ScopePattern Scope = "pattern"
)

// Ask is one approval request handed to a client.
type Ask struct {
	SessionID string
	Tool      string
	Args      json.RawMessage
	// Rule is the policy rule that produced the ask, shown so the human
	// answering knows why they are being asked.
	Rule string
	// PendingID is the persisted suspension this request belongs to.
	PendingID int64
	// Pattern is the rule "always allow this pattern" would write.
	Pattern string
}

type Answer struct {
	Allow bool
	Scope Scope
}

// Approver renders an approval request to a human and returns their answer.
// The CLI implements it against the terminal; Plan 3's daemon implements it
// over SSE, and Plan 4's bridge with inline keyboard buttons.
type Approver interface {
	Ask(ctx context.Context, a Ask) (Answer, error)
}

type sessionKey struct{}

type sessionInfo struct {
	id      string
	profile Profile
}

// WithSession attaches the session and trust profile a turn is running under.
// The guard reads them from the context so one guard can serve concurrent
// sessions in the daemon.
func WithSession(ctx context.Context, sessionID string, p Profile) context.Context {
	return context.WithValue(ctx, sessionKey{}, sessionInfo{id: sessionID, profile: p})
}

// SessionFrom returns the session and profile on the context. The zero
// profile is local.
func SessionFrom(ctx context.Context) (string, Profile) {
	info, _ := ctx.Value(sessionKey{}).(sessionInfo)
	if info.profile == "" {
		info.profile = ProfileLocal
	}
	return info.id, info.profile
}

// Guard evaluates every call before the wrapped runner sees it.
type Guard struct {
	inner    Runner
	engine   *Engine
	approver Approver
	store    *store.Store
	// learn persists a rule accepted with ScopePattern. Nil disables the
	// "always this pattern" answer.
	learn func(d Decision, rule string) error
}

func NewGuard(inner Runner, e *Engine, ap Approver, st *store.Store, learn func(Decision, string) error) *Guard {
	return &Guard{inner: inner, engine: e, approver: ap, store: st, learn: learn}
}

func (g *Guard) Specs() []provider.ToolSpec { return g.inner.Specs() }
func (g *Guard) ReadOnly(name string) bool  { return g.inner.ReadOnly(name) }

func denied(id, format string, args ...any) provider.Block {
	return provider.Block{
		Type:    provider.BlockToolResult,
		ID:      id,
		Content: fmt.Sprintf(format, args...),
		IsError: true,
	}
}

// Run resolves policy, suspends for approval when needed, and only then
// delegates. It never returns an error: a refusal is a tool error the model
// can read and route around.
func (g *Guard) Run(ctx context.Context, call provider.Block) provider.Block {
	c := Call{Tool: call.Name, Args: call.Input}
	sessionID, profile := SessionFrom(ctx)

	res := g.engine.Evaluate(profile, c)
	sporetrace.RecordPolicy(ctx, string(res.Decision), res.Rule)

	switch res.Decision {
	case DecisionDeny:
		// Deny is absolute and is never escalated to a human: offering an
		// approval prompt here is exactly the lever prompt injection wants.
		return denied(call.ID, "denied by policy rule %q. Do not retry this call; choose another approach.", res.Rule)
	case DecisionAllow:
		return g.inner.Run(ctx, call)
	}

	// From here the decision is ask.
	if sessionID == "" {
		return denied(call.ID, "cannot request approval: no session on the context")
	}
	if remembered, ok, err := g.store.SessionDecision(ctx, sessionID, call.Name); err == nil && ok {
		if remembered == string(DecisionAllow) {
			sporetrace.RecordPolicy(ctx, "allow", "approved earlier this session")
			return g.inner.Run(ctx, call)
		}
		sporetrace.RecordPolicy(ctx, "deny", "declined earlier this session")
		return denied(call.ID, "the user declined %s earlier in this session", call.Name)
	}

	if g.approver == nil {
		return denied(call.ID, "%s needs approval but no approver is attached", call.Name)
	}

	pattern := PatternFor(c)
	pendingID, err := g.store.AddPendingCall(ctx, store.PendingCall{
		SessionID: sessionID,
		ToolUseID: call.ID,
		Tool:      call.Name,
		ArgsJSON:  call.Input,
		Profile:   string(profile),
		Rule:      res.Rule,
	})
	if err != nil {
		return denied(call.ID, "could not record the approval request: %v", err)
	}

	// An approval nobody answers denies, so a turn started from a phone
	// cannot sit half-executed forever.
	askCtx, cancel := context.WithTimeout(ctx, g.engine.ApprovalTimeout())
	defer cancel()

	answer, err := g.approver.Ask(askCtx, Ask{
		SessionID: sessionID,
		Tool:      call.Name,
		Args:      call.Input,
		Rule:      res.Rule,
		PendingID: pendingID,
		Pattern:   pattern,
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		_ = g.store.ResolvePendingCall(ctx, pendingID, "timeout")
		_ = g.store.RecordApproval(ctx, sessionID, call.Name, call.Input, "deny", "timeout")
		sporetrace.RecordPolicy(ctx, "deny", "approval timed out")
		return denied(call.ID, "approval for %s timed out after %s and was denied", call.Name, g.engine.ApprovalTimeout())
	case err != nil:
		_ = g.store.ResolvePendingCall(ctx, pendingID, "error")
		sporetrace.RecordPolicy(ctx, "deny", "approver unavailable")
		return denied(call.ID, "could not ask for approval: %v", err)
	}

	decision := DecisionDeny
	if answer.Allow {
		decision = DecisionAllow
	}
	_ = g.store.ResolvePendingCall(ctx, pendingID, string(decision))
	_ = g.store.RecordApproval(ctx, sessionID, call.Name, call.Input, string(decision), string(answer.Scope))

	if answer.Scope == ScopePattern && g.learn != nil {
		if err := g.learn(decision, pattern); err != nil {
			// Failing to persist the rule must not change this call's
			// outcome; the user simply gets asked again next time.
			sporetrace.RecordPolicy(ctx, string(decision), "learned rule not persisted: "+err.Error())
		}
	}

	if !answer.Allow {
		sporetrace.RecordPolicy(ctx, "deny", "user declined")
		return denied(call.ID, "the user declined this %s call", call.Name)
	}
	sporetrace.RecordPolicy(ctx, "allow", "user approved ("+string(answer.Scope)+")")
	return g.inner.Run(ctx, call)
}

// PatternFor proposes the rule an "always allow this pattern" answer writes.
// When the call has a path it generalises to that path's directory; otherwise
// it falls back to the bare tool name rather than guessing.
func PatternFor(c Call) string {
	paths := argPaths(c)
	if len(paths) != 1 {
		return c.Tool
	}
	dir := filepath.Dir(paths[0])
	if dir == "." || dir == string(filepath.Separator) {
		return c.Tool
	}
	return fmt.Sprintf("%s(path matches %s/**)", c.Tool, strings.TrimSuffix(dir, "/"))
}
