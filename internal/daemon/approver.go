package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/codered/spore/internal/policy"
)

// answeredTTL is how long to remember answered suspensions. This must exceed
// the approval timeout so we don't re-answer the same suspension if the guard's
// audit write is still in flight when a retry arrives.
const answeredTTL = 10 * time.Minute

// waiter is a waiting Ask, keyed by suspension id in the broker's map.
type waiter struct {
	sessionID string
	ch        chan policy.Answer
}

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

	mu          sync.Mutex
	waiters     map[int64]waiter
	answered    map[int64]time.Time
	answeredTTL time.Duration
}

func NewBroker(h *Hub) *Broker {
	return NewBrokerWithTTL(h, answeredTTL)
}

func NewBrokerWithTTL(h *Hub, ttl time.Duration) *Broker {
	return &Broker{
		hub:         h,
		waiters:     map[int64]waiter{},
		answered:    map[int64]time.Time{},
		answeredTTL: ttl,
	}
}

// Ask implements policy.Approver.
func (b *Broker) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	ch := make(chan policy.Answer, 1)
	b.mu.Lock()
	b.waiters[a.PendingID] = waiter{sessionID: a.SessionID, ch: ch}
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
		// Deciding to give up has to be atomic with respect to Answer, or a
		// decision a human already made is discarded while the handler has
		// told them it was delivered. The waiter map is the synchronisation
		// point: if our entry is still there, no Answer has claimed us and
		// giving up is safe. If it is gone, exactly one Answer removed it
		// under the lock and is committed to sending into a buffered channel
		// that cannot block — so the value is either there now or imminently,
		// and we must take it.
		b.mu.Lock()
		_, stillWaiting := b.waiters[a.PendingID]
		delete(b.waiters, a.PendingID)
		b.mu.Unlock()
		if !stillWaiting {
			ans := <-ch
			b.hub.Publish(a.SessionID, WireEvent{
				Type: WireResolved, PendingID: a.PendingID, Tool: a.Tool,
				Decision: decisionOf(ans),
			})
			return ans, nil
		}
		// The guard turns a deadline into a denial and records it; the broker
		// only reports why it stopped waiting.
		return policy.Answer{}, ctx.Err()
	}
}

// Answer hands an answer to a waiting Ask, reporting whether one was there.
// False means the daemon restarted (or the turn was abandoned) since the
// suspension was written, and the caller should use Guard.Resolve instead.
// It verifies that the asking session matches the caller's sessionID to prevent
// cross-session approval.
func (b *Broker) Answer(sessionID string, pendingID int64, ans policy.Answer) bool {
	b.mu.Lock()
	w, ok := b.waiters[pendingID]
	if !ok || w.sessionID != sessionID {
		// Either no waiter exists, or it's for a different session.
		// Do NOT delete the waiter; another session must not destroy the real owner's waiter.
		b.mu.Unlock()
		return false
	}

	// Delete under the lock so a second concurrent Answer finds nothing
	// and is told it lost.
	delete(b.waiters, pendingID)

	// Record that we've answered this, so we reject retries that arrive
	// before Guard.Run's audit write completes.
	b.answered[pendingID] = time.Now()
	b.pruneAnswered()
	b.mu.Unlock()

	// Send without the lock so we don't block other goroutines.
	w.ch <- ans
	return true
}

// AlreadyAnswered reports whether this daemon has already delivered an
// answer for a suspension whose audit write may still be in flight.
func (b *Broker) AlreadyAnswered(pendingID int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.answered[pendingID]
	return ok
}

// pruneAnswered removes entries older than the TTL. Must be called with the
// lock held.
func (b *Broker) pruneAnswered() {
	now := time.Now()
	for id, t := range b.answered {
		if now.Sub(t) > b.answeredTTL {
			delete(b.answered, id)
		}
	}
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
	// Verify the session exists early, before attempting to answer.
	if _, ok := s.findSession(w, r, sessionID); !ok {
		return
	}
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
	if s.broker.Answer(sessionID, pendingID, ans) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
		return
	}
	if s.broker.AlreadyAnswered(pendingID) {
		writeError(w, http.StatusConflict, "approval %d was already answered", pendingID)
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
