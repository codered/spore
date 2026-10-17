package companion

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool/schedule"
)

// heldAlertWindow is how far back RetryHeldAlerts looks for held alerts.
const heldAlertWindow = 30 * 24 * time.Hour

// Verdict is what a watch run decided: an alert for the user, or nothing to
// report. Kind is store.VerdictAlert or store.VerdictNothing.
type Verdict struct {
	Kind    string
	Message string
}

// VerdictSuffix is appended to a watch prompt so the run ends with a verdict
// line that ParseVerdict can read.
const VerdictSuffix = "\n\nEnd your work with a verdict. Begin your reply with exactly one of these lines:\n" +
	"ALERT: <what to tell the user, in a few lines>\n" +
	"NOTHING: <one line on what you checked>\n" +
	"Use ALERT only when what you found crosses the goal's bar."

// alertClip is how many runes of an alert reach the chat. The rest waits in
// the Jobs folder.
const alertClip = 1200

// alertFooter follows a clipped alert.
const alertFooter = "\n\n· the rest is in the Jobs folder"

// ParseVerdict reads a watch run's reply. A reply that does not start with a
// verdict marker is treated as an alert, so the user never loses a finding.
func ParseVerdict(reply string) Verdict {
	body := stripFence(strings.TrimSpace(reply))
	lines := strings.Split(body, "\n")
	first := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return Verdict{Kind: store.VerdictAlert, Message: ""}
	}
	line := strings.TrimLeft(lines[first], "*#_> \t")
	rest := strings.Join(lines[first+1:], "\n")
	if msg, ok := markerText(line, "NOTHING"); ok {
		return Verdict{Kind: store.VerdictNothing, Message: msg}
	}
	if msg, ok := markerText(line, "ALERT"); ok {
		if rest != "" {
			msg += "\n" + rest
		}
		return Verdict{Kind: store.VerdictAlert, Message: strings.TrimSpace(msg)}
	}
	return Verdict{Kind: store.VerdictAlert, Message: strings.TrimSpace(reply)}
}

// stripFence removes one surrounding ``` fence, with or without a language tag.
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	s = s[nl+1:]
	if t := strings.TrimSpace(s); strings.HasSuffix(t, "```") {
		s = t[:len(t)-3]
	}
	return s
}

// markerText reports whether line starts with word followed by a colon, and
// returns the text after that colon. Emphasis marks may sit between the word
// and the colon, as in **NOTHING**: or __ALERT__:, and after it.
func markerText(line, word string) (string, bool) {
	if len(line) < len(word) || !strings.EqualFold(line[:len(word)], word) {
		return "", false
	}
	rest := strings.TrimLeft(line[len(word):], "*_")
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimLeft(rest[1:], "*_")), true
}

// ResearchPrompt is what a research run is told: the question, framed so the
// run knows it is unattended, read-only, and must list its sources.
func ResearchPrompt(label, question string) string {
	return fmt.Sprintf("You are running research for spore's companion, on %s. Question: %s\n\n"+
		"You run unattended, with read-only tools and your own notes; nobody will answer you. "+
		"Answer the question in a few short paragraphs and list your sources (URLs, files or tool results). "+
		"If you need a tool you do not have, say which and why.",
		label, strings.TrimSpace(question))
}

// AlertText is the text of an alert in the chat: at most 1200 runes, then a
// footer that points at the Jobs folder for the rest.
func AlertText(message string) string {
	r := []rune(message)
	if len(r) <= alertClip {
		return message
	}
	return string(r[:alertClip]) + alertFooter
}

// watchOf reports the active interest whose watch job is job, if any.
func (h *Heartbeat) watchOf(ctx context.Context, job store.Job) (store.Interest, bool, error) {
	active, err := h.Store.Interests(ctx, store.InterestActive)
	if err != nil {
		return store.Interest{}, false, err
	}
	for _, in := range active {
		if in.WatchJobID == job.ID {
			return in, true, nil
		}
	}
	return store.Interest{}, false, nil
}

