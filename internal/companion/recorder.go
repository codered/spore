package companion

import (
	"context"
	"fmt"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// FadeAfter is how long an observing or candidate interest may go unseen
// before it is retired.
const FadeAfter = 30 * 24 * time.Hour

// Recorder turns validated signals into interest rows and keeps interest
// states in step with the evidence.
type Recorder struct {
	Store *store.Store
	Cfg   *config.Config
	// Now is the clock; tests replace it.
	Now func() time.Time
}

func NewRecorder(st *store.Store, cfg *config.Config) *Recorder {
	return &Recorder{Store: st, Cfg: cfg, Now: time.Now}
}

// Enabled reports whether the companion is on. Off, Record does nothing and
// the refiner does not ask for signals.
func (r *Recorder) Enabled() bool { return r.Cfg.Companion.Enabled }

// Known is every interest not retired, so the planner reuses existing keys
// instead of inventing stock:zscaler next to stock:zs.
func (r *Recorder) Known(ctx context.Context) ([]store.Interest, error) {
	return r.Store.Interests(ctx, store.InterestObserving, store.InterestCandidate,
		store.InterestProposed, store.InterestActive, store.InterestDeclined)
}

// RecordResult is what Record did. Dropped explains each signal it refused.
type RecordResult struct {
	Recorded int
	Dropped  []string
}

// Record validates and stores one round's signals for a session, then sweeps
// interest states. Validation failures are reported in Dropped, never as an
// error: a bad signal must not cost the round its edits.
func (r *Recorder) Record(ctx context.Context, sess store.Session, signals []Signal) (RecordResult, error) {
	var res RecordResult
	if !r.Enabled() || len(signals) == 0 {
		return res, nil
	}
	if !Trusted(sess) {
		res.Dropped = append(res.Dropped, fmt.Sprintf("signals: a %s session's signals do not count", sess.Source))
		return res, nil
	}
	loc, err := r.Cfg.Companion.Location()
	if err != nil {
		return res, err
	}
	now := r.Now()
	day := now.In(loc).Format("2006-01-02")
	seen := map[string]bool{}
	for _, raw := range signals {
		s, err := Normalize(raw)
		if err != nil {
			res.Dropped = append(res.Dropped, err.Error())
			continue
		}
		if seen[s.Key] {
			continue // one round, one sighting per key
		}
		if res.Recorded >= MaxSignalsPerRound {
			res.Dropped = append(res.Dropped, fmt.Sprintf("signal %s: over the %d-signal cap", s.Key, MaxSignalsPerRound))
			continue
		}
		seen[s.Key] = true
		in, err := r.Store.TouchInterest(ctx, s.Key, s.Label)
		if err != nil {
			return res, err
		}
		if err := r.Store.AddInterestSignal(ctx, in.ID, sess.ID, s.Kind, day, now); err != nil {
			return res, err
		}
		res.Recorded++
	}
	return res, r.Sweep(ctx, now)
}

// Sweep moves observing and candidate interests to match their evidence:
// retired after FadeAfter without a signal, candidate once seen on
// habit_days distinct days (unless cooling down), and back to observing if
// a candidate's evidence shrank below the bar.
func (r *Recorder) Sweep(ctx context.Context, now time.Time) error {
	rows, err := r.Store.Interests(ctx, store.InterestObserving, store.InterestCandidate)
	if err != nil {
		return err
	}
	habit := r.Cfg.Companion.HabitDays
	for _, in := range rows {
		to := in.State
		switch {
		case !in.LastSeen.IsZero() && now.Sub(in.LastSeen) > FadeAfter:
			to = store.InterestRetired
		case in.State == store.InterestObserving && in.DaysSeen >= habit && !now.Before(in.CooldownUntil):
			to = store.InterestCandidate
		case in.State == store.InterestCandidate && in.DaysSeen < habit:
			to = store.InterestObserving
		}
		if to == in.State {
			continue
		}
		if _, err := r.Store.SetInterestState(ctx, in.ID, in.State, to); err != nil {
			return err
		}
	}
	return nil
}
