package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// AttachCompanion supplies the companion service. Nil leaves the companion
// routes answering 503 and the reply hook off.
func (s *Server) AttachCompanion(svc *companion.Service) { s.companion = svc }

// Companion is the attached companion service, or nil.
func (s *Server) Companion() *companion.Service { return s.companion }

// SetCompanionChannel sets where outreach is sent besides the companion
// session. Call it before the heartbeat starts: it is read without a lock.
func (s *Server) SetCompanionChannel(ch companion.Channel) { s.companionChannel = ch }

// AttachHeartbeat supplies the heartbeat serve runs.
func (s *Server) AttachHeartbeat(h *companion.Heartbeat) { s.heartbeat = h }

// Heartbeat is the attached heartbeat, or nil.
func (s *Server) Heartbeat() *companion.Heartbeat { return s.heartbeat }

// CompanionDeliverer is the heartbeat's way into the companion session.
func (s *Server) CompanionDeliverer() companion.Deliverer { return companionDeliverer{s} }

type companionDeliverer struct{ s *Server }

// EnsureSession returns the companion session, opening a new one when there
// is none or the user deleted it. A new session takes over every active
// watch whose origin the delete cleared, so their runs report here.
func (d companionDeliverer) EnsureSession(ctx context.Context) (string, error) {
	s := d.s
	id, ok, err := s.store.CompanionState(ctx, companion.StateSessionID)
	if err != nil {
		return "", err
	}
	if ok && id != "" {
		_, found, err := s.store.Session(ctx, id)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}
	id, err = s.CreateSession(ctx, "spore", "", store.SourceCompanion, policy.ProfileLocal)
	if err != nil {
		return "", err
	}
	if err := s.store.SetCompanionState(ctx, companion.StateSessionID, id); err != nil {
		return "", err
	}
	active, err := s.store.Interests(ctx, store.InterestActive)
	if err != nil {
		return "", err
	}
	for _, in := range active {
		if in.WatchJobID == 0 {
			continue
		}
		if _, err := s.store.AttachJobOrigin(ctx, in.WatchJobID, id); err != nil {
			slog.Warn("companion: could not point a watch at the new session", "job", in.WatchJobID, "err", err)
		}
	}
	return id, nil
}

// Deliver records o in the store and in the companion session, then sends it
// on the channel. The session's turn slot is held while the messages are
// appended, so they can never land inside a turn; while one runs, Deliver
// writes nothing and returns companion.ErrBusy. A failed channel send still
// counts: the message is in the terminal, recorded as terminal-fallback.
func (d companionDeliverer) Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error) {
	s := d.s
	sid, err := d.EnsureSession(ctx)
	if err != nil {
		return store.Outreach{}, err
	}
	if !s.hub.Begin(sid) {
		return store.Outreach{}, companion.ErrBusy
	}
	ch := s.companionChannel
	o.Channel = store.OutreachChannelTerminal
	if ch != nil {
		o.Channel = ch.Name()
	}
	o.ID, err = s.store.AddOutreach(ctx, o)
	if err == nil {
		err = s.appendOutreach(ctx, sid, o)
	}
	s.hub.End(sid)
	if err != nil {
		return store.Outreach{}, err
	}
	o.Outcome = store.OutcomePending
	s.hub.Publish(sid, WireEvent{Type: WireJobNote, Text: o.Text})
	if ch != nil {
		if err := ch.Send(ctx, sid, o); err != nil {
			slog.Warn("companion: channel delivery failed; the message is in the terminal", "outreach", o.ID, "err", err)
			o.Channel = store.OutreachChannelFallback
			if err := s.store.SetOutreachChannel(ctx, o.ID, o.Channel); err != nil {
				slog.Warn("companion: could not record the fallback", "outreach", o.ID, "err", err)
			}
		}
	}
	return o, nil
}

// appendOutreach writes the outreach into the companion session as a
// scheduler-tagged user cue followed by the message as spore's own reply.
// The pair keeps the history alternating -- a session may not open on an
// assistant message -- and tells the model, when the user answers, what it
// said and why.
func (s *Server) appendOutreach(ctx context.Context, sid string, o store.Outreach) error {
	cue := fmt.Sprintf("%s companion outreach #%d (%s), started by spore at %s UTC. The next message is what you sent; the user may answer it.",
		SchedulerTag, o.ID, o.Kind, o.SentAt.UTC().Format("2006-01-02 15:04"))
	if o.Kind == store.OutreachProposal {
		cue += " It carries Yes / Not now / Never buttons; only a button press starts a watch."
	}
	for _, m := range []struct{ role, text string }{{string(provider.RoleUser), cue}, {string(provider.RoleAssistant), o.Text}} {
		blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: m.text}})
		if err != nil {
			return err
		}
		if _, err := s.store.AppendMessage(ctx, store.Message{SessionID: sid, Role: m.role, BlocksJSON: blocks}); err != nil {
			return err
		}
	}
	return nil
}

// AnswerProposal answers a proposal's button and leaves the answer as a note
// in the companion session, which the bridge also posts to its thread. The
// HTTP route and the Discord buttons both come through here.
func (s *Server) AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error) {
	if s.companion == nil {
		return "", errCompanionOff
	}
	msg, err := s.companion.AnswerProposal(ctx, outreachID, choice)
	if err != nil {
		return "", err
	}
	if sid, ok, err := s.store.CompanionState(ctx, companion.StateSessionID); err == nil && ok && sid != "" {
		if err := s.postNote(ctx, sid, msg); err != nil {
			slog.Warn("companion: could not post the proposal answer", "outreach", outreachID, "err", err)
		}
	}
	return msg, nil
}

var errCompanionOff = errors.New("the companion is not enabled")

func (s *Server) handleAnswerProposal(w http.ResponseWriter, r *http.Request) {
	if s.companion == nil {
		writeError(w, http.StatusServiceUnavailable, "%v", errCompanionOff)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "outreach id must be a number: %v", err)
		return
	}
	var body struct {
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	msg, err := s.AnswerProposal(r.Context(), id, body.Choice)
	switch {
	case errors.Is(err, companion.ErrNotProposal):
		writeError(w, http.StatusConflict, "%v", err)
	case err != nil:
		writeError(w, http.StatusBadRequest, "%v", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"message": msg})
	}
}

// CompanionStatusJSON is GET /api/companion/status.
type CompanionStatusJSON struct {
	Enabled        bool       `json:"enabled"`
	Channel        string     `json:"channel"`
	PausedUntil    *time.Time `json:"paused_until,omitempty"`
	Budget         float64    `json:"budget"`
	SpentToday     int        `json:"spent_today"`
	LastReflection *time.Time `json:"last_reflection,omitempty"`
	LastResult     string     `json:"last_result,omitempty"`
	Failures       int        `json:"failures"`
	SessionID      string     `json:"session_id,omitempty"`
}

func (s *Server) handleCompanionStatus(w http.ResponseWriter, r *http.Request) {
	if s.companion == nil {
		writeError(w, http.StatusServiceUnavailable, "%v", errCompanionOff)
		return
	}
	st, err := s.companion.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := CompanionStatusJSON{Enabled: st.Enabled, Channel: st.Channel, Budget: st.Budget, SpentToday: st.SpentToday,
		LastResult: st.LastResult, Failures: st.Failures, SessionID: st.SessionID}
	if !st.PausedUntil.IsZero() {
		out.PausedUntil = &st.PausedUntil
	}
	if !st.LastReflection.IsZero() {
		out.LastReflection = &st.LastReflection
	}
	writeJSON(w, http.StatusOK, out)
}
