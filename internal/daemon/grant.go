package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/codered/spore/internal/policy"
)

// handleStartGrant starts or replaces the root's timed grant, then answers
// every ask already waiting in its tree. The duration must be one of the
// choices in policy.grant_durations, read from the engine in force now.
func (s *Server) handleStartGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if s.guard == nil {
		writeError(w, http.StatusServiceUnavailable, "timed grants need the policy guard, which this daemon does not run")
		return
	}
	if !s.refuseSubagent(w, r, id) {
		return
	}
	var body struct {
		Duration string `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	eng := s.guard.Engine()
	d, ok := eng.GrantDuration(body.Duration)
	if !ok {
		choices := eng.GrantChoices()
		if len(choices) == 0 {
			writeError(w, http.StatusBadRequest, "timed grants are off: policy.grant_durations is empty")
			return
		}
		writeError(w, http.StatusBadRequest, "duration must be one of %s, got %q", strings.Join(choices, ", "), body.Duration)
		return
	}
	until := time.Now().Add(d)
	// The grant is set before the sweep, so an ask arriving between the two
	// is covered by the grant instead of waiting on a card.
	s.guard.StartGrant(id, until)
	s.hub.Publish(id, WireEvent{Type: WireGrant, ExpiresAt: expiresAt(until)})
	// The sweep settles rows through the store. A client that disconnects
	// now must not cancel it half way and leave a waiting ask pending, so it
	// runs on a context that keeps the request's values but not its cancel.
	s.sweepGrantedAsks(context.WithoutCancel(r.Context()), id)
	writeJSON(w, http.StatusOK, map[string]string{"expires_at": expiresAt(until)})
}

// handleEndGrant ends the root's timed grant. Asks after it prompt again.
func (s *Server) handleEndGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if s.guard == nil {
		writeError(w, http.StatusServiceUnavailable, "timed grants need the policy guard, which this daemon does not run")
		return
	}
	if !s.refuseSubagent(w, r, id) {
		return
	}
	s.guard.EndGrant(id)
	s.hub.Publish(id, WireEvent{Type: WireGrant})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ended"})
}

// refuseSubagent writes a 400 and returns false when id is a sub-agent. Grants
// belong to the root, the session a human is attached to.
func (s *Server) refuseSubagent(w http.ResponseWriter, r *http.Request, id string) bool {
	anc, err := s.store.SessionAncestors(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read session ancestors: %v", err)
		return false
	}
	if len(anc) > 0 {
		writeError(w, http.StatusBadRequest, "session %s is a sub-agent; send its root session id", id)
		return false
	}
	return true
}

// sweepGrantedAsks answers every waiting ask in the root's tree allow/once.
// A live waiter takes the answer through the broker. A suspension with no
// waiter, left by a restart, is settled through Guard.Resolve, as the
// approval endpoint does. A row that is no longer pending is skipped.
func (s *Server) sweepGrantedAsks(ctx context.Context, root string) {
	pending, err := s.guard.PendingTree(ctx, root)
	if err != nil {
		slog.Warn("grant sweep: list pending approvals", "session", root, "err", err)
		return
	}
	ans := policy.Answer{Allow: true, Scope: policy.ScopeOnce}
	for _, p := range pending {
		if s.broker.Answer(root, p.ID, ans) {
			continue
		}
		if err := s.guard.Resolve(ctx, root, p.ID, ans); err != nil {
			if !errors.Is(err, policy.ErrNotPending) {
				slog.Warn("grant sweep: resolve approval", "session", root, "pending", p.ID, "err", err)
			}
			continue
		}
		s.hub.Publish(root, WireEvent{Type: WireResolved, PendingID: p.ID, Decision: decisionOf(ans)})
	}
}

// grantEvents is the live grant of one root, for a client attaching to that
// session's stream. It is empty when no grant is live.
func (s *Server) grantEvents(root string) []WireEvent {
	if s.guard == nil {
		return nil
	}
	until, ok := s.guard.GrantUntil(root)
	if !ok {
		return nil
	}
	return []WireEvent{{Type: WireGrant, ExpiresAt: expiresAt(until)}}
}

// allGrantEvents is grantEvents for every live grant, each tagged with its
// root, for a client attaching to the global feed.
func (s *Server) allGrantEvents() []WireEvent {
	if s.guard == nil {
		return nil
	}
	var out []WireEvent
	for root, until := range s.guard.Grants() {
		out = append(out, WireEvent{Type: WireGrant, Session: root, ExpiresAt: expiresAt(until)})
	}
	return out
}
