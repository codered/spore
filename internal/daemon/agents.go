package daemon

import (
	"errors"
	"net/http"

	"github.com/codered/spore/internal/subagent"
)

// AgentsJSON is the /agents response. Live and finished children render the
// same shape, because both are read from the same run rows.
type AgentsJSON struct {
	Agents []subagent.Status `json:"agents"`
}

// handleAgents lists the sub-agents a session has launched: what each was
// asked, whether it is still going, how long it has run and what it has cost.
// It polls rather than streaming -- a child's turn deltas are deliberately
// kept off the parent's stream, since keeping that noise out of the parent is
// the point of delegating in the first place.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if s.subagents == nil {
		writeJSON(w, http.StatusOK, AgentsJSON{Agents: []subagent.Status{}})
		return
	}
	list, err := s.subagents.List(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sub-agents: %v", err)
		return
	}
	if list == nil {
		list = []subagent.Status{}
	}
	writeJSON(w, http.StatusOK, AgentsJSON{Agents: list})
}

// handleCancelAgent stops one running sub-agent. Cancelling is a human
// action, which is why it is an endpoint and not a tool the model can call.
// The child must be one this session launched: a session id in the path is
// no licence to stop another conversation's work.
func (s *Server) handleCancelAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	childID := r.PathValue("child")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	run, found, err := s.store.SubagentRun(r.Context(), childID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read sub-agent: %v", err)
		return
	}
	if !found || run.ParentID != id || s.subagents == nil {
		writeError(w, http.StatusNotFound, "no sub-agent %s in session %s", childID, id)
		return
	}
	if err := s.subagents.Cancel(r.Context(), childID); err != nil {
		if errors.Is(err, subagent.ErrNotRunning) {
			writeError(w, http.StatusConflict, "%v", err)
			return
		}
		writeError(w, http.StatusInternalServerError, "cancel sub-agent: %v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
