package store

import (
	"context"
	"fmt"
)

// SessionTree is a session's id and every descendant's: the sub-agents it
// launched, theirs, and so on. Empty when the session does not exist.
func (s *Store) SessionTree(ctx context.Context, rootID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE tree(id) AS (
		  SELECT id FROM sessions WHERE id = ?
		  UNION
		  SELECT s.id FROM sessions s JOIN tree t ON s.parent_id = t.id
		)
		SELECT id FROM tree`, rootID)
	if err != nil {
		return nil, fmt.Errorf("session tree for %s: %w", rootID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DeleteSessions deletes each named session with its whole sub-agent tree,
// in one transaction, and returns every id it removed. Unknown ids are
// skipped. The foreign keys cascade the rest: messages, summaries, approvals,
// pending calls, sub-agent runs and bridge bindings; the message triggers
// clear the search index and queue tombstones for the recall mirror.
func (s *Store) DeleteSessions(ctx context.Context, ids []string) ([]string, error) {
	seen := map[string]bool{}
	var all []string
	for _, id := range ids {
		tree, err := s.SessionTree(ctx, id)
		if err != nil {
			return nil, err
		}
		for _, t := range tree {
			if !seen[t] {
				seen[t] = true
				all = append(all, t)
			}
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, s.deleteRows(ctx, all)
}

// DeleteAllSessions deletes every session and returns their ids.
func (s *Store) DeleteAllSessions(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions`)
	if err != nil {
		return nil, fmt.Errorf("list sessions to delete: %w", err)
	}
	var all []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, id)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, s.deleteRows(ctx, all)
}

func (s *Store) deleteRows(ctx context.Context, ids []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete session %s: %w", id, err)
		}
		// A job outlives the chat that created it, and then reports only to
		// the Jobs folder.
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET origin_session_id = '' WHERE origin_session_id = ?`, id); err != nil {
			return fmt.Errorf("clear job origin %s: %w", id, err)
		}
	}
	return tx.Commit()
}

// Binding is one chat-surface channel a bridge has tied to a session.
type Binding struct {
	ExternalID string
	SessionID  string
}

// BindingsForSessions is every channel the named bridge has bound to any of
// the sessions: what a bridge must clean up when they are deleted.
func (s *Store) BindingsForSessions(ctx context.Context, bridge string, sessionIDs []string) ([]Binding, error) {
	var out []Binding
	for _, id := range sessionIDs {
		rows, err := s.db.QueryContext(ctx,
			`SELECT external_id FROM bridge_bindings WHERE bridge = ? AND session_id = ? ORDER BY external_id`, bridge, id)
		if err != nil {
			return nil, fmt.Errorf("read bindings for %s: %w", id, err)
		}
		for rows.Next() {
			b := Binding{SessionID: id}
			if err := rows.Scan(&b.ExternalID); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, b)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// SetSessionJob records which scheduled job a job run belongs to.
func (s *Store) SetSessionJob(ctx context.Context, sessionID string, jobID int64) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET job_id = ? WHERE id = ?`, jobID, sessionID); err != nil {
		return fmt.Errorf("set job of session %s: %w", sessionID, err)
	}
	return nil
}

// MarkSessionSeen records that a human has opened everything the session
// holds right now, so it no longer reads as unread.
func (s *Store) MarkSessionSeen(ctx context.Context, sessionID string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET seen_seq = (SELECT coalesce(max(seq), 0) FROM messages WHERE session_id = ?) WHERE id = ?`,
		sessionID, sessionID); err != nil {
		return fmt.Errorf("mark session %s seen: %w", sessionID, err)
	}
	return nil
}

// JobRuns is every session a job's runs opened, newest first.
func (s *Store) JobRuns(ctx context.Context, jobID int64) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE job_id = ? ORDER BY created_at DESC, id`, jobID)
	if err != nil {
		return nil, fmt.Errorf("list runs of job %d: %w", jobID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}
