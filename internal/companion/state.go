package companion

import (
	"context"
	"strconv"
	"time"

	"github.com/codered/spore/internal/store"
)

// Keys of the companion_state table.
const (
	StateSessionID      = "session_id"
	StateBudget         = "daily_budget"
	StatePausedUntil    = "paused_until"
	StateLastReflection = "last_reflection_at"
	StateLastResult     = "last_reflection_result"
	StateFailures       = "reflection_failures"
	StateFingerprint    = "last_fingerprint"
)

// LoadBudget is the governor's budget: the stored value, or start when none
// is stored yet. A value that does not parse -- a hand-edited row -- reads
// as start rather than stopping the companion.
func LoadBudget(ctx context.Context, st *store.Store, start int) (float64, error) {
	v, ok, err := st.CompanionState(ctx, StateBudget)
	if err != nil {
		return 0, err
	}
	if !ok {
		return float64(start), nil
	}
	b, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return float64(start), nil
	}
	return clampBudget(b, start), nil
}

// SaveBudget stores the governor's budget.
func SaveBudget(ctx context.Context, st *store.Store, b float64) error {
	return st.SetCompanionState(ctx, StateBudget, strconv.FormatFloat(b, 'f', -1, 64))
}

// StateTime reads a time-valued key; zero when unset or unreadable.
func StateTime(ctx context.Context, st *store.Store, key string) (time.Time, error) {
	v, ok, err := st.CompanionState(ctx, key)
	if err != nil || !ok || v == "" {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

func setStateTime(ctx context.Context, st *store.Store, key string, t time.Time) error {
	v := ""
	if !t.IsZero() {
		v = t.UTC().Format(time.RFC3339)
	}
	return st.SetCompanionState(ctx, key, v)
}

// PausedUntil is when a pause ends; zero when there is none.
func PausedUntil(ctx context.Context, st *store.Store) (time.Time, error) {
	return StateTime(ctx, st, StatePausedUntil)
}

// SetPausedUntil pauses outreach until t; a zero t resumes it.
func SetPausedUntil(ctx context.Context, st *store.Store, t time.Time) error {
	return setStateTime(ctx, st, StatePausedUntil, t)
}

// SpentToday counts the messages started since local midnight that count
// against the daily budget: all but alerts, which have their own allowance.
func SpentToday(ctx context.Context, st *store.Store, now time.Time, loc *time.Location) (int, error) {
	rows, err := st.OutreachSince(ctx, LocalDayStart(now, loc))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		if o.Kind != store.OutreachAlert {
			n++
		}
	}
	return n, nil
}

// Resolve moves one outreach to `to`, when its outcome is one of from, and
// lets the governor respond. A pending nudge of it follows it to the same
// outcome without a second governor step: one answer, one adjustment. It
// reports whether the outreach moved.
func Resolve(ctx context.Context, st *store.Store, start int, id int64, from []string, to, reason string, now time.Time) (bool, error) {
	moved, err := st.ResolveOutreach(ctx, id, from, to, reason, now)
	if err != nil || !moved {
		return moved, err
	}
	b, err := LoadBudget(ctx, st, start)
	if err != nil {
		return true, err
	}
	if nb := ApplyOutcome(b, to, start); nb != b {
		if err := SaveBudget(ctx, st, nb); err != nil {
			return true, err
		}
	}
	n, ok, err := st.NudgeFor(ctx, id)
	if err != nil {
		return true, err
	}
	if ok {
		if _, err := st.ResolveOutreach(ctx, n.ID, []string{store.OutcomePending}, to, "", now); err != nil {
			return true, err
		}
	}
	return true, nil
}

// returnProposal puts a proposed interest back to candidate with
// ReturnCooldown when its proposal was dropped or went unanswered after a
// nudge: the user never said no, so it may come up again later.
func returnProposal(ctx context.Context, st *store.Store, o store.Outreach, now time.Time) error {
	if o.Kind != store.OutreachProposal || o.InterestID == 0 {
		return nil
	}
	moved, err := st.SetInterestState(ctx, o.InterestID, store.InterestProposed, store.InterestCandidate)
	if err != nil || !moved {
		return err
	}
	return st.SetInterestCooldown(ctx, o.InterestID, now.Add(ReturnCooldown))
}
