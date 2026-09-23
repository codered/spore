package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool/schedule"
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
	// OriginSessionID is the chat that hears about the job's runs; Notify is
	// how (ask, each, failures, none).
	OriginSessionID string `json:"origin_session_id,omitempty"`
	Notify          string `json:"notify,omitempty"`
}

func toJobJSON(j store.Job) JobJSON {
	var lastRun *time.Time
	if !j.LastRun.IsZero() {
		lastRun = &j.LastRun
	}
	return JobJSON{
		ID: j.ID, Kind: j.Kind, Spec: j.Spec, Prompt: j.Prompt, Enabled: j.Enabled,
		NextRun: j.NextRun, LastRun: lastRun, LastSessionID: j.LastSessionID,
		OriginSessionID: j.OriginSessionID, Notify: j.Notify,
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
		// Session is optional: the chat that hears about the job's runs.
		Session string `json:"session"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	if body.Session != "" {
		if _, ok := s.findSession(w, r, body.Session); !ok {
			return
		}
	}
	job, err := scheduler.CreateJob(r.Context(), s.store, body.Spec, body.Prompt, body.Session, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, toJobJSON(job))
}

// Run statuses as GET /api/jobs/{id}/runs reports them.
const (
	RunOK      = "ok"
	RunFailed  = "failed"
	RunRunning = "running"
)

// JobRunJSON is one run of a job: the session it ran in, how it ended, and
// its final reply. Failed covers a run that errored or was stopped: either
// way it left no reply.
type JobRunJSON struct {
	Session SessionJSON `json:"session"`
	Status  string      `json:"status"`
	Output  string      `json:"output,omitempty"`
}

// handleJobRuns lists every run of a job, newest first, with each run's
// output, so a client can show them side by side.
func (s *Server) handleJobRuns(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "job id must be a number: %v", err)
		return
	}
	if _, found, err := s.store.Job(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "read job: %v", err)
		return
	} else if !found {
		writeError(w, http.StatusNotFound, "no job %d", id)
		return
	}
	runs, err := s.store.JobRuns(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := make([]JobRunJSON, 0, len(runs))
	for _, sess := range runs {
		reply, err := schedule.LastReply(r.Context(), s.store, sess.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read run %s: %v", sess.ID, err)
			return
		}
		status := RunFailed
		switch {
		case s.hub.Running(sess.ID):
			status = RunRunning
		case reply != "":
			status = RunOK
		}
		out = append(out, JobRunJSON{Session: toSessionJSON(sess), Status: status, Output: reply})
	}
	writeJSON(w, http.StatusOK, out)
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
	sessionID, err := s.CreateSession(ctx, title, "", store.SourceJob, policy.ProfileLocal)
	if err != nil {
		return "", err
	}
	// The run still happens if this fails; it only lands ungrouped.
	if err := s.store.SetSessionJob(ctx, sessionID, job.ID); err != nil {
		slog.Warn("could not tag the job run with its job", "job", job.ID, "session", sessionID, "err", err)
	}
	if !s.hub.Begin(sessionID) {
		return sessionID, errSessionBusy
	}
	then := func(end WireEvent) { s.afterJobRun(job.ID, sessionID, end) }
	if err := s.startTurnThen(sessionID, job.Prompt, "job", policy.ProfileLocal, then); err != nil {
		s.hub.End(sessionID)
		return sessionID, err
	}
	return sessionID, nil
}
