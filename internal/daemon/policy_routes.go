package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

type PolicyRuleJSON struct {
	Profile  string `json:"profile"`
	Decision string `json:"decision"`
	Rule     string `json:"rule"`
	Source   string `json:"source"`
}

type PolicyJSON struct {
	Rules []PolicyRuleJSON `json:"rules"`
}

// handlePolicy lists every profile's rules in evaluation order, from the
// engine calls are evaluated against right now.
func (s *Server) handlePolicy(w http.ResponseWriter, r *http.Request) {
	if s.guard == nil {
		writeError(w, http.StatusServiceUnavailable, "policy is not available on this daemon")
		return
	}
	rows := s.guard.Engine().Rules()
	out := PolicyJSON{Rules: make([]PolicyRuleJSON, 0, len(rows))}
	for _, row := range rows {
		out.Rules = append(out.Rules, PolicyRuleJSON{
			Profile: string(row.Profile), Decision: string(row.Decision), Rule: row.Rule, Source: row.Source,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRevoke removes one learned rule and makes that live.
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	if s.op.Policy == nil {
		writeError(w, http.StatusServiceUnavailable, "revoking rules is not available on this daemon")
		return
	}
	var body struct {
		Decision string `json:"decision"`
		Rule     string `json:"rule"`
	}
	// A revoke body is one rule; anything larger is not a request this route serves.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "body must be {\"decision\", \"rule\"}: %v", err)
		return
	}
	switch policy.Decision(body.Decision) {
	case policy.DecisionAllow, policy.DecisionAsk, policy.DecisionDeny:
	default:
		writeError(w, http.StatusBadRequest, "decision must be allow, ask or deny, got %q", body.Decision)
		return
	}
	err := s.op.Policy.Unlearn(policy.Decision(body.Decision), body.Rule)
	switch {
	case errors.Is(err, config.ErrNotLearned):
		writeError(w, http.StatusNotFound, "only rules added with p can be revoked here")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	audit(r, "revoke", body.Rule, "decision", body.Decision)
	writeJSON(w, http.StatusOK, map[string]string{"revoked": body.Rule})
}
