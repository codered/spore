package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool/schedule"
)

// Timeouts and cooldowns. See the companion spec, sections 2.3, 4.6 and 5.3.
const (
	UnansweredAfter   = 12 * time.Hour
	NudgeIgnoredAfter = 24 * time.Hour
	// RepliedProposalReturn is how long a proposal answered in text may
	// stay proposed before it goes back to candidate. Replied rows are
	// scanned on every tick, and only the interest's newest proposal counts:
	// an older replied row never returns a re-proposed interest.
	RepliedProposalReturn = 24 * time.Hour
	// PendingDecisionMaxAge is how old a saved busy decision may be and
	// still be applied.
	PendingDecisionMaxAge = 12 * time.Hour
	NotNowCooldown        = 14 * 24 * time.Hour
	ReturnCooldown        = 7 * 24 * time.Hour
	RecentOutreach        = 7 * 24 * time.Hour
	// FailureWarning is how many reflections in a row may fail before
	// status warns about it.
	FailureWarning = 3
	// ResearchStuckAfter is how long a research run may stay pending before
	// it is failed. A late finish never overwrites that failure.
	ResearchStuckAfter = 2 * time.Hour
	// RunsShownFor is how far back reflection sees finished runs; RunsKeptFor
	// is how long a run's row is kept.
	RunsShownFor = 7 * 24 * time.Hour
	RunsKeptFor  = 30 * 24 * time.Hour
	// FollowupWindow is the span the idle follow-up counts its runs over.
	FollowupWindow = 14 * 24 * time.Hour
	// FollowupIdleRuns is how many runs in FollowupWindow make a watch idle.
	FollowupIdleRuns = 10
	// FollowupFailedRuns is how many failed runs in a row prompt a follow-up.
	FollowupFailedRuns = 3
)

// ErrBusy is the deliverer refusing because a turn is running in the
// companion session: appending to it now would land inside that turn.
var ErrBusy = errors.New("the companion session has a turn running")

// ErrBadCheck wraps an operator's check that names no usable interest.
var ErrBadCheck = errors.New("bad check")

// Reflector makes the reflection call.
type Reflector interface {
	Reflect(ctx context.Context, system, input string) (string, error)
}

// Deliverer is the daemon's side of an outreach: the companion session and
// the channel. The heartbeat never touches either directly.
type Deliverer interface {
	// EnsureSession returns the companion session, creating it when there
	// is none.
	EnsureSession(ctx context.Context) (string, error)
	// Deliver records o (as pending) in the store and the companion session,
	// then sends it on the channel. It returns the stored row. It returns
	// ErrBusy, having written nothing, while a turn runs in the session.
	Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error)
}

// SelfWriter replaces self.md through the refinement ledger, recorded
// against sessionID.
type SelfWriter func(ctx context.Context, sessionID, body string) error

// Heartbeat is the companion's tick: resolve stale outreach, sweep
// interests, and -- when something changed and the gate allows -- ask the
// model whether to start a conversation.
type Heartbeat struct {
	Store     *store.Store
	Cfg       *config.Config
	Recorder  *Recorder
	Reflector Reflector
	Deliverer Deliverer
	WriteSelf SelfWriter
	// Now is the clock; tests replace it.
	Now func() time.Time

	// mu keeps an operator's check and the ticker from reflecting at once.
	mu sync.Mutex
	// alertMu keeps a run's alert and a retry of held alerts from sending
	// the same one twice.
	alertMu sync.Mutex
}

// TickOptions shape an operator's check. The zero value is a heartbeat tick.
type TickOptions struct {
	// Manual skips pause, quiet hours and the unchanged-state skip. The
	// budget still applies.
	Manual bool
	// Promote makes this observing interest a candidate first, however few
	// days it has been seen on.
	Promote string
	// Force tells the reflection it must propose Promote.
	Force bool
}

