package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Sub-agent run states. A run is running until exactly one of the other
// three replaces it.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// SubagentRun is one sub-agent's bookkeeping row.
type SubagentRun struct {
	SessionID string
	ParentID  string
	Prompt    string
	Depth     int
	State     string
	Result    string
	Error     string
	StartedAt time.Time
	EndedAt   time.Time // zero while running
}

func (s *Store) StartSubagentRun(ctx context.Context, r SubagentRun) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subagent_runs (session_id, parent_id, prompt, depth, state, started_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.SessionID, r.ParentID, r.Prompt, r.Depth, RunRunning, nowString())
	if err != nil {
		return fmt.Errorf("start subagent run %s: %w", r.SessionID, err)
	}
	return nil
}

// FinishSubagentRun records a terminal state. It only ever moves a running
// row: a cancel racing a natural completion must not overwrite the result
// the child actually produced.
func (s *Store) FinishSubagentRun(ctx context.Context, sessionID, state, result, errText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subagent_runs SET state = ?, result = ?, error = ?, ended_at = ?
		 WHERE session_id = ? AND state = ?`,
		state, result, errText, nowString(), sessionID, RunRunning)
	if err != nil {
		return fmt.Errorf("finish subagent run %s: %w", sessionID, err)
	}
	return nil
}

func scanRun(sc interface{ Scan(...any) error }) (SubagentRun, error) {
	var r SubagentRun
	var started, ended string
	if err := sc.Scan(&r.SessionID, &r.ParentID, &r.Prompt, &r.Depth,
		&r.State, &r.Result, &r.Error, &started, &ended); err != nil {
		return SubagentRun{}, err
	}
	r.StartedAt, _ = time.Parse(timeFormat, started)
	if ended != "" {
		r.EndedAt, _ = time.Parse(timeFormat, ended)
	}
	return r, nil
}

const runColumns = `session_id, parent_id, prompt, depth, state, result, error, started_at, ended_at`

func (s *Store) SubagentRun(ctx context.Context, sessionID string) (SubagentRun, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM subagent_runs WHERE session_id = ?`, sessionID)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return SubagentRun{}, false, nil
	}
	if err != nil {
		return SubagentRun{}, false, fmt.Errorf("read subagent run %s: %w", sessionID, err)
	}
	return r, true, nil
}

func (s *Store) SubagentRunsByParent(ctx context.Context, parentID string) ([]SubagentRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM subagent_runs WHERE parent_id = ? ORDER BY started_at`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list subagent runs: %w", err)
	}
	defer rows.Close()
	var out []SubagentRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterruptRunningSubagents marks every still-running row interrupted. The
// goroutines that were driving them died with the previous process, so a
// running row at startup describes work that stopped without saying so.
func (s *Store) InterruptRunningSubagents(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE subagent_runs SET state = ?, error = ?, ended_at = ? WHERE state = ?`,
		RunInterrupted, "the process running this sub-agent exited", nowString(), RunRunning)
	if err != nil {
		return 0, fmt.Errorf("interrupt orphaned subagent runs: %w", err)
	}
	return res.RowsAffected()
}

// TreeCost sums what a session and every descendant have spent. The walk is
// a recursive CTE rather than repeated queries because the ceiling is checked
// before every child turn.
func (s *Store) TreeCost(ctx context.Context, rootID string) (float64, error) {
	var total sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE tree(id) AS (
		  SELECT id FROM sessions WHERE id = ?
		  UNION
		  SELECT s.id FROM sessions s JOIN tree t ON s.parent_id = t.id
		)
		SELECT SUM(m.cost_usd) FROM messages m JOIN tree ON m.session_id = tree.id`,
		rootID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("tree cost for %s: %w", rootID, err)
	}
	return total.Float64, nil
}
