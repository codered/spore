package daemon

import (
	"context"
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

// Session states as the listing reports them. Blocked wins over working: a
// session waiting on a human is the one that needs attention.
const (
	SessionIdle    = "idle"
	SessionWorking = "working"
	SessionBlocked = "blocked"
)

type SessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Workspace is where the session is rooted. Clients show it so a human
	// can see which directory a detached session is operating on.
	Workspace string    `json:"workspace"`
	Source    string    `json:"source"`
	ParentID  string    `json:"parent_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// State and Pending are filled by the listing only.
	State   string `json:"state,omitempty"`
	Pending int    `json:"pending,omitempty"`
	// JobID is the scheduled job a job run belongs to; Unread is set while
	// the session holds messages nobody has opened.
	JobID  int64 `json:"job_id,omitempty"`
	Unread bool  `json:"unread,omitempty"`
}

type MessageJSON struct {
	Seq              int              `json:"seq"`
	Role             string           `json:"role"`
	Blocks           []provider.Block `json:"blocks"`
	Model            string           `json:"model,omitempty"`
	TokensIn         int              `json:"tokens_in,omitempty"`
	TokensOut        int              `json:"tokens_out,omitempty"`
	TokensCacheWrite int              `json:"tokens_cache_write,omitempty"`
	TokensCacheRead  int              `json:"tokens_cache_read,omitempty"`
	CostUSD          float64          `json:"cost_usd,omitempty"`
	CreatedAt        time.Time        `json:"created_at"`
}

type TranscriptJSON struct {
	Session        SessionJSON   `json:"session"`
	Messages       []MessageJSON `json:"messages"`
	SummaryThrough int           `json:"summary_through"`
	// Running reports whether a turn is in flight, so a client attaching
	// mid-turn knows to expect deltas rather than assuming it is idle.
	Running bool `json:"running"`
}

func toSessionJSON(s store.Session) SessionJSON {
	return SessionJSON{ID: s.ID, Title: s.Title, Workspace: s.Workspace,
		Source: s.Source, ParentID: s.ParentID,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
		JobID: s.JobID, Unread: s.Unread()}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	children := r.URL.Query().Get("children") == "1"
	sessions, err := s.store.ListSessions(r.Context(), 200, children)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sessions: %v", err)
		return
	}
	pending := s.pendingCounts(r.Context())
	out := make([]SessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		j := toSessionJSON(sess)
		j.Pending = pending[sess.ID]
		j.State = s.sessionState(r.Context(), sess, j.Pending)
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// pendingCounts counts unanswered approvals per session, crediting each to
// the session that asked and to every ancestor: a child's approval is
// answered through its root, so the root is blocked by it too.
func (s *Server) pendingCounts(ctx context.Context) map[string]int {
	counts := map[string]int{}
	pending, err := s.store.PendingCallsAll(ctx)
	if err != nil {
		return counts
	}
	for _, p := range pending {
		counts[p.SessionID]++
		anc, err := s.store.SessionAncestors(ctx, p.SessionID)
		if err != nil {
			continue
		}
		for _, a := range anc {
			counts[a]++
		}
	}
	return counts
}

// sessionState is blocked when a human is being waited on, working when a
// turn is in flight -- in the hub for a top-level session, in its run row for
// a sub-agent, whose turns never pass through the hub -- and idle otherwise.
func (s *Server) sessionState(ctx context.Context, sess store.Session, pending int) string {
	if pending > 0 {
		return SessionBlocked
	}
	if s.hub.Running(sess.ID) {
		return SessionWorking
	}
	if sess.ParentID != "" {
		if run, ok, err := s.store.SubagentRun(ctx, sess.ID); err == nil && ok && run.State == store.RunRunning {
			return SessionWorking
		}
	}
	return SessionIdle
}

// CreateSession is the one place a session's root is decided: the HTTP
// handler, the scheduler and the bridge all come through here, so the
// ceiling is checked once rather than in three places that can drift.
func (s *Server) CreateSession(ctx context.Context, title, requested, source string, profile policy.Profile) (string, error) {
	root, err := workspace.Root(workspace.Request{
		Requested:  requested,
		Ceiling:    s.cfg.Policy.Workspace,
		RemoteRoot: s.cfg.Policy.Profiles[string(policy.ProfileRemote)].Workspace,
		Remote:     profile == policy.ProfileRemote,
	})
	if err != nil {
		return "", err
	}
	id, err := s.store.CreateSessionFrom(ctx, title, root, source)
	if err != nil {
		return "", err
	}
	// Announce it on the global feed. The workspace is read back because an
	// empty root is allocated by the store.
	ws := root
	if sess, ok, err := s.store.Session(ctx, id); err == nil && ok {
		ws = sess.Workspace
	}
	s.hub.Publish(id, WireEvent{Type: WireSession, Title: title, Workspace: ws, Source: source})
	return id, nil
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
	// The profile is fixed to local here, never read from the request body:
	// this handler serves loopback HTTP clients, and a client must not be
	// able to name its own trust level. A remote caller (the bridge) reaches
	// CreateSession directly with policy.ProfileRemote instead of through
	// this handler.
	id, err := s.CreateSession(r.Context(), strings.TrimSpace(body.Title), strings.TrimSpace(body.Workspace), store.SourceChat, policy.ProfileLocal)
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

// handlePatchSession re-roots a session, or moves its summary boundary.
// The two fields are independent: a caller (the chat CLI) can send either or both.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var body struct {
		Workspace      *string `json:"workspace"`
		SummaryThrough *int    `json:"summary_through"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	if body.Workspace == nil && body.SummaryThrough == nil {
		writeError(w, http.StatusBadRequest, "at least one of workspace or summary_through is required")
		return
	}
	if body.Workspace != nil {
		root, err := workspace.Root(workspace.Request{
			Requested: strings.TrimSpace(*body.Workspace),
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
	}
	if body.SummaryThrough != nil {
		if *body.SummaryThrough < 0 {
			writeError(w, http.StatusBadRequest, "summary_through must be >= 0")
			return
		}
		// The boundary must be a real message's sequence number. Snapshot
		// skips every row at or below it and nothing ever moves it back, so a
		// boundary past the newest message hides the messages appended after
		// it too -- the next thing the user types included -- and the session
		// assembles an empty prompt for the rest of its life. Clamping here
		// closes that for every client at once.
		last, err := s.store.LastSeq(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "read the newest message: %v", err)
			return
		}
		through := min(*body.SummaryThrough, last)
		if err := s.store.SetSummaryThrough(r.Context(), id, through); err != nil {
			writeError(w, http.StatusInternalServerError, "move summary boundary: %v", err)
			return
		}
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
	_, through, err := s.store.Summary(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read summary boundary: %v", err)
		return
	}
	out := TranscriptJSON{Session: toSessionJSON(sess), Messages: []MessageJSON{}, SummaryThrough: through, Running: s.hub.Running(id)}
	for _, m := range rows {
		var blocks []provider.Block
		if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
			writeError(w, http.StatusInternalServerError, "decode message %d: %v", m.Seq, err)
			return
		}
		out.Messages = append(out.Messages, MessageJSON{
			Seq: m.Seq, Role: m.Role, Blocks: blocks, Model: m.Model,
			TokensIn: m.TokensIn, TokensOut: m.TokensOut, TokensCacheWrite: m.TokensCacheWrite, TokensCacheRead: m.TokensCacheRead, CostUSD: m.CostUSD,
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

// handleStop stops the session's running turn. The turn itself ends with a
// `stopped` event on the stream; this only asks for it.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if !s.hub.Stop(id, agent.ErrStopped) {
		writeError(w, http.StatusConflict, "nothing running in session %s", id)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
}

// CompactJSON is what a manual compaction reports back: how many messages
// were folded, and the assembled estimate either side of the fold. A client
// that only knew it succeeded could not tell a fold from a no-op.
type CompactJSON struct {
	Folded int `json:"folded"`
	Before int `json:"before"`
	After  int `json:"after"`
}

// handleClear removes the current transcript from the live model context.
// The store chooses the boundary so future message sequences remain visible.
func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if !s.hub.Begin(id) {
		writeError(w, http.StatusConflict, "session %s has a turn running", id)
		return
	}
	defer s.hub.End(id)
	through, err := s.store.ClearThroughLatestMessage(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "clear session: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"summary_through": through})
}

// handleCompact folds the session now. It calls Compact rather than
// MaybeCompact on purpose: /compact is a manual override, so it must fold
// whatever lies outside the protected recent window even when the session is
// far below the auto-compaction threshold. MaybeCompact keeps its own job,
// which is the threshold test at the end of every turn.
func (s *Server) handleCompact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.findSession(w, r, id)
	if !ok {
		return
	}
	// Compaction rewrites the summary boundary a running turn is reading, so
	// it takes the turn slot rather than merely checking for one: between a
	// Running() check and the fold, a turn could start and snapshot the
	// boundary this is about to move.
	if !s.hub.Begin(id) {
		writeError(w, http.StatusConflict, "session %s has a turn running", id)
		return
	}
	defer s.hub.End(id)
	// The session's own root, exactly as a turn supplies it: Snapshot renders
	// the environment section and the skills index from it, and both are part
	// of the estimate this reports.
	ctx := policy.WithSession(s.base, policy.Session{ID: id, Workspace: sess.Workspace})
	folded, before, after, err := s.agent.Compact(ctx, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "compact: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, CompactJSON{Folded: folded, Before: before, After: after})
}

// startTurn runs one turn on the SERVER's context and pumps its events into
// the hub. The caller must already hold the session's turn slot; startTurn
// releases it when the turn ends. The profile is the caller's trust level and
// decides which ruleset the policy engine applies — an HTTP client on
// loopback is local, a chat bridge is remote.
func (s *Server) startTurn(sessionID, text, client string, profile policy.Profile) error {
	return s.startTurnThen(sessionID, text, client, profile, nil)
}

// startTurnThen is startTurn with a hook for the turn's end. then receives
// the event that ended it -- turn_done, error or stopped -- and runs after the
// slot is released. It is not called when the turn fails to start.
func (s *Server) startTurnThen(sessionID, text, client string, profile policy.Profile, then func(end WireEvent)) error {
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
	turnCtx, cancel := context.WithCancelCause(s.base)
	ctx := policy.WithSession(turnCtx, policy.Session{
		ID: sessionID, Profile: profile, Workspace: sess.Workspace,
	})
	ctx, turn = sporetrace.StartTurn(ctx, sessionID, client)
	s.hub.SetCancel(sessionID, cancel)

	ch, err := s.agent.Run(ctx, sessionID, text)
	if err != nil {
		cancel(nil)
		turn.End()
		return err
	}
	// Published before the pump starts, so it precedes every event of the turn.
	s.hub.Publish(sessionID, WireEvent{Type: WireTurnStarted})
	go func() {
		// end is the last event that can end a turn. A channel that closes
		// without one is a turn that ended without saying how: an error.
		end := WireEvent{Type: WireError, Error: "the turn ended without finishing"}
		if then != nil {
			// Deferred first so it runs last, after the slot is released. It
			// runs outside the recover below, so it carries its own: a hook
			// that panics must not take the daemon with it.
			defer func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Error("panic after turn", "session", sessionID, "panic", r)
					}
				}()
				then(end)
			}()
		}
		defer s.hub.End(sessionID)
		defer cancel(nil)
		defer turn.End()

		// Recover in the pump goroutine so a panic in event handling
		// does not crash the daemon. Publish an error to the session.
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in turn pump", "session", sessionID, "panic", r)
				end = WireEvent{
					Type:  WireError,
					Error: "turn crashed: " + fmt.Sprint(r),
				}
				s.hub.Publish(sessionID, end)
			}
		}()

		for ev := range ch {
			if ev.Type == agent.EvError && ev.Err != nil {
				turn.RecordError(ev.Err)
			}
			w := FromAgent(ev)
			switch w.Type {
			case WireTurnDone, WireError, WireStopped:
				end = w
			}
			s.hub.Publish(sessionID, w)
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
