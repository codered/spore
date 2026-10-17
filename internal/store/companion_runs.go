package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run kinds: a watch check that a scheduled job ran, or a research run that
// looks into a question.
const (
	RunWatch    = "watch"
	RunResearch = "research"
)

// Run verdicts. A run is pending until it finishes, then done or failed for
// research, and alert or nothing for a watch check.
const (
	VerdictAlert   = "alert"
	VerdictNothing = "nothing"
	VerdictFailed  = "failed"
	VerdictPending = "pending"
	VerdictDone    = "done"
)

// CompanionRun is one watch check or research run the companion started.
type CompanionRun struct {
	ID         int64
	InterestID int64
	JobID      int64
	Kind       string
	SessionID  string
	Question   string
	Verdict    string
	// StartedAt and FinishedAt are zero when unset.
	StartedAt  time.Time
	FinishedAt time.Time
	// ReportedOutreachID is the outreach that reported this run, or 0.
	ReportedOutreachID int64
	// SeenAt is zero until the user saw the run.
	SeenAt time.Time
}

const companionRunColumns = `id, interest_id, job_id, kind, session_id, question, verdict,
       started_at, finished_at, COALESCE(reported_outreach_id, 0), seen_at`

func scanCompanionRun(r rowScanner) (CompanionRun, error) {
	var c CompanionRun
	var started, finished, seen string
	if err := r.Scan(&c.ID, &c.InterestID, &c.JobID, &c.Kind, &c.SessionID, &c.Question, &c.Verdict,
		&started, &finished, &c.ReportedOutreachID, &seen); err != nil {
		return CompanionRun{}, err
	}
	c.StartedAt = parseOptionalTime(started)
	c.FinishedAt = parseOptionalTime(finished)
	c.SeenAt = parseOptionalTime(seen)
	return c, nil
}

// parseOptionalTime reads a stored time; an empty string is the zero time.
func parseOptionalTime(s string) time.Time {
	t, _ := time.Parse(timeFormat, s)
	return t
}

// formatOptionalTime stores the zero time as an empty string.
func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// placeholders returns n SQL placeholders, comma separated. Callers bind one
// argument for each. Only the placeholders are concatenated into a query.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// checkRunFields rejects a kind or verdict the table does not define.
func checkRunFields(kind, verdict string) error {
	switch kind {
	case RunWatch, RunResearch:
	default:
		return fmt.Errorf("companion run kind %q is not watch or research", kind)
	}
	return checkVerdict(verdict)
}

// checkVerdict rejects a verdict the table does not define.
func checkVerdict(verdict string) error {
	switch verdict {
	case VerdictAlert, VerdictNothing, VerdictFailed, VerdictPending, VerdictDone:
		return nil
	}
	return fmt.Errorf("companion run verdict %q is not a known verdict", verdict)
}

// AddCompanionRun records a run the companion started and returns its id.
func (s *Store) AddCompanionRun(ctx context.Context, r CompanionRun) (int64, error) {
	if err := checkRunFields(r.Kind, r.Verdict); err != nil {
		return 0, fmt.Errorf("add companion run: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO companion_runs (interest_id, kind, job_id, session_id, question, verdict,
		   started_at, finished_at, reported_outreach_id, seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.InterestID, r.Kind, r.JobID, r.SessionID, r.Question, r.Verdict,
		formatOptionalTime(r.StartedAt), formatOptionalTime(r.FinishedAt),
		nullID(r.ReportedOutreachID), formatOptionalTime(r.SeenAt))
	if err != nil {
		return 0, fmt.Errorf("add companion run: %w", err)
	}
	return res.LastInsertId()
}

// CompanionRunByJob returns the newest run of the given kind for a job;
// false when the job has none.
func (s *Store) CompanionRunByJob(ctx context.Context, jobID int64, kind string) (CompanionRun, bool, error) {
	c, err := scanCompanionRun(s.db.QueryRowContext(ctx,
		`SELECT `+companionRunColumns+` FROM companion_runs
		 WHERE job_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`, jobID, kind))
	if errors.Is(err, sql.ErrNoRows) {
		return CompanionRun{}, false, nil
	}
	if err != nil {
		return CompanionRun{}, false, fmt.Errorf("read companion run of job %d: %w", jobID, err)
	}
	return c, true, nil
}

// SetCompanionRunSession records the run session a research run uses.
func (s *Store) SetCompanionRunSession(ctx context.Context, id int64, sessionID string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE companion_runs SET session_id = ? WHERE id = ?`, sessionID, id); err != nil {
		return fmt.Errorf("set companion run %d session: %w", id, err)
	}
	return nil
}

// FinishCompanionRun sets a run's verdict and finish time. It moves only a
// pending run, and reports whether it moved. A second finish is a no-op.
func (s *Store) FinishCompanionRun(ctx context.Context, id int64, verdict string, at time.Time) (bool, error) {
	if err := checkVerdict(verdict); err != nil {
		return false, fmt.Errorf("finish companion run %d: %w", id, err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE companion_runs SET verdict = ?, finished_at = ? WHERE id = ? AND verdict = ?`,
		verdict, formatOptionalTime(at), id, VerdictPending)
	if err != nil {
		return false, fmt.Errorf("finish companion run %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CompanionRuns lists runs started at or after since, newest first.
func (s *Store) CompanionRuns(ctx context.Context, since time.Time) ([]CompanionRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+companionRunColumns+` FROM companion_runs WHERE started_at >= ? ORDER BY id DESC`,
		formatOptionalTime(since))
	if err != nil {
		return nil, fmt.Errorf("read companion runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []CompanionRun
	for rows.Next() {
		c, err := scanCompanionRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetCompanionRunsReported links runs to the outreach that reported them.
// Only runs not yet reported change, so the first report wins. An empty ids
// list is a no-op.
func (s *Store) SetCompanionRunsReported(ctx context.Context, ids []int64, outreachID int64) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{nullID(outreachID)}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE companion_runs SET reported_outreach_id = ? ` + //nolint:gosec // G202: only placeholders are concatenated; every value is bound
		`WHERE reported_outreach_id IS NULL AND id IN (` + placeholders(len(ids)) + `)`
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("set companion runs reported: %w", err)
	}
	return nil
}

// SetCompanionRunsSeen marks runs as seen at a time. Only runs not yet seen
// change, so the first time sticks. An empty ids list is a no-op.
func (s *Store) SetCompanionRunsSeen(ctx context.Context, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{formatOptionalTime(at)}
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE companion_runs SET seen_at = ? WHERE seen_at = '' AND id IN (` + placeholders(len(ids)) + `)` //nolint:gosec // G202: only placeholders are concatenated; every value is bound
	if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("set companion runs seen: %w", err)
	}
	return nil
}

// DeleteCompanionRunsBefore removes runs started before the given time and
// returns how many it removed.
func (s *Store) DeleteCompanionRunsBefore(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM companion_runs WHERE started_at < ?`, formatOptionalTime(before))
	if err != nil {
		return 0, fmt.Errorf("delete companion runs: %w", err)
	}
	return res.RowsAffected()
}
