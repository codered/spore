package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
	"github.com/codered/spore/internal/workspace"
)

type SessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Workspace is where the session is rooted. Clients show it so a human
	// can see which directory a detached session is operating on.
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MessageJSON struct {
	Seq       int              `json:"seq"`
	Role      string           `json:"role"`
	Blocks    []provider.Block `json:"blocks"`
	Model     string           `json:"model,omitempty"`
	TokensIn  int              `json:"tokens_in,omitempty"`
	TokensOut int              `json:"tokens_out,omitempty"`
	CostUSD   float64          `json:"cost_usd,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

type TranscriptJSON struct {
	Session  SessionJSON   `json:"session"`
	Messages []MessageJSON `json:"messages"`
	// Running reports whether a turn is in flight, so a client attaching
	// mid-turn knows to expect deltas rather than assuming it is idle.
	Running bool `json:"running"`
}

func toSessionJSON(s store.Session) SessionJSON {
	return SessionJSON{ID: s.ID, Title: s.Title, Workspace: s.Workspace,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sessions: %v", err)
		return
	}
	out := make([]SessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionJSON(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		// Workspace is optional. Omitting it is a creator saying it has no
		// directory of its own -- the web UI, a script -- and it gets a
		// session directory. Naming one outside the ceiling is an error, not
		// a fallback: a client that asked for the wrong place must be told.
		Workspace string `json:"workspace"`
	}
	// An empty body is fine -- a session with no title is legal.
	_ = json.NewDecoder(r.Body).Decode(&body)
	id, err := s.CreateSession(r.Context(), strings.TrimSpace(body.Title), strings.TrimSpace(body.Workspace), policy.ProfileLocal)
	if err != nil {
		writeError(w, http.StatusBadRequest, "create session: %v", err)
		return
	}
	sess, found, err := s.store.Session(r.Context(), id)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "read back session %s: %v", id, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSessionJSON(sess))
}

// handlePatchSession re-roots a session. The root is fixed at creation
// everywhere else; this exists for the CLI's deliberate "--workspace on a
// resume", and it is bounded by the same ceiling as creation.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var body struct {
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	root, err := workspace.Root(workspace.Request{
		Requested: strings.TrimSpace(body.Workspace),
		Ceiling:   s.cfg.Policy.Workspace,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if root == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}
	if err := s.store.SetSessionWorkspace(r.Context(), id, root); err != nil {
		writeError(w, http.StatusInternalServerError, "re-root session: %v", err)
		return
	}
	sess, _, err := s.store.Session(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read back session %s: %v", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionJSON(sess))
}

// findSession returns the session row, or writes a 404 and reports false.
func (s *Server) findSession(w http.ResponseWriter, r *http.Request, id string) (store.Session, bool) {
	sess, found, err := s.store.Session(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read session: %v", err)
		return store.Session{}, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "no session %s", id)
		return store.Session{}, false
	}
	return sess, true
}

func (s *Server) handleShowSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.findSession(w, r, id)
	if !ok {
		return
	}
	rows, err := s.store.Messages(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read messages: %v", err)
		return
	}
	out := TranscriptJSON{Session: toSessionJSON(sess), Messages: []MessageJSON{}, Running: s.hub.Running(id)}
	for _, m := range rows {
		var blocks []provider.Block
		if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
			writeError(w, http.StatusInternalServerError, "decode message %d: %v", m.Seq, err)
			return
		}
		out.Messages = append(out.Messages, MessageJSON{
			Seq: m.Seq, Role: m.Role, Blocks: blocks, Model: m.Model,
			TokensIn: m.TokensIn, TokensOut: m.TokensOut, CostUSD: m.CostUSD,
			CreatedAt: m.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if !s.hub.Begin(id) {
		writeError(w, http.StatusConflict, "session %s already has a turn running", id)
		return
	}
	if err := s.startTurn(id, text, "http", policy.ProfileLocal); err != nil {
		s.hub.End(id)
		writeError(w, http.StatusInternalServerError, "start turn: %v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

// startTurn runs one turn on the SERVER's context and pumps its events into
// the hub. The caller must already hold the session's turn slot; startTurn
// releases it when the turn ends. The profile is the caller's trust level and
// decides which ruleset the policy engine applies — an HTTP client on
// loopback is local, a chat bridge is remote.
func (s *Server) startTurn(sessionID, text, client string, profile policy.Profile) error {
	var turn sporetrace.Span
	// Recover before any operations so panics in policy.WithSession or
	// sporetrace.StartTurn are also caught.
	defer func() {
		if r := recover(); r != nil {
			if turn != nil {
				turn.End()
			}
			s.hub.End(sessionID)
			slog.Error("panic in startTurn setup", "session", sessionID, "panic", r)
			panic(r)
		}
	}()

	// The session's root is read once per turn and travels on the context:
	// the tools, the prompt's environment section and the policy engine all
	// take it from there, so there is one answer to "where is this session
	// working" and it comes from the row.
	sess, found, err := s.store.Session(s.base, sessionID)
	if err != nil {
		return fmt.Errorf("read session %s: %w", sessionID, err)
	}
	if !found {
		return fmt.Errorf("no session %s", sessionID)
	}
	// A directory spore allocated is spore's to create, and it is created
	// here rather than at creation so a session that is opened and never used
	// leaves nothing on disk. A directory a human named is never created:
	// a typo must fail loudly, not silently make an empty directory.
	if workspace.Allocated(s.store.SessionsDir(), sess.Workspace) {
		if err := os.MkdirAll(sess.Workspace, 0o700); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	ctx := policy.WithSession(s.base, policy.Session{
		ID: sessionID, Profile: profile, Workspace: sess.Workspace,
	})
	ctx, turn = sporetrace.StartTurn(ctx, sessionID, client)

	ch, err := s.agent.Run(ctx, sessionID, text)
	if err != nil {
		turn.End()
		return err
	}
	go func() {
		defer s.hub.End(sessionID)
		defer turn.End()

		// Recover in the pump goroutine so a panic in event handling
		// does not crash the daemon. Publish an error to the session.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in turn pump", "session", sessionID, "panic", r)
				s.hub.Publish(sessionID, WireEvent{
					Type:  WireError,
					Error: "turn crashed: " + fmt.Sprint(r),
				})
			}
		}()

		for ev := range ch {
			if ev.Type == agent.EvError && ev.Err != nil {
				turn.RecordError(ev.Err)
			}
			s.hub.Publish(sessionID, FromAgent(ev))
		}
	}()
	return nil
}

// ErrTurnRunning reports that the session already has a turn in flight. Two
// clients posting at once must not interleave two turns into one transcript.
var ErrTurnRunning = errors.New("the session already has a turn running")

// StartTurn runs a turn for a non-HTTP client. It claims the session's turn
// slot, so callers must not call hub.Begin themselves. The turn runs on the
// server's context and outlives whatever started it (spec invariant 2), which
// is why no caller's context is accepted here.
func (s *Server) StartTurn(sessionID, text, client string, profile policy.Profile) error {
	if !s.hub.Begin(sessionID) {
		return ErrTurnRunning
	}
	if err := s.startTurn(sessionID, text, client, profile); err != nil {
		s.hub.End(sessionID)
		return err
	}
	return nil
}
