package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Refinement statuses. A round writes applied or proposed; the rest are
// where a row goes afterwards.
const (
	RefineApplied    = "applied"
	RefineProposed   = "proposed"
	RefineRejected   = "rejected"
	RefineRolledBack = "rolled_back"
	RefineStale      = "stale"
	RefineFailed     = "failed"
)

// Refinement is one ledger row: an edit a refinement round made or proposed.
// Before and After are whole-file contents; nil means "no file".
type Refinement struct {
	ID        int64
	RoundID   string
	SessionID string
	Trigger   string
	Kind      string
	Target    string
	Before    *string
	After     *string
	Rationale string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const refinementCols = `id, round_id, session_id, trigger, kind, target, before, after, rationale, status, created_at, updated_at`

func scanRefinement(r rowScanner) (Refinement, error) {
	var x Refinement
	var before, after sql.NullString
	var created, updated string
	if err := r.Scan(&x.ID, &x.RoundID, &x.SessionID, &x.Trigger, &x.Kind, &x.Target,
		&before, &after, &x.Rationale, &x.Status, &created, &updated); err != nil {
		return Refinement{}, err
	}
	if before.Valid {
		s := before.String
		x.Before = &s
	}
	if after.Valid {
		s := after.String
		x.After = &s
	}
	x.CreatedAt, _ = time.Parse(timeFormat, created)
	x.UpdatedAt, _ = time.Parse(timeFormat, updated)
	return x, nil
}

// nullable maps a nil content pointer to SQL NULL.
func nullable(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func (s *Store) AddRefinement(ctx context.Context, x Refinement) (int64, error) {
	now := nowString()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO refinements (round_id, session_id, trigger, kind, target, before, after, rationale, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		x.RoundID, x.SessionID, x.Trigger, x.Kind, x.Target, nullable(x.Before), nullable(x.After),
		x.Rationale, x.Status, now, now)
	if err != nil {
		return 0, fmt.Errorf("add refinement: %w", err)
	}
	return res.LastInsertId()
}

func (s *Store) Refinement(ctx context.Context, id int64) (Refinement, bool, error) {
	x, err := scanRefinement(s.db.QueryRowContext(ctx, `SELECT `+refinementCols+` FROM refinements WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Refinement{}, false, nil
	}
	if err != nil {
		return Refinement{}, false, err
	}
	return x, true, nil
}

// Refinements lists the newest rows first, filtered to one status unless
// status is "".
func (s *Store) Refinements(ctx context.Context, status string, limit int) ([]Refinement, error) {
	q := `SELECT ` + refinementCols + ` FROM refinements`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryRefinements(ctx, q, args...)
}

// RoundRefinements returns one round's rows in the order they were written.
func (s *Store) RoundRefinements(ctx context.Context, roundID string) ([]Refinement, error) {
	return s.queryRefinements(ctx, `SELECT `+refinementCols+` FROM refinements WHERE round_id = ? ORDER BY id`, roundID)
}

func (s *Store) queryRefinements(ctx context.Context, q string, args ...any) ([]Refinement, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read refinements: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Refinement
	for rows.Next() {
		x, err := scanRefinement(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// LatestAppliedRound is the most recent round in the session that still has
// an applied row: what "/refine rollback" undoes.
func (s *Store) LatestAppliedRound(ctx context.Context, sessionID string) (string, bool, error) {
	var round string
	err := s.db.QueryRowContext(ctx,
		`SELECT round_id FROM refinements WHERE session_id = ? AND status = ? ORDER BY id DESC LIMIT 1`,
		sessionID, RefineApplied).Scan(&round)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return round, true, nil
}

// SetRefinementStatus moves a row from one status to another and reports
// whether it did. The from check makes accept, reject and rollback safe
// against a second click racing the first.
func (s *Store) SetRefinementStatus(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE refinements SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		to, nowString(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RefinedThrough is the seq of the last message a successful round reviewed.
func (s *Store) RefinedThrough(ctx context.Context, sessionID string) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx, `SELECT refined_through FROM sessions WHERE id = ?`, sessionID).Scan(&seq)
	return seq, err
}

// SetRefinedThrough advances the watermark. It never moves it backwards: a
// compaction-triggered round over an older range must not reopen messages a
// later round already reviewed.
func (s *Store) SetRefinedThrough(ctx context.Context, sessionID string, seq int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET refined_through = max(refined_through, ?) WHERE id = ?`, seq, sessionID)
	return err
}

// MarkRefineAttempt records that a round started, so the idle sweeper does
// not retry a failing session every tick.
func (s *Store) MarkRefineAttempt(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET refine_attempted_at = ? WHERE id = ?`, nowString(), sessionID)
	return err
}

// IdleSessions lists top-level sessions last updated before `before` that
// have a user message the refiner has not reviewed, and that have not been
// attempted since their last update. Scheduled-job sessions are excluded:
// each job run opens a fresh session, and their "user" turn is the job prompt,
// so they are never swept for idle review.
func (s *Store) IdleSessions(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.id FROM sessions s
		 WHERE s.source NOT IN (?, ?) AND s.parent_id = ''
		   AND s.updated_at < ?
		   AND s.refine_attempted_at < s.updated_at
		   AND EXISTS (SELECT 1 FROM messages m
		                WHERE m.session_id = s.id AND m.role = 'user' AND m.seq > s.refined_through)
		 ORDER BY s.updated_at`,
		SourceSubagent, SourceJob, before.UTC().Format(timeFormat))
	if err != nil {
		return nil, fmt.Errorf("idle sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
