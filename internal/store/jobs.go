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
	defer rows.Close()
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

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	enabled := 0
	if j.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (kind, spec, prompt, enabled, next_run, last_session_id, created_at)
		 VALUES (?, ?, ?, ?, ?, '', ?)`,
		j.Kind, j.Spec, j.Prompt, enabled,
		j.NextRun.UTC().Format(timeFormat), time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	return res.LastInsertId()
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		var next, created string
		var last, lastSession sql.NullString
		if err := rows.Scan(&j.ID, &j.Kind, &j.Spec, &j.Prompt, &enabled, &next, &last, &lastSession, &created); err != nil {
			return nil, err
		}
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

const jobColumns = `id, kind, spec, prompt, enabled, next_run, last_run, last_session_id, created_at`

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return scanJobs(rows)
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