// RunPrompt is the prompt a job runs with. A companion watch gets
// VerdictSuffix appended; a research job has its run session recorded. Any
// other job runs its own prompt unchanged.
func (h *Heartbeat) RunPrompt(ctx context.Context, job store.Job, sessionID string) string {
	if !h.Cfg.Companion.Enabled {
		return job.Prompt
	}
	_, watch, err := h.watchOf(ctx, job)
	if err != nil {
		slog.Warn("companion: could not read the job's watch", "job", job.ID, "err", err)
		return job.Prompt
	}
	if watch {
		return job.Prompt + VerdictSuffix
	}
	row, ok, err := h.Store.CompanionRunByJob(ctx, job.ID, store.RunResearch)
	if err != nil {
		slog.Warn("companion: could not read the job's research run", "job", job.ID, "err", err)
		return job.Prompt
	}
	if ok {
		if err := h.Store.SetCompanionRunSession(ctx, row.ID, sessionID); err != nil {
			slog.Warn("companion: could not record the research run's session", "job", job.ID, "err", err)
		}
	}
	return job.Prompt
}

// RunFinished records a companion run. handled is true when the job is the
// companion's, so the caller skips its ordinary note path; failNote is true
// only for a failed watch run. Errors are logged, never returned: a run's
// bookkeeping must not fail the job.
func (h *Heartbeat) RunFinished(ctx context.Context, job store.Job, sessionID string, ok bool) (handled, failNote bool) {
	if !h.Cfg.Companion.Enabled {
		return false, false
	}
	now := h.now()
	in, watch, err := h.watchOf(ctx, job)
	if err != nil {
		slog.Warn("companion: could not read the job's watch", "job", job.ID, "err", err)
		return false, false
	}
	if watch {
		verdict := h.recordWatchRun(ctx, in, job, sessionID, ok, now)
		return true, verdict == store.VerdictFailed
	}
	row, found, err := h.Store.CompanionRunByJob(ctx, job.ID, store.RunResearch)
	if err != nil {
		slog.Warn("companion: could not read the job's research run", "job", job.ID, "err", err)
		return false, false
	}
	if !found {
		return false, false
	}
	verdict := store.VerdictDone
	if !ok {
		verdict = store.VerdictFailed
	}
	moved, err := h.Store.FinishCompanionRun(ctx, row.ID, verdict, now)
	switch {
	case err != nil:
		slog.Warn("companion: could not finish the research run", "job", job.ID, "err", err)
	case !moved:
		slog.Debug("companion: research run already finished", "job", job.ID, "run", row.ID)
	}
	return true, false
}

// recordWatchRun writes one watch run, delivers its alert when it has one,
// and returns the run's verdict. A run whose reply cannot be read is failed.
func (h *Heartbeat) recordWatchRun(ctx context.Context, in store.Interest, job store.Job, sessionID string, ok bool, now time.Time) string {
	verdict := store.VerdictFailed
	if ok {
		// A finished run with an unreadable reply still alerts, so the user
		// hears of it; the alert carries the no-details text.
		verdict = store.VerdictAlert
		reply, err := schedule.LastReply(ctx, h.Store, sessionID)
		if err != nil {
			slog.Warn("companion: could not read the watch run's reply; alerting without its details", "job", job.ID, "err", err)
		} else {
			verdict = ParseVerdict(reply).Kind
		}
	}
	run := store.CompanionRun{InterestID: in.ID, JobID: job.ID, Kind: store.RunWatch, SessionID: sessionID,
		Verdict: verdict, StartedAt: now, FinishedAt: now}
	id, err := h.Store.AddCompanionRun(ctx, run)
	if err != nil {
		slog.Warn("companion: could not record the watch run", "job", job.ID, "err", err)
		return verdict
	}
	run.ID = id
	if verdict == store.VerdictAlert {
		sent, err := h.deliverAlert(ctx, run, now)
		if err != nil {
			slog.Warn("companion: could not deliver the watch alert", "job", job.ID, "err", err)
		} else if !sent {
			slog.Debug("companion: watch alert held", "job", job.ID, "run", run.ID)
		}
	}
	return verdict
}

