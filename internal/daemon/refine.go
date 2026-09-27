package daemon

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/refine"
	"github.com/codered/spore/internal/store"
)

type RefinementJSON struct {
	ID        int64     `json:"id"`
	RoundID   string    `json:"round_id"`
	SessionID string    `json:"session_id"`
	Trigger   string    `json:"trigger"`
	Kind      string    `json:"kind"`
	Target    string    `json:"target"`
	Before    *string   `json:"before"`
	After     *string   `json:"after"`
	Rationale string    `json:"rationale"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

type RefineResultJSON struct {
	RoundID  string           `json:"round_id"`
	Note     string           `json:"note"`
	Applied  []RefinementJSON `json:"applied"`
	Proposed []RefinementJSON `json:"proposed"`
	Stale    []RefinementJSON `json:"stale"`
	Failed   []RefinementJSON `json:"failed"`
	Dropped  []string         `json:"dropped"`
}

type RollbackJSON struct {
	RoundID    string           `json:"round_id"`
	RolledBack []RefinementJSON `json:"rolled_back"`
	Stale      []RefinementJSON `json:"stale"`
	Failed     []RefinementJSON `json:"failed"`
}

func toRefinementJSON(x store.Refinement) RefinementJSON {
	return RefinementJSON{
		ID: x.ID, RoundID: x.RoundID, SessionID: x.SessionID, Trigger: x.Trigger, Kind: x.Kind,
		Target: x.Target, Before: x.Before, After: x.After, Rationale: x.Rationale,
		Status: x.Status, CreatedAt: x.CreatedAt,
	}
}

func toRefinementsJSON(xs []store.Refinement) []RefinementJSON {
	out := make([]RefinementJSON, 0, len(xs))
	for _, x := range xs {
		out = append(out, toRefinementJSON(x))
	}
	return out
}

// refinerOr503 returns the Refiner or answers 503.
func (s *Server) refinerOr503(w http.ResponseWriter) (*refine.Refiner, bool) {
	if s.refiner == nil {
		writeError(w, http.StatusServiceUnavailable, "refinement is not configured")
		return nil, false
	}
	return s.refiner, true
}

// decodeOptional reads a JSON body into v; an empty body is fine.
func decodeOptional(r *http.Request, v any) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// handleRefine runs a manual round now. It runs under the server's base
// context, not the request's: a client that disconnects mid-round must not
// cancel it between a ledger row and its file write.
func (s *Server) handleRefine(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.refinerOr503(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var body struct {
		Instructions string `json:"instructions"`
	}
	if err := decodeOptional(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	res, err := ref.Round(s.base, id, refine.TriggerManual, body.Instructions, 0)
	switch {
	case errors.Is(err, refine.ErrSubagent):
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	case errors.Is(err, refine.ErrBusy):
		writeError(w, http.StatusConflict, "%v", err)
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "refine: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, RefineResultJSON{
		RoundID: res.RoundID, Note: res.Note,
		Applied: toRefinementsJSON(res.Applied), Proposed: toRefinementsJSON(res.Proposed),
		Stale: toRefinementsJSON(res.Stale), Failed: toRefinementsJSON(res.Failed),
		Dropped: res.Dropped,
	})
}

func (s *Server) handleRefineRollback(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.refinerOr503(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var body struct {
		RoundID string `json:"round_id"`
	}
	if err := decodeOptional(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	res, err := ref.Rollback(s.base, id, body.RoundID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, RollbackJSON{
		RoundID: res.RoundID, RolledBack: toRefinementsJSON(res.RolledBack),
		Stale: toRefinementsJSON(res.Stale), Failed: toRefinementsJSON(res.Failed),
	})
}

func (s *Server) handleListRefinements(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	rows, err := s.store.Refinements(r.Context(), status, 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list refinements: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, toRefinementsJSON(rows))
}

func refinementID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad refinement id %q", r.PathValue("id"))
		return 0, false
	}
	return id, true
}

func (s *Server) handleAcceptRefinement(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.refinerOr503(w)
	if !ok {
		return
	}
	id, ok := refinementID(w, r)
	if !ok {
		return
	}
	row, err := ref.Accept(s.base, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, toRefinementJSON(row))
}

func (s *Server) handleRejectRefinement(w http.ResponseWriter, r *http.Request) {
	ref, ok := s.refinerOr503(w)
	if !ok {
		return
	}
	id, ok := refinementID(w, r)
	if !ok {
		return
	}
	if err := ref.Reject(s.base, id); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
