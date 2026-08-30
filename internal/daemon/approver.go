package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	"github.com/codered/spore/internal/policy"
)

// Broker is the daemon's policy.Approver. Ask cannot prompt a browser
// synchronously, so it publishes the request to every client attached to the
// session and blocks on a waiter keyed by the persisted suspension's id; the
// resolve endpoint delivers the answer.
//
// Exactly one of two paths records an approval: this waiter (after which
// Guard.Run writes the audit row), or Guard.Resolve when no waiter exists.
// Answer's bool is what keeps them mutually exclusive.
type Broker struct {
	hub *Hub

	mu      sync.Mutex
	waiters map[int64]chan policy.Answer
}

func NewBroker(h *Hub) *Broker {
	return &Broker{hub: h, waiters: map[int64]chan policy.Answer{}}
}

// Ask implements policy.Approver.
func (b *Broker) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	ch := make(chan policy.Answer, 1)
	b.mu.Lock()
	b.waiters[a.PendingID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, a.PendingID)
		b.mu.Unlock()
	}()

	b.hub.Publish(a.SessionID, approvalEvent(a))

	select {
	case ans := <-ch:
		b.hub.Publish(a.SessionID, WireEvent{
			Type: WireResolved, PendingID: a.PendingID, Tool: a.Tool,
			Decision: decisionOf(ans),
		})
		return ans, nil
	case <-ctx.Done():
		// The guard turns a deadline into a denial and records it; the broker
		// only reports why it stopped waiting.
		return policy.Answer{}, ctx.Err()
	}
}

// Answer hands an answer to a waiting Ask, reporting whether one was there.
// False means the daemon restarted (or the turn was abandoned) since the
// suspension was written, and the caller should use Guard.Resolve instead.
func (b *Broker) Answer(pendingID int64, ans policy.Answer) bool {
	b.mu.Lock()
	ch, ok := b.waiters[pendingID]
	if ok {
		// Delete under the lock so a second concurrent Answer finds nothing
		// and is told it lost.
		delete(b.waiters, pendingID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- ans
	return true
}

func approvalEvent(a policy.Ask) WireEvent {
	return WireEvent{
		Type: WireApproval, PendingID: a.PendingID, Tool: a.Tool,
		Args: string(a.Args), Rule: a.Rule, Pattern: a.Pattern,
	}
}

func decisionOf(a policy.Answer) string {
	if a.Allow {
		return "allow"
	}
	return "deny"
}

// pendingApprovalEvents lists the approvals already waiting on a human, as
// wire events. A client attaching mid-suspension — a second browser tab, or
// the first one after a daemon restart — sees them before any deltas.
func (s *Server) pendingApprovalEvents(ctx context.Context, sessionID string) []WireEvent {
	if s.guard == nil {
		return nil
	}
	pending, err := s.guard.Pending(ctx, sessionID)
	if err != nil {
		return nil
	}
	out := make([]WireEvent, 0, len(pending))
	for _, p := range pending {
		out = append(out, WireEvent{
			Type: WireApproval, PendingID: p.ID, Tool: p.Tool,
			Args: string(p.ArgsJSON), Rule: p.Rule,
			Pattern: policy.PatternFor(policy.Call{Tool: p.Tool, Args: p.ArgsJSON}),
		})
	}
	return out
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.pendingApprovalEvents(r.Context(), id))
}

func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	pendingID, err := strconv.ParseInt(r.PathValue("pending"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pending id must be a number: %v", err)
		return
	}
	var body struct {
		Allow bool   `json:"allow"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	scope := policy.Scope(body.Scope)
	switch scope {
	case policy.ScopeOnce, policy.ScopeSession, policy.ScopePattern:
	case "":
		scope = policy.ScopeOnce
	default:
		writeError(w, http.StatusBadRequest, "scope must be once, session or pattern, got %q", body.Scope)
		return
	}
	ans := policy.Answer{Allow: body.Allow, Scope: scope}

	// A live waiter is the normal path: the suspended turn takes the answer
	// and Guard.Run records it. Only when nothing is waiting — the daemon
	// restarted while the approval was open — does the out-of-band path
	// apply. Taking both would write two audit rows for one decision.
	if s.broker.Answer(pendingID, ans) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
		return
	}
	if s.guard == nil {
		writeError(w, http.StatusNotFound, "no approval %d is waiting", pendingID)
		return
	}
	if err := s.guard.Resolve(r.Context(), sessionID, pendingID, ans); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.hub.Publish(sessionID, WireEvent{Type: WireResolved, PendingID: pendingID, Decision: decisionOf(ans)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}
