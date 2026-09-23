package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Job is one scheduled prompt. Kind is "cron" (Spec is a five-field cron
// expression) or "once" (Spec is an RFC3339 instant). Firing opens a fresh
// session; LastSessionID records the most recent one it produced.
type Job struct {
	ID            int64
	Kind          string
	Spec          string
	Prompt        string
	Enabled       bool
	NextRun       time.Time
	LastRun       time.Time
	LastSessionID string
	CreatedAt     time.Time
	// OriginSessionID is the root of the session the job was created in:
	// the one chat that hears about its runs. Empty for a job created with
	// no session, for every job older than the column, and once that
	// session is deleted.
	OriginSessionID string
	// Notify is how later runs are reported to the origin: NotifyAsk until
	// the user has answered the first-run check-in.
	Notify string
	// CheckedIn is set once the first-run check-in has been claimed.
	CheckedIn bool
}

// Notification modes for a job's runs after its first-run check-in.
const (
	NotifyAsk      = "ask"
	NotifyEach     = "each"
	NotifyFailures = "failures"
	NotifyNone     = "none"
)

// ValidNotify reports whether mode is one a user may choose. NotifyAsk is
// not: it is only the state before anyone has answered.
func ValidNotify(mode string) bool {
	return mode == NotifyEach || mode == NotifyFailures || mode == NotifyNone
}

// migrateJobs replaces the unused Plan 2 jobs stub with the scheduler's
// shape. The stub had a "schedule" column and no "kind"; nothing ever wrote
// a row to it, so dropping it loses no user data. Guarded on the column
// check so a database already on the new shape is left untouched — Open runs
// this on every start.
func migrateJobs(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return fmt.Errorf("inspect jobs table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var columns int
	hasKind := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns++
		if name == "kind" {
			hasKind = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// No table at all: the schema statement will create the right one.
	if columns == 0 || hasKind {
		return nil
	}
	if _, err := db.Exec(`DROP TABLE jobs`); err != nil {
		return fmt.Errorf("drop legacy jobs table: %w", err)
	}
	return nil
}

// migrateJobColumns adds the check-in columns to a jobs table written before
// they existed. It runs after the schema, so the table is always there. An
// older job gets no origin, which is what keeps it from ever checking in.
func migrateJobColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return fmt.Errorf("inspect jobs table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range []struct{ name, decl string }{
		{"origin_session_id", `TEXT NOT NULL DEFAULT ''`},
		{"notify", `TEXT NOT NULL DEFAULT 'ask'`},
		{"checked_in", `INTEGER NOT NULL DEFAULT 0`},
	} {
		if have[col.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE jobs ADD COLUMN ` + col.name + ` ` + col.decl); err != nil {
			return fmt.Errorf("add jobs.%s: %w", col.name, err)
		}
	}
	return nil
}

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	enabled := 0
	if j.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (kind, spec, prompt, enabled, next_run, last_session_id, created_at, origin_session_id, notify)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?)`,
		j.Kind, j.Spec, j.Prompt, enabled,
		j.NextRun.UTC().Format(timeFormat), time.Now().UTC().Format(timeFormat),
		j.OriginSessionID, NotifyAsk)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	return res.LastInsertId()
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		var next, created string
		var last, lastSession sql.NullString
		var checkedIn int
		if err := rows.Scan(&j.ID, &j.Kind, &j.Spec, &j.Prompt, &enabled, &next, &last, &lastSession, &created,
			&j.OriginSessionID, &j.Notify, &checkedIn); err != nil {
			return nil, err
		}
		j.CheckedIn = checkedIn != 0
		j.Enabled = enabled != 0
		j.NextRun, _ = time.Parse(timeFormat, next)
		if last.Valid {
			j.LastRun, _ = time.Parse(timeFormat, last.String)
		}
		j.LastSessionID = lastSession.String
		j.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, j)
	}
	return out, rows.Err()
}

const jobColumns = `id, kind, spec, prompt, enabled, next_run, last_run, last_session_id, created_at, origin_session_id, notify, checked_in`

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return scanJobs(rows)
}

// Job reads one job; found is false when there is no such id.
func (s *Store) Job(ctx context.Context, id int64) (Job, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, id)
	if err != nil {
		return Job{}, false, fmt.Errorf("read job %d: %w", id, err)
	}
	jobs, err := scanJobs(rows)
	if err != nil || len(jobs) == 0 {
		return Job{}, false, err
	}
	return jobs[0], true, nil
}

// DueJobs returns enabled jobs whose next_run has passed, oldest first.
func (s *Store) DueJobs(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE enabled = 1 AND next_run <= ? ORDER BY next_run`,
		now.UTC().Format(timeFormat))
	if err != nil {
		return nil, fmt.Errorf("read due jobs: %w", err)
	}
	return scanJobs(rows)
}

// MarkJobRun records a firing and moves the job forward. A zero next time
// means the job has no further runs and is disabled — the one-shot case.
func (s *Store) MarkJobRun(ctx context.Context, id int64, ran, next time.Time, sessionID string) error {
	if next.IsZero() {
		_, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET enabled = 0, last_run = ?, last_session_id = ? WHERE id = ?`,
			ran.UTC().Format(timeFormat), sessionID, id)
		if err != nil {
			return fmt.Errorf("retire job %d: %w", id, err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET last_run = ?, next_run = ?, last_session_id = ? WHERE id = ?`,
		ran.UTC().Format(timeFormat), next.UTC().Format(timeFormat), sessionID, id)
	if err != nil {
		return fmt.Errorf("advance job %d: %w", id, err)
	}
	return nil
}

func (s *Store) SetJobEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set job %d enabled: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no job %d", id)
	}
	return nil
}

// SetJobLastSession records the session a firing produced. It is separate
// from MarkJobRun because the advance must be persisted BEFORE the job runs,
// while the session id only exists afterwards.
func (s *Store) SetJobLastSession(ctx context.Context, id int64, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET last_session_id = ? WHERE id = ?`, sessionID, id)
	if err != nil {
		return fmt.Errorf("record job %d session: %w", id, err)
	}
	return nil
}

// SetJobNotify sets how a job's later runs are reported to its origin chat.
func (s *Store) SetJobNotify(ctx context.Context, id int64, mode string) error {
	if !ValidNotify(mode) {
		return fmt.Errorf("notify mode must be each, failures or none, not %q", mode)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET notify = ? WHERE id = ?`, mode, id)
	if err != nil {
		return fmt.Errorf("set job %d notify: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no job %d", id)
	}
	return nil
}

// ClaimJobCheckIn marks the job's first-run check-in as taken, reporting
// false when it already was. It is a claim rather than a plain write so two
// runs ending together cannot both check in.
func (s *Store) ClaimJobCheckIn(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET checked_in = 1 WHERE id = ? AND checked_in = 0`, id)
	if err != nil {
		return false, fmt.Errorf("claim job %d check-in: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