// deliverAlert sends one alert row now, or leaves it held. It reports whether
// it was sent. The text is re-read from the run's session each time, so a
// retry needs no stored text. A session that is gone leaves the row held.
func (h *Heartbeat) deliverAlert(ctx context.Context, run store.CompanionRun, now time.Time) (bool, error) {
	h.alertMu.Lock()
	defer h.alertMu.Unlock()
	if unreported, err := h.stillHeld(ctx, run); err != nil || !unreported {
		return false, err
	}
	if _, found, err := h.Store.Session(ctx, run.SessionID); err != nil {
		return false, err
	} else if !found {
		return false, nil
	}
	held, err := h.alertHeld(ctx, now)
	if err != nil || held {
		return false, err
	}
	var msg string
	reply, err := schedule.LastReply(ctx, h.Store, run.SessionID)
	if err != nil {
		// Send the no-details text rather than hold an alert the user needs.
		slog.Warn("companion: could not re-read the alert's reply; alerting without its details", "run", run.ID, "err", err)
	} else {
		msg = strings.TrimSpace(ParseVerdict(reply).Message)
	}
	text := AlertText(msg)
	if msg == "" {
		text = h.alertLabel(ctx, run.InterestID) + " watch raised an alert with no details · see the Jobs folder"
	}
	o, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: run.InterestID, Kind: store.OutreachAlert,
		Text: text, SentAt: now})
	if errors.Is(err, ErrBusy) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("deliver alert: %w", err)
	}
	// The alert is sent before the run is marked reported. Marking first would
	// lose the alert if the send failed, which is worse than a duplicate. If the
	// mark fails, the run stays held and a retry sends it again: one duplicate.
	if err := h.Store.SetCompanionRunsReported(ctx, []int64{run.ID}, o.ID); err != nil {
		slog.Error("companion: alert sent but its run is not marked reported; a retry may send it again",
			"run", run.ID, "outreach", o.ID, "err", err)
	}
	return true, nil
}

// stillHeld reports whether run is still unreported. A live run and a retry
// may both hold it, and only the first to check may send it.
func (h *Heartbeat) stillHeld(ctx context.Context, run store.CompanionRun) (bool, error) {
	runs, err := h.Store.CompanionRuns(ctx, run.StartedAt)
	if err != nil {
		return false, err
	}
	for _, r := range runs {
		if r.ID == run.ID {
			return r.ReportedOutreachID == 0, nil
		}
	}
	return false, nil
}

// alertHeld reports whether a gate holds alerts now: a pause, quiet hours
// unless AlertsInQuietHours, or the day's alert budget spent.
func (h *Heartbeat) alertHeld(ctx context.Context, now time.Time) (bool, error) {
	c := h.Cfg.Companion
	paused, err := PausedUntil(ctx, h.Store)
	if err != nil {
		return false, err
	}
	if now.Before(paused) {
		return true, nil
	}
	loc, err := c.Location()
	if err != nil {
		return false, err
	}
	if !c.AlertsInQuietHours {
		quiet, err := InQuietHours(now, loc, c.QuietHours)
		if err != nil {
			return false, err
		}
		if quiet {
			return true, nil
		}
	}
	sent, err := alertsSince(ctx, h.Store, LocalDayStart(now, loc))
	if err != nil {
		return false, err
	}
	return sent >= c.AlertBudget, nil
}

// alertLabel names the interest an alert is about, for the no-details text.
func (h *Heartbeat) alertLabel(ctx context.Context, interestID int64) string {
	in, ok, err := h.Store.InterestByID(ctx, interestID)
	if err != nil || !ok {
		return "companion"
	}
	return in.Label
}

// alertsSince counts the alerts sent at or after since.
func alertsSince(ctx context.Context, st *store.Store, since time.Time) (int, error) {
	rows, err := st.OutreachSince(ctx, since)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		if o.Kind == store.OutreachAlert {
			n++
		}
	}
	return n, nil
}

// RetryHeldAlerts tries every held watch alert from the last 30 days. It makes
// no model call. Used by the digest.
func (h *Heartbeat) RetryHeldAlerts(ctx context.Context, now time.Time) error {
	if !h.Cfg.Companion.Enabled {
		return nil
	}
	runs, err := h.Store.CompanionRuns(ctx, now.Add(-heldAlertWindow))
	if err != nil {
		return err
	}
	var errs []error
	sent := 0
	for _, r := range runs {
		if r.Kind != store.RunWatch || r.Verdict != store.VerdictAlert || r.ReportedOutreachID != 0 {
			continue
		}
		ok, err := h.deliverAlert(ctx, r, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("held alert of run %d: %w", r.ID, err))
		}
		if ok {
			sent++
		}
	}
	slog.Debug("companion: held alerts retried", "sent", sent)
	return errors.Join(errs...)
}
