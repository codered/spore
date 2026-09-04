package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

type JobJSON struct {
	ID            int64      `json:"id"`
	Kind          string     `json:"kind"`
	Spec          string     `json:"spec"`
	Prompt        string     `json:"prompt"`
	Enabled       bool       `json:"enabled"`
	NextRun       time.Time  `json:"next_run"`
	LastRun       *time.Time `json:"last_run,omitempty"`
	LastSessionID string     `json:"last_session_id,omitempty"`
}

func toJobJSON(j store.Job) JobJSON {
	var lastRun *time.Time
	if !j.LastRun.IsZero() {
		lastRun = &j.LastRun
	}
	return JobJSON{
		ID: j.ID, Kind: j.Kind, Spec: j.Spec, Prompt: j.Prompt, Enabled: j.Enabled,
		NextRun: j.NextRun, LastRun: lastRun, LastSessionID: j.LastSessionID,
	}
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list jobs: %v", err)
		return
	}
	out := make([]JobJSON, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobJSON(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec   string `json:"spec"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	job, err := scheduler.CreateJob(r.Context(), s.store, body.Spec, body.Prompt, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, toJobJSON(job))
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "job id must be a number: %v", err)
		return
	}
	if err := s.store.SetJobEnabled(r.Context(), id, false); err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// StartJob implements scheduler.Runner. A job always opens a FRESH session,
// so a recurring job never accumulates one unbounded thread, and the policy
// engine sees the turn exactly as it sees a human's.
func (s *Server) StartJob(ctx context.Context, job store.Job) (string, error) {
	title := job.Prompt
	if len(title) > 60 {
		title = title[:60]
	}
	// A job has no directory of its own, so it gets a session directory --
	// the same treatment as the web UI and the bridge.
	sessionID, err := s.CreateSession(ctx, title, "", policy.ProfileLocal)
	if err != nil {
		return "", err
	}
	if !s.hub.Begin(sessionID) {
		return sessionID, errSessionBusy
	}
	if err := s.startTurn(sessionID, job.Prompt, "job", policy.ProfileLocal); err != nil {
		s.hub.End(sessionID)
		return sessionID, err
	}
	return sessionID, nil
}
