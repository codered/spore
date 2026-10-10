package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Interest states. See the companion spec, section 2.1, for the transitions.
const (
	InterestObserving = "observing"
	InterestCandidate = "candidate"
	InterestProposed  = "proposed"
	InterestActive    = "active"
	InterestDeclined  = "declined"
	InterestRetired   = "retired"
)

// Interest is one row of interests with its evidence summarised.
type Interest struct {
	ID            int64
	Key           string
	Label         string
	State         string
	WatchJobID    int64
	CooldownUntil time.Time
	// DaysSeen, FirstSeen and LastSeen are derived from interest_signals.
	// FirstSeen and LastSeen are zero when there are no signals.
	DaysSeen  int
	FirstSeen time.Time
	LastSeen  time.Time
}

const interestSelect = `
SELECT i.id, i.key, i.label, i.state, COALESCE(i.watch_job_id, 0), i.cooldown_until,
       COUNT(DISTINCT sg.day), COALESCE(MIN(sg.seen_at), ''), COALESCE(MAX(sg.seen_at), '')
  FROM interests i LEFT JOIN interest_signals sg ON sg.interest_id = i.id`

func scanInterest(r rowScanner) (Interest, error) {
	var x Interest
	var cooldown, first, last string
	if err := r.Scan(&x.ID, &x.Key, &x.Label, &x.State, &x.WatchJobID, &cooldown,
		&x.DaysSeen, &first, &last); err != nil {
		return Interest{}, err
	}
	x.CooldownUntil, _ = time.Parse(timeFormat, cooldown)
	x.FirstSeen, _ = time.Parse(timeFormat, first)
	x.LastSeen, _ = time.Parse(timeFormat, last)
	return x, nil
}

// TouchInterest returns the interest for key, creating it as observing when
// it is new. A retired interest seen again goes back to observing; every
// other state is left alone -- a declined interest stays declined however
// often it comes up. The label is the first one recorded.
func (s *Store) TouchInterest(ctx context.Context, key, label string) (Interest, error) {
	now := nowString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO interests (key, label, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`, key, label, InterestObserving, now, now); err != nil {
		return Interest{}, fmt.Errorf("touch interest: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, updated_at = ? WHERE key = ? AND state = ?`,
		InterestObserving, now, key, InterestRetired); err != nil {
		return Interest{}, fmt.Errorf("touch interest: %w", err)
	}
	in, ok, err := s.InterestByKey(ctx, key)
	if err != nil {
		return Interest{}, err
	}
	if !ok {
		return Interest{}, fmt.Errorf("touch interest: %s vanished", key)
	}
	return in, nil
}

// AddInterestSignal records one sighting. day is the local date
// ("2006-01-02") the caller computed in the companion's timezone.
func (s *Store) AddInterestSignal(ctx context.Context, interestID int64, sessionID, kind, day string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO interest_signals (interest_id, session_id, kind, day, seen_at) VALUES (?, ?, ?, ?, ?)`,
		interestID, sessionID, kind, day, at.UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("add interest signal: %w", err)
	}
	return nil
}

// Interests lists interests ordered by key, limited to the given states
// when any are given.
func (s *Store) Interests(ctx context.Context, states ...string) ([]Interest, error) {
	q := interestSelect
	var args []any
	if len(states) > 0 {
		q += ` WHERE i.state IN (?` + strings.Repeat(`, ?`, len(states)-1) + `)`
		for _, st := range states {
			args = append(args, st)
		}
	}
	q += ` GROUP BY i.id ORDER BY i.key`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read interests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Interest
	for rows.Next() {
		x, err := scanInterest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// InterestByKey returns the interest stored under key, and false when there
// is none.
func (s *Store) InterestByKey(ctx context.Context, key string) (Interest, bool, error) {
	x, err := scanInterest(s.db.QueryRowContext(ctx, interestSelect+` WHERE i.key = ? GROUP BY i.id`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Interest{}, false, nil
	}
	if err != nil {
		return Interest{}, false, err
	}
	return x, true, nil
}

// SetInterestState moves an interest from one state to another and reports
// whether it did; the from check makes a racing second move a no-op.
func (s *Store) SetInterestState(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, nowString(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
