package daemon

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

type SessionJSON struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
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
	return SessionJSON{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
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
	}
	// An empty body is fine — a session with no title is legal.
	_ = json.NewDecoder(r.Body).Decode(&body)
	id, err := s.store.CreateSession(r.Context(), strings.TrimSpace(body.Title))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session: %v", err)
		return
	}
	writeJSON(w, http.StatusCreated, SessionJSON{ID: id, Title: body.Title,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
}

// findSession returns the session row, or writes a 404 and reports false.
func (s *Server) findSession(w http.ResponseWriter, r *http.Request, id string) (store.Session, bool) {
	sessions, err := s.store.ListSessions(r.Context(), 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read sessions: %v", err)
		return store.Session{}, false
	}
	for _, sess := range sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	writeError(w, http.StatusNotFound, "no session %s", id)
	return store.Session{}, false
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
	if err := s.startTurn(id, text, "http"); err != nil {
		s.hub.End(id)
		writeError(w, http.StatusInternalServerError, "start turn: %v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

// startTurn runs one turn on the SERVER's context and pumps its events into
// the hub. The caller must already hold the session's turn slot; startTurn
// releases it when the turn ends.
func (s *Server) startTurn(sessionID, text, client string) error {
	ctx := policy.WithSession(s.base, sessionID, policy.ProfileLocal)
	ctx, turn := sporetrace.StartTurn(ctx, sessionID, client)

	// Defer End before calling agent.Run so that even if Run panics,
	// we still release the session slot.
	defer func() {
		if r := recover(); r != nil {
			s.hub.End(sessionID)
			panic(r)
		}
	}()

	ch, err := s.agent.Run(ctx, sessionID, text)
	if err != nil {
		turn.End()
		return err
	}
	go func() {
		defer s.hub.End(sessionID)
		defer turn.End()
		for ev := range ch {
			if ev.Type == agent.EvError && ev.Err != nil {
				turn.RecordError(ev.Err)
			}
			s.hub.Publish(sessionID, FromAgent(ev))
		}
	}()
	return nil
}
