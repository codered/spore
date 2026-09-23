package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"

	"github.com/codered/spore/internal/store"
)

// Cleaner removes a chat surface's copy of sessions that are about to be
// deleted: the Discord bridge deletes the threads it opened for them. It is
// called before the rows go, while the bindings that say where they live
// still exist, and reports what it did and what it could not do.
type Cleaner interface {
	ForgetSessions(ctx context.Context, sessions []store.Session) []string
}

// SetCleaner attaches the chat surface to clean on a delete that asks for
// it. Call it before Run: it is read without a lock.
func (s *Server) SetCleaner(c Cleaner) { s.cleaner = c }

// DeleteSessionsJSON is POST /api/sessions/delete's answer: every session id
// removed (sub-agents included) and anything the chat surface reported.
type DeleteSessionsJSON struct {
	Deleted []string `json:"deleted"`
	Notes   []string `json:"notes"`
}

// handleDeleteSessions deletes the named sessions, or all of them, each with
// its whole sub-agent tree. It refuses while a turn runs anywhere in what
// would be deleted: a turn writing into a session that no longer exists fails
// halfway through a tool call. With discord set, the bridge deletes its copy
// first.
func (s *Server) handleDeleteSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs     []string `json:"ids"`
		All     bool     `json:"all"`
		Discord bool     `json:"discord"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	if !body.All && len(body.IDs) == 0 {
		writeError(w, http.StatusBadRequest, "name the sessions to delete, or set all")
		return
	}
	ctx := r.Context()
	doomed, err := s.doomedSessions(ctx, body.IDs, body.All)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	for _, sess := range doomed {
		if s.turnRunning(ctx, sess) {
			writeError(w, http.StatusConflict, "session %s has a turn running; stop it first", sess.ID)
			return
		}
	}

	out := DeleteSessionsJSON{Deleted: []string{}, Notes: []string{}}
	if body.Discord && len(doomed) > 0 {
		if s.cleaner == nil {
			out.Notes = append(out.Notes, "Discord was not touched: no Discord bridge is connected")
		} else {
			out.Notes = append(out.Notes, s.cleaner.ForgetSessions(ctx, doomed)...)
		}
	}
	var deleted []string
	if body.All {
		deleted, err = s.store.DeleteAllSessions(ctx)
	} else {
		deleted, err = s.store.DeleteSessions(ctx, body.IDs)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete sessions: %v", err)
		return
	}
	for _, id := range deleted {
		s.hub.Publish(id, WireEvent{Type: WireSessionDeleted})
	}
	if deleted != nil {
		out.Deleted = deleted
	}
	writeJSON(w, http.StatusOK, out)
}

// doomedSessions is every session a delete would remove.
func (s *Server) doomedSessions(ctx context.Context, ids []string, all bool) ([]store.Session, error) {
	if all {
		list, err := s.store.ListSessions(ctx, math.MaxInt32, true)
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		return list, nil
	}
	seen := map[string]bool{}
	var out []store.Session
	for _, id := range ids {
		tree, err := s.store.SessionTree(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, t := range tree {
			if seen[t] {
				continue
			}
			seen[t] = true
			sess, found, err := s.store.Session(ctx, t)
			if err != nil {
				return nil, err
			}
			if found {
				out = append(out, sess)
			}
		}
	}
	return out, nil
}

// turnRunning reports a turn in flight: in the hub for a top-level session,
// in its run row for a sub-agent, whose turns never pass through the hub.
func (s *Server) turnRunning(ctx context.Context, sess store.Session) bool {
	if s.hub.Running(sess.ID) {
		return true
	}
	if sess.ParentID != "" {
		if run, ok, err := s.store.SubagentRun(ctx, sess.ID); err == nil && ok && run.State == store.RunRunning {
			return true
		}
	}
	return false
}

// handleSeen records that a human opened the session, so it stops reading
// as unread.
func (s *Server) handleSeen(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if err := s.store.MarkSessionSeen(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "seen"})
}
