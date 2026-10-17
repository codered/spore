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
)

// Timeouts and cooldowns. See the companion spec, sections 2.3, 4.6 and 5.3.
const (
	UnansweredAfter   = 12 * time.Hour
	NudgeIgnoredAfter = 24 * time.Hour
	NotNowCooldown    = 14 * 24 * time.Hour
	ReturnCooldown    = 7 * 24 * time.Hour
	RecentOutreach    = 7 * 24 * time.Hour
	// FailureWarning is how many reflections in a row may fail before
	// status warns about it.
	FailureWarning = 3
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
	v, err := h.view(ctx, now, loc)
	if err != nil {
		return "", err
	}
	if o.Force {
		v.Force = o.Promote
	}
	if !Eligible(v) {
		return "nothing to decide", nil
	}
	if v.BudgetLeft < 1 {
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
			// The user is talking to spore right now; the next tick asks again.
			return "companion session busy", nil
		}
		return "", h.fail(ctx, now, err)
	}
	res := summarize(d)
	return res, h.succeed(ctx, now, fp, res)
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
	return nil
}

// view reads everything a reflection sees.
func (h *Heartbeat) view(ctx context.Context, now time.Time, loc *time.Location) (View, error) {
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
	return v, nil
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
		orig, _ := v.UnansweredByID(d.NudgeOf)
		if _, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: orig.InterestID, Kind: store.OutreachNudge,
			Text: d.Message, NudgeOf: orig.ID, SentAt: now}); err != nil {
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

// summarize names what a decision did, for logs and status.
func summarize(d Decision) string {
	var s string
	switch d.Action {
	case ActionPropose:
		s = "propose " + d.Interest
	case ActionNudge:
		s = "nudge #" + strconv.FormatInt(d.NudgeOf, 10)
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