func (h *Heartbeat) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Run ticks every `every` until ctx ends, starting with one tick now. The
// heartbeat reads current state, never a backlog, so a restart or a night
// of quiet hours never produces a burst.
func (h *Heartbeat) Run(ctx context.Context, every time.Duration) {
	tick := func() {
		res, err := h.Tick(ctx)
		if err != nil {
			slog.Warn("companion heartbeat failed", "err", err)
			return
		}
		slog.Debug("companion heartbeat", "result", res)
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// Tick runs one heartbeat and says what it did in a few words: why it
// skipped, or the action taken. An error means the tick failed and nothing
// was sent.
func (h *Heartbeat) Tick(ctx context.Context) (string, error) {
	return h.TickWith(ctx, TickOptions{})
}

// TickWith runs one tick shaped by o. See TickOptions.
func (h *Heartbeat) TickWith(ctx context.Context, o TickOptions) (string, error) {
	if o.Force && o.Promote == "" {
		return "", fmt.Errorf("%w: force needs an interest to promote", ErrBadCheck)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.Cfg.Companion
	if !c.Enabled {
		return "disabled", nil
	}
	now := h.now()
	loc, err := c.Location()
	if err != nil {
		return "", err
	}
	if err := h.resolveStale(ctx, now); err != nil {
		return "", err
	}
	if h.Recorder != nil {
		if err := h.Recorder.Sweep(ctx, now); err != nil {
			return "", err
		}
	}
	if err := h.housekeep(ctx, now); err != nil {
		return "", err
	}
	if o.Promote != "" {
		if err := h.promote(ctx, o.Promote, now, loc); err != nil {
			return "", err
		}
	}
	if !o.Manual {
		paused, err := PausedUntil(ctx, h.Store)
		if err != nil {
			return "", err
		}
		if now.Before(paused) {
			return "paused", nil
		}
		quiet, err := InQuietHours(now, loc, c.QuietHours)
		if err != nil {
			return "", err
		}
		if quiet {
			return "quiet hours", nil
		}
	}
	v, err := h.view(ctx, now, loc, o)
	if err != nil {
		return "", err
	}
	if o.Force {
		v.Force = o.Promote
	}
	// A decision the busy session refused is applied before the gate: it was
	// made already, so the gate and the fingerprint do not decide it again.
	// An operator's check that names an interest asks for a fresh answer.
	if o.Promote == "" {
		res, settled, err := h.applySaved(ctx, v, now)
		if err != nil {
			return "", err
		}
		if settled {
			return res, nil
		}
	}
	if !Eligible(v) {
		return "nothing to decide", nil
	}
	// The digest and research cost no budget: a spent budget never holds them.
	if v.BudgetLeft < 1 && !v.DigestDue && v.ForceResearch == "" {
		return "budget spent", nil
	}
	fp := Fingerprint(v)
	last, _, err := h.Store.CompanionState(ctx, StateFingerprint)
	if err != nil {
		return "", err
	}
	if last == fp && !o.Manual {
		return "nothing changed", nil
	}
	reply, err := h.Reflector.Reflect(ctx, ReflectionPrompt, RenderInput(v))
	if err != nil {
		return "", h.fail(ctx, now, fmt.Errorf("reflection call: %w", err))
	}
	d, err := ParseDecision(reply)
	if err == nil {
		err = Validate(d, v)
	}
	if err != nil {
		return "", h.fail(ctx, now, fmt.Errorf("reflection output: %w", err))
	}
	if err := h.apply(ctx, d, v, now); err != nil {
		if errors.Is(err, ErrBusy) {
			// The user is talking to spore right now. Keep the decision: the
			// next tick applies it without asking the model again.
			if err := h.saveDecision(ctx, d, now); err != nil {
				return "", h.fail(ctx, now, fmt.Errorf("save busy decision: %w", err))
			}
			return "companion session busy (decision saved)", nil
		}
		return "", h.fail(ctx, now, err)
	}
	// Only a decision that applied counts as the user having seen the runs.
	if err := h.Store.SetCompanionRunsSeen(ctx, runIDs(v.Runs), now); err != nil {
		return "", h.fail(ctx, now, err)
	}
	res := summarize(d)
	return res, h.succeed(ctx, now, fp, res)
}

// savedDecision is a reflection decision that a busy session refused.
type savedDecision struct {
	Decision Decision  `json:"decision"`
	SavedAt  time.Time `json:"saved_at"`
}

// saveDecision keeps d, made at now, for the next tick.
func (h *Heartbeat) saveDecision(ctx context.Context, d Decision, now time.Time) error {
	b, err := json.Marshal(savedDecision{Decision: d, SavedAt: now})
	if err != nil {
		return err
	}
	return h.Store.SetCompanionState(ctx, StatePendingDecision, string(b))
}

// clearDecision forgets the saved decision. An empty value means none saved.
func (h *Heartbeat) clearDecision(ctx context.Context) error {
	return h.Store.SetCompanionState(ctx, StatePendingDecision, "")
}

// applySaved applies the saved decision against the fresh view v. It reports
// the tick's result and true when the decision settled the tick. A decision
// that is too old, that v no longer allows, or that cannot be read is
// discarded, and the tick goes on as usual.
func (h *Heartbeat) applySaved(ctx context.Context, v View, now time.Time) (string, bool, error) {
	raw, _, err := h.Store.CompanionState(ctx, StatePendingDecision)
	if err != nil {
		return "", false, err
	}
	if raw == "" {
		return "", false, nil
	}
	var s savedDecision
	if err := json.Unmarshal([]byte(raw), &s); err != nil || now.Sub(s.SavedAt) > PendingDecisionMaxAge || Validate(s.Decision, v) != nil {
		return "", false, h.clearDecision(ctx)
	}
	fp := Fingerprint(v)
	switch err := h.apply(ctx, s.Decision, v, now); {
	case errors.Is(err, ErrBusy):
		// Still busy: keep the decision for the next tick.
		return "companion session busy", true, nil
	case err != nil:
		if cerr := h.clearDecision(ctx); cerr != nil {
			slog.Warn("companion: could not clear a failed saved decision", "err", cerr)
		}
		return "", true, h.fail(ctx, now, err)
	}
	// The message is out. Clear the decision before anything else can fail:
	// a surviving one would send the same message again on the next tick.
	if err := h.clearDecision(ctx); err != nil {
		slog.Warn("companion: applied a saved decision but could not clear it; it may apply again", "err", err)
	}
	if err := h.Store.SetCompanionRunsSeen(ctx, runIDs(v.Runs), now); err != nil {
		return "", true, h.fail(ctx, now, err)
	}
	res := summarize(s.Decision) + " (saved decision)"
	return res, true, h.succeed(ctx, now, fp, res)
}

// housekeep fails research runs that never finished, forgets runs past their
// keep window, and retries held alerts. It makes no model call. A failed
// retry is logged, never returned: it must not stop the reflection.
func (h *Heartbeat) housekeep(ctx context.Context, now time.Time) error {
	runs, err := h.Store.CompanionRuns(ctx, now.Add(-RunsKeptFor))
	if err != nil {
		return err
	}
	for _, r := range runs {
		if r.Kind != store.RunResearch || r.Verdict != store.VerdictPending || now.Sub(r.StartedAt) < ResearchStuckAfter {
			continue
		}
		if _, err := h.Store.FinishCompanionRun(ctx, r.ID, store.VerdictFailed, now); err != nil {
			return err
		}
	}
	if _, err := h.Store.DeleteCompanionRunsBefore(ctx, now.Add(-RunsKeptFor)); err != nil {
		return err
	}
	if err := h.RetryHeldAlerts(ctx, now); err != nil {
		slog.Warn("companion: some held alerts could not be sent; they stay held", "err", err)
	}
	return nil
}

// promote makes key a candidate for one check. It runs after the sweep,
// which would otherwise send a candidate with too few days straight back.
func (h *Heartbeat) promote(ctx context.Context, key string, now time.Time, loc *time.Location) error {
	in, ok, err := h.Store.InterestByKey(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no interest %q (see spore companion interests)", ErrBadCheck, key)
	}
	if now.Before(in.CooldownUntil) {
		return fmt.Errorf("%w: interest %s is cooling down until %s", ErrBadCheck, key, in.CooldownUntil.In(loc).Format("2006-01-02 15:04"))
	}
	switch in.State {
	case store.InterestCandidate:
		return nil
	case store.InterestObserving:
		_, err := h.Store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestCandidate)
		return err
	default:
		return fmt.Errorf("%w: interest %s is %s; only an observing or candidate interest can be promoted", ErrBadCheck, key, in.State)
	}
}

// resolveStale times out what the user did not answer: a message pending
// for UnansweredAfter becomes unanswered (reflection decides what next), and
// a nudge pending for NudgeIgnoredAfter ends itself and its original as
// ignored.
func (h *Heartbeat) resolveStale(ctx context.Context, now time.Time) error {
	pending, err := h.Store.OutreachByOutcome(ctx, store.OutcomePending)
	if err != nil {
		return err
	}
	for _, o := range pending {
		age := now.Sub(o.SentAt)
		if o.Kind != store.OutreachNudge {
			if age >= UnansweredAfter {
				if _, err := h.Store.ResolveOutreach(ctx, o.ID, []string{store.OutcomePending}, store.OutcomeUnanswered, "", now); err != nil {
					return err
				}
			}
			continue
		}
		if age < NudgeIgnoredAfter {
			continue
		}
		if _, err := h.Store.ResolveOutreach(ctx, o.ID, []string{store.OutcomePending}, store.OutcomeIgnored, "", now); err != nil {
			return err
		}
		orig, ok, err := h.Store.OutreachByID(ctx, o.NudgeOf)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		moved, err := Resolve(ctx, h.Store, h.Cfg.Companion.StartBudget, orig.ID, []string{store.OutcomeUnanswered},
			store.OutcomeIgnored, "its nudge went unanswered for a day", now)
		if err != nil {
			return err
		}
		if moved {
			if err := returnProposal(ctx, h.Store, orig, now); err != nil {
				return err
			}
		}
	}
	// A proposal answered in text never resolves its interest. Once it has
	// waited RepliedProposalReturn, it goes back to candidate. Only the
	// interest's newest proposal may do this: a re-proposal is live.
	replied, err := h.Store.OutreachByOutcome(ctx, store.OutcomeReplied)
	if err != nil {
		return err
	}
	for _, o := range replied {
		if o.Kind != store.OutreachProposal || o.ResolvedAt.After(now.Add(-RepliedProposalReturn)) {
			continue
		}
		latest, ok, err := h.Store.LatestProposal(ctx, o.InterestID)
		if err != nil {
			return err
		}
		if !ok || latest.ID != o.ID {
			continue
		}
		if err := returnProposal(ctx, h.Store, o, now); err != nil {
			return err
		}
	}
	return nil
}

// view reads everything a reflection sees. o shapes an operator's check.
func (h *Heartbeat) view(ctx context.Context, now time.Time, loc *time.Location, o TickOptions) (View, error) {
	v := View{Now: now, Loc: loc, SelfMax: h.Cfg.Companion.SelfMaxBytes,
		Days: map[int64][]string{}, Keys: map[int64]string{}, Nudged: map[int64]bool{}}
	// soul.md is the user's; missing or unreadable, it simply has no section.
	if b, err := os.ReadFile(h.Cfg.SoulPath()); err == nil {
		v.Soul = string(b)
	}
	switch b, err := os.ReadFile(h.Cfg.SelfPath()); {
	case err == nil:
		v.Self, v.SelfReadable = string(b), true
	case errors.Is(err, fs.ErrNotExist):
		v.SelfReadable = true
	default:
		slog.Warn("companion: self.md is unreadable; reflection may not rewrite it", "err", err)
	}
	all, err := h.Store.Interests(ctx)
	if err != nil {
		return View{}, err
	}
	for _, in := range all {
		v.Keys[in.ID] = in.Key
		switch in.State {
		case store.InterestCandidate, store.InterestProposed, store.InterestActive:
			v.Interests = append(v.Interests, in)
			days, err := h.Store.InterestDays(ctx, in.ID)
			if err != nil {
				return View{}, err
			}
			v.Days[in.ID] = days
		}
	}
	if v.Recent, err = h.Store.OutreachSince(ctx, now.Add(-RecentOutreach)); err != nil {
		return View{}, err
	}
	if v.Unanswered, err = h.Store.OutreachByOutcome(ctx, store.OutcomeUnanswered); err != nil {
		return View{}, err
	}
	for _, o := range v.Unanswered {
		_, ok, err := h.Store.NudgeFor(ctx, o.ID)
		if err != nil {
			return View{}, err
		}
		v.Nudged[o.ID] = ok
	}
	b, err := LoadBudget(ctx, h.Store, h.Cfg.Companion.StartBudget)
	if err != nil {
		return View{}, err
	}
	spent, err := SpentToday(ctx, h.Store, now, loc)
	if err != nil {
		return View{}, err
	}
	v.BudgetLeft = int(b) - spent
	// The digest is due once a day, on the first tick after its day began.
	// An operator's check never sends it.
	last, _, err := h.Store.CompanionState(ctx, StateLastDigestDay)
	if err != nil {
		return View{}, err
	}
	v.DigestDue = last != now.In(loc).Format("2006-01-02") && !o.Force
	if err := h.viewRuns(ctx, &v, now, loc); err != nil {
		return View{}, err
	}
	return v, nil
}

// viewRuns reads the run side of the view: research counts, the finished
// runs the user has not been told of, and the follow-up subjects.
func (h *Heartbeat) viewRuns(ctx context.Context, v *View, now time.Time, loc *time.Location) error {
	runs, err := h.Store.CompanionRuns(ctx, now.Add(-RunsKeptFor))
	if err != nil {
		return err
	}
	dayStart := LocalDayStart(now, loc)
	for _, r := range runs {
		if r.Kind == store.RunResearch {
			if !r.StartedAt.Before(dayStart) {
				v.ResearchToday++
			}
			if r.Verdict == store.VerdictPending {
				v.ResearchInFlight = true
			}
		}
		if r.Verdict == store.VerdictPending || r.ReportedOutreachID != 0 || r.FinishedAt.IsZero() ||
			r.FinishedAt.Before(now.Add(-RunsShownFor)) {
			continue
		}
		reply, err := h.runReply(ctx, r.SessionID)
		if err != nil {
			return err
		}
		v.Runs = append(v.Runs, RunView{Run: r, Key: v.Keys[r.InterestID], Reply: reply,
			Held: r.Kind == store.RunWatch && r.Verdict == store.VerdictAlert})
	}
	return h.followups(ctx, v, runs, now)
}

// runReply is the full reply of a run, read from its session. A session that
// is gone reads "(run deleted)". A reply that cannot be read is said so, and
// does not stop the tick.
func (h *Heartbeat) runReply(ctx context.Context, sessionID string) (string, error) {
	_, found, err := h.Store.Session(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if !found {
		return "(run deleted)", nil
	}
	reply, err := schedule.LastReply(ctx, h.Store, sessionID)
	if err != nil {
		slog.Warn("companion: could not read a run's reply", "session", sessionID, "err", err)
		return "(reply unreadable)", nil
	}
	return reply, nil
}

// followups sets the subjects reflection may raise: an active watch that ran
// often with no answer, one whose last runs all failed, and in the digest
// slot the threads line. runs is every run in the keep window, newest first.
func (h *Heartbeat) followups(ctx context.Context, v *View, runs []store.CompanionRun, now time.Time) error {
	since := now.Add(-FollowupWindow)
	recent, err := h.Store.OutreachSince(ctx, since)
	if err != nil {
		return err
	}
	for _, in := range v.Interests {
		if in.State != store.InterestActive {
			continue
		}
		var mine []store.CompanionRun
		inWindow := 0
		for _, r := range runs {
			if r.Kind != store.RunWatch || r.InterestID != in.ID {
				continue
			}
			mine = append(mine, r)
			if !r.StartedAt.Before(since) {
				inWindow++
			}
		}
		if inWindow >= FollowupIdleRuns && !answered(recent, in.ID) {
			v.Followups = append(v.Followups, fmt.Sprintf("watch %s has run %d times in 14 days with no reply or reaction to anything about it: ask whether to keep, change or retire it", in.Key, inWindow))
		}
		if len(mine) >= FollowupFailedRuns && allFailed(mine[:FollowupFailedRuns]) {
			v.Followups = append(v.Followups, fmt.Sprintf("watch %s failed its last %d runs: tell the user, or suggest changing or retiring it", in.Key, FollowupFailedRuns))
		}
	}
	if v.DigestDue {
		v.Followups = append(v.Followups, `read "Threads with you" in your notes and raise any that has come due`)
	}
	return nil
}

// answered reports whether any message about interest id since the window
// was replied to or reacted to.
func answered(recent []store.Outreach, id int64) bool {
	for _, o := range recent {
		if o.InterestID == id && (o.Outcome == store.OutcomeReplied || o.Outcome == store.OutcomeReacted) {
			return true
		}
	}
	return false
}

// allFailed reports whether every run in runs failed.
func allFailed(runs []store.CompanionRun) bool {
	for _, r := range runs {
		if r.Verdict != store.VerdictFailed {
			return false
		}
	}
	return true
}

// runIDs lists the ids of runs.
func runIDs(runs []RunView) []int64 {
	ids := make([]int64, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.Run.ID)
	}
	return ids
}

// uniqueIDs drops repeated ids, keeping the first of each. A model may name
// one id twice; a write must not do it twice.
func uniqueIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	var out []int64
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// apply carries out a validated decision. The message goes first: if it
// cannot be delivered, nothing else in the decision is written.
func (h *Heartbeat) apply(ctx context.Context, d Decision, v View, now time.Time) error {
	switch d.Action {
	case ActionPropose:
		in, _ := v.Interest(d.Interest)
		payload, err := json.Marshal(d.Watch)
		if err != nil {
			return err
		}
		if _, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal,
			Text: d.Message, Payload: string(payload), SentAt: now}); err != nil {
			return err
		}
		if _, err := h.Store.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed); err != nil {
			return err
		}
	case ActionNudge:
		orig, _ := v.UnansweredByID(d.NudgeOf[0])
		if _, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: orig.InterestID, Kind: store.OutreachNudge,
			Text: d.Message, NudgeOf: orig.ID, SentAt: now}); err != nil {
			return err
		}
	case ActionResearch:
		in, _ := v.Interest(d.Interest)
		q := strings.TrimSpace(d.Research.Question)
		jobID, err := h.Store.CreateJob(ctx, store.Job{Kind: "once", Spec: now.UTC().Format(time.RFC3339),
			Prompt: ResearchPrompt(in.Label, q), Enabled: true, NextRun: now, Notify: store.NotifyNone,
			CheckedIn: true, Profile: config.CompanionProfile})
		if err != nil {
			return err
		}
		if _, err := h.Store.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, Kind: store.RunResearch,
			JobID: jobID, Question: q, Verdict: store.VerdictPending, StartedAt: now}); err != nil {
			// Without its row the job would run as an ordinary one: stop it.
			_ = h.Store.SetJobEnabled(ctx, jobID, false)
			return err
		}
	case ActionFinding, ActionFollowup:
		kind := store.OutreachFollowup
		if d.Action == ActionFinding {
			kind = store.OutreachFinding
		}
		var interestID int64
		if in, ok := v.Interest(d.Interest); ok {
			interestID = in.ID
		}
		o, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: interestID, Kind: kind, Text: d.Message, SentAt: now})
		if err != nil {
			return err
		}
		if d.Action == ActionFinding {
			if err := h.Store.SetCompanionRunsReported(ctx, uniqueIDs(d.Runs), o.ID); err != nil {
				return err
			}
		}
	case ActionDigest:
		if err := h.sendDigest(ctx, d, v, now); err != nil {
			return err
		}
	}
	for _, dr := range d.Drops {
		o, _ := v.UnansweredByID(dr.Outreach)
		moved, err := Resolve(ctx, h.Store, h.Cfg.Companion.StartBudget, o.ID, []string{store.OutcomeUnanswered},
			store.OutcomeIgnored, strings.TrimSpace(dr.Reason), now)
		if err != nil {
			return err
		}
		if moved {
			if err := returnProposal(ctx, h.Store, o, now); err != nil {
				return err
			}
		}
	}
	if d.SelfMD != nil && h.WriteSelf != nil {
		sid, err := h.Deliverer.EnsureSession(ctx)
		if err != nil {
			return err
		}
		if err := h.WriteSelf(ctx, sid, *d.SelfMD); err != nil {
			return fmt.Errorf("rewrite self.md: %w", err)
		}
	}
	return nil
}

