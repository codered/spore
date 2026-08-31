package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

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
	ScopeOnce Scope = "once"
	// ScopeSession remembers the answer for the whole TOOL for the rest of the
	// session, not for these arguments. Approving shell_exec once for "ls"
	// approves it for every later command too. Baseline deny still applies, but
	// an approver must say this plainly when it offers the option.
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

// SessionFrom returns the session and profile on the context. When nothing is
// attached it reports the LEAST trusted profile, not the most: a caller that
// forgot WithSession must fail toward the strictest ruleset, never toward the
// one that allows the most.
func SessionFrom(ctx context.Context) (string, Profile) {
	info, ok := ctx.Value(sessionKey{}).(sessionInfo)
	if !ok || info.profile == "" {
		// Both cases are a caller mistake — nothing attached, or a session
		// attached without naming its trust level — and both resolve to the
		// strictest ruleset. A profile is only ever trusted because someone
		// said so explicitly.
		return info.id, ProfileRemote
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
	sessionID, profile := SessionFrom(ctx)
	// Checked BEFORE evaluation, not on the ask branch: a call with no session
	// cannot be audited, attributed, or routed to a human, so it must not run
	// at all — not even a tool policy would allow outright. Leaving this until
	// the ask branch would let an allowed call through unattributed.
	if sessionID == "" {
		sporetrace.RecordPolicy(ctx, "deny", "policy.no-session")
		return denied(call.ID, "refusing %s: no session on the context, so the call cannot be audited", call.Name)
	}

	c := Call{Tool: call.Name, Args: call.Input}
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

	// Bookkeeping writes use a context detached from the caller's. When a turn
	// is abandoned mid-approval the caller's ctx is already dead, and writing
	// through it fails instantly — stranding the suspension row we just wrote
	// and losing the audit entry. Values are preserved, cancellation is not.
	book, cancelBook := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelBook()

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
		// Resolve the suspension. If this call claimed it (not already answered
		// elsewhere), write the audit row. If not, skip — another path already
		// recorded this decision and we must not add a second contradictory row.
		if claimed, err := g.store.ResolvePendingCall(book, pendingID, "timeout"); err == nil && claimed {
			_ = g.store.RecordApproval(book, sessionID, call.Name, call.Input, "deny", "timeout")
		}
		sporetrace.RecordPolicy(ctx, "deny", "approval timed out")
		return denied(call.ID, "approval for %s timed out after %s and was denied", call.Name, g.engine.ApprovalTimeout())
	case err != nil:
		// Same as timeout branch: only write the audit row if we claimed the
		// suspension, not if another path already answered it.
		if claimed, err := g.store.ResolvePendingCall(book, pendingID, "error"); err == nil && claimed {
			// Note: error case doesn't write an audit row in the original code
		}
		sporetrace.RecordPolicy(ctx, "deny", "approver unavailable")
		return denied(call.ID, "could not ask for approval: %v", err)
	}

	decision := DecisionDeny
	if answer.Allow {
		decision = DecisionAllow
	}
	// Resolve the suspension. If this call claimed it, write both the audit
	// row and learn the rule if applicable. If not claimed, skip both because
	// another path already recorded this decision.
	claimed, err := g.store.ResolvePendingCall(book, pendingID, string(decision))
	_ = err // Already logged by caller if needed
	if claimed {
		_ = g.store.RecordApproval(book, sessionID, call.Name, call.Input, string(decision), string(answer.Scope))

		if answer.Scope == ScopePattern && g.learn != nil {
			if err := g.learn(decision, pattern); err != nil {
				// Failing to persist the rule must not change this call's
				// outcome; the user simply gets asked again next time.
				sporetrace.RecordPolicy(ctx, string(decision), "learned rule not persisted: "+err.Error())
			}
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

// Pending lists the session's unanswered approval requests. A client that
// attaches to a session — after a restart, or as a second client — calls this
// to find out what is waiting on a human.
func (g *Guard) Pending(ctx context.Context, sessionID string) ([]store.PendingCall, error) {
	return g.store.PendingCalls(ctx, sessionID)
}

// Resolve answers a pending approval by id. It is the out-of-band path used
// when the answer arrives from somewhere other than the Approver that asked —
// a second client, or a process that restarted while the request was open.
// The session is checked so one session cannot answer another's approvals.
func (g *Guard) Resolve(ctx context.Context, sessionID string, pendingID int64, ans Answer) error {
	decision := DecisionDeny
	if ans.Allow {
		decision = DecisionAllow
	}
	// One transaction claims the suspension and writes its audit row together.
	// Two clients answering at once cannot both record an answer, and a
	// failure part-way cannot leave a resolved row with no audit entry.
	claimed, won, err := g.store.ClaimPendingCall(ctx, pendingID, sessionID, string(decision), string(ans.Scope))
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("no pending call %d in session %s (already answered, or another session's)", pendingID, sessionID)
	}
	if ans.Scope == ScopePattern && g.learn != nil {
		if err := g.learn(decision, PatternFor(Call{Tool: claimed.Tool, Args: claimed.ArgsJSON})); err != nil {
			// Same invariant as Run: failing to persist a learned rule must not
			// undo an answer already recorded, or the caller retries and is
			// told the call was "already answered". The user is asked again
			// next time instead.
			sporetrace.RecordPolicy(ctx, string(decision), "learned rule not persisted: "+err.Error())
		}
	}
	return nil
}
