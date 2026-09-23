package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool/schedule"
)

// SchedulerTag opens every message the scheduler puts in a chat as the
// user. Clients show such a message as spore's own, not as something the
// person typed.
const SchedulerTag = "[spore scheduler]"

// checkInReplyLimit bounds how much of a run's reply the check-in quotes to
// the model. The model quotes it briefly anyway; job_output has all of it.
const checkInReplyLimit = 1500

// noteTimeout bounds one note's delivery, Discord included. A note is sent
// from the end of a job run and must not hold that goroutine for long.
const noteTimeout = 15 * time.Second

// Notifier is a chat surface that can speak in a session's chat: post a
// note there, or render a turn spore started itself. The Discord bridge is
// one; a session it has no channel for is simply not delivered to.
type Notifier interface {
	// Deliver posts text to the channel bound to the session, if any.
	Deliver(ctx context.Context, sessionID, text string)
	// Follow renders the session's next turn into its bound channel, if
	// any. It is called before the turn starts; stop detaches it when the
	// turn never does. stop is never nil.
	Follow(sessionID string) (stop func())
}

// SetNotifier attaches the chat surface job reports reach besides the TUI.
// Call it before Run and before the scheduler starts: it is read without a
// lock.
func (s *Server) SetNotifier(n Notifier) { s.notifier = n }

// afterJobRun reports a finished job run to the chat that created the job.
// The first successful run checks in with a model turn there; every later
// run follows the job's notify mode with a plain note. A job with no origin
// reports only to the Jobs folder.
func (s *Server) afterJobRun(jobID int64, runSession string, end WireEvent) {
	ctx, cancel := context.WithTimeout(s.base, noteTimeout)
	defer cancel()
	job, found, err := s.store.Job(ctx, jobID)
	if err != nil || !found {
		if err != nil {
			slog.Warn("could not read the job to report its run", "job", jobID, "err", err)
		}
		return
	}
	if job.OriginSessionID == "" {
		return
	}
	ok := end.Type == WireTurnDone
	if !job.CheckedIn {
		if !ok {
			return // only a success checks in
		}
		claimed, err := s.store.ClaimJobCheckIn(ctx, job.ID)
		if err != nil {
			slog.Warn("could not claim the job's check-in", "job", job.ID, "err", err)
			return
		}
		if claimed {
			s.checkIn(ctx, job, runSession)
		}
		return
	}
	switch job.Notify {
	case store.NotifyEach:
	case store.NotifyFailures:
		if ok {
			return
		}
	default:
		return
	}
	if err := s.postNote(ctx, job.OriginSessionID, runNote(job, end)); err != nil {
		slog.Warn("could not post the job's note", "job", job.ID, "err", err)
	}
}

// runNote is the one line a later run leaves in the origin chat. It says
// the job ran, never what it said: that is in the Jobs folder.
func runNote(job store.Job, end WireEvent) string {
	at := job.LastRun
	if at.IsZero() {
		at = time.Now()
	}
	when := at.UTC().Format("15:04") + " UTC"
	switch end.Type {
	case WireTurnDone:
		return fmt.Sprintf("⏰ job %d ran at %s — ok · it's in the Jobs folder", job.ID, when)
	case WireStopped:
		return fmt.Sprintf("⏰ job %d was stopped at %s · it's in the Jobs folder", job.ID, when)
	default:
		return fmt.Sprintf("⏰ job %d failed at %s: %s · it's in the Jobs folder", job.ID, when, firstLine(end.Error))
	}
}

// postNote writes a note into the session's transcript, shows it to every
// client watching, and posts it to the session's Discord channel if it has
// one. A note is not a model message, so it may land mid-turn.
func (s *Server) postNote(ctx context.Context, sessionID, text string) error {
	blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: text}})
	if err != nil {
		return err
	}
	if _, err := s.store.AppendMessage(ctx, store.Message{SessionID: sessionID, Role: store.RoleNote, BlocksJSON: blocks}); err != nil {
		return err
	}
	s.hub.Publish(sessionID, WireEvent{Type: WireJobNote, Text: text})
	if s.notifier != nil {
		s.notifier.Deliver(ctx, sessionID, text)
	}
	return nil
}

// checkIn starts a turn in the origin chat that tells the person the job
// ran and asks how to report later runs. It is a model turn so the question
// sits in the model's history: when the person answers "only failures", the
// model knows what they are answering. A busy chat gets it when its current
// turn ends, never alongside it.
func (s *Server) checkIn(ctx context.Context, job store.Job, runSession string) {
	reply, err := schedule.LastReply(ctx, s.store, runSession)
	if err != nil {
		slog.Warn("could not read the job's reply for its check-in", "job", job.ID, "err", err)
	}
	text := checkInText(job, reply)
	origin := job.OriginSessionID
	s.hub.Enqueue(origin, func() {
		sess, found, err := s.store.Session(s.base, origin)
		if err != nil || !found {
			// Deleted while the check-in waited: the Jobs folder has it.
			s.hub.End(origin)
			return
		}
		stop := func() {}
		if s.notifier != nil {
			stop = s.notifier.Follow(origin)
		}
		if err := s.startTurn(origin, text, "scheduler", profileFor(sess)); err != nil {
			stop()
			s.hub.End(origin)
			slog.Warn("could not start the job's check-in", "job", job.ID, "session", origin, "err", err)
		}
	})
}

// profileFor is the trust level a turn spore starts in a session runs at:
// the one its own chat runs at. Only a session opened locally is local; a
// Discord chat, or one whose origin is unknown, is remote.
func profileFor(sess store.Session) policy.Profile {
	switch sess.Source {
	case store.SourceChat, store.SourceJob:
		return policy.ProfileLocal
	default:
		return policy.ProfileRemote
	}
}

// checkInText is the event the model reads as the person's turn. It is
// marked as the scheduler's so neither the model nor a client mistakes it
// for something the person typed.
func checkInText(job store.Job, reply string) string {
	at := job.LastRun
	if at.IsZero() {
		at = time.Now()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s Job %d (%q) ran for the first time at %s UTC and succeeded.",
		SchedulerTag, job.ID, clip(firstLine(job.Prompt), 60), at.UTC().Format("15:04"))
	if reply = strings.TrimSpace(reply); reply != "" {
		fmt.Fprintf(&b, " Its reply was: %q.", clip(reply, checkInReplyLimit))
	}
	fmt.Fprintf(&b, " Tell the user it ran, quote the reply briefly, and ask how they want to hear about"+
		" later runs: each run, only failures, or not at all. It is always in the Jobs folder, and"+
		" job_output shows the last run. When they answer, record it with schedule_notify (id %d).", job.ID)
	return b.String()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