// sendDigest sends the morning digest. It covers the runs the digest names
// and every held alert, folds in the nudges it names, and marks the day sent
// only once the message is out.
func (h *Heartbeat) sendDigest(ctx context.Context, d Decision, v View, now time.Time) error {
	loc, err := h.Cfg.Companion.Location()
	if err != nil {
		return err
	}
	o, err := h.Deliverer.Deliver(ctx, store.Outreach{Kind: store.OutreachDigest, Text: d.Message, SentAt: now})
	if err != nil {
		return err
	}
	covered := uniqueIDs(d.Runs)
	for _, r := range v.Runs {
		if r.Held {
			covered = append(covered, r.Run.ID)
		}
	}
	if err := h.Store.SetCompanionRunsReported(ctx, uniqueIDs(covered), o.ID); err != nil {
		return err
	}
	for _, id := range uniqueIDs(d.NudgeOf) {
		orig, _ := v.UnansweredByID(id)
		if _, err := h.Store.AddOutreach(ctx, store.Outreach{InterestID: orig.InterestID, Kind: store.OutreachNudge,
			Channel: store.OutreachChannelDigest, Text: fmt.Sprintf("(folded into digest #%d)", o.ID),
			NudgeOf: id, SentAt: now}); err != nil {
			return err
		}
	}
	if err := h.supersedeDigests(ctx, o.ID, now); err != nil {
		return err
	}
	return h.Store.SetCompanionState(ctx, StateLastDigestDay, now.In(loc).Format("2006-01-02"))
}

// supersedeDigests ends every earlier digest still waiting for an answer. The
// next digest replaces it, so no governor step: the user did not ignore it.
func (h *Heartbeat) supersedeDigests(ctx context.Context, keep int64, now time.Time) error {
	open, err := h.Store.OutreachByOutcome(ctx, store.OutcomePending, store.OutcomeUnanswered)
	if err != nil {
		return err
	}
	for _, o := range open {
		if o.Kind != store.OutreachDigest || o.ID == keep {
			continue
		}
		if _, err := h.Store.ResolveOutreach(ctx, o.ID, []string{store.OutcomePending, store.OutcomeUnanswered},
			store.OutcomeIgnored, "superseded by the next digest", now); err != nil {
			return err
		}
	}
	return nil
}

// summarize names what a decision did, for logs and status.
func summarize(d Decision) string {
	var s string
	switch d.Action {
	case ActionPropose:
		s = "propose " + d.Interest
	case ActionNudge:
		s = "nudge #" + strconv.FormatInt(d.NudgeOf[0], 10)
	case ActionResearch:
		s = "research " + d.Interest
	case ActionFinding:
		s = "finding"
	case ActionFollowup:
		s = "followup"
	case ActionDigest:
		s = "digest"
	default:
		s = "none"
	}
	if n := len(d.Drops); n > 0 {
		s += fmt.Sprintf(" (+%d dropped)", n)
	}
	if d.SelfMD != nil {
		s += " (self.md rewritten)"
	}
	return s
}

// fail records a failed reflection and returns cause.
func (h *Heartbeat) fail(ctx context.Context, now time.Time, cause error) error {
	n := 0
	if v, ok, err := h.Store.CompanionState(ctx, StateFailures); err == nil && ok {
		n, _ = strconv.Atoi(v)
	}
	n++
	_ = h.Store.SetCompanionState(ctx, StateFailures, strconv.Itoa(n))
	_ = setStateTime(ctx, h.Store, StateLastReflection, now)
	_ = h.Store.SetCompanionState(ctx, StateLastResult, clip("failed: "+cause.Error(), 200))
	return cause
}

// succeed records a reflection's result and the fingerprint it answered.
func (h *Heartbeat) succeed(ctx context.Context, now time.Time, fp, result string) error {
	if err := h.Store.SetCompanionState(ctx, StateFailures, "0"); err != nil {
		return err
	}
	if err := setStateTime(ctx, h.Store, StateLastReflection, now); err != nil {
		return err
	}
	if err := h.Store.SetCompanionState(ctx, StateLastResult, result); err != nil {
		return err
	}
	return h.Store.SetCompanionState(ctx, StateFingerprint, fp)
}
