// Package store persists sessions, messages and summaries in one SQLite file.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct{ db *sql.DB }

type Session struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID         int64
	SessionID  string
	Seq        int
	Role       string
	BlocksJSON []byte
	Model      string
	CallSite   string
	TokensIn   int
	TokensOut  int
	CostUSD    float64
	CreatedAt  time.Time
}

// Fixed-width so the text column sorts chronologically; still parses as
// plain time.RFC3339.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer keeps WAL contention out of the picture; the daemon is a
	// single process and writes are short.
	db.SetMaxOpenConns(1)
	if err := migrateJobs(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

func (s *Store) CreateSession(ctx context.Context, title string) (string, error) {
	id := newID()
	now := time.Now().UTC().Format(timeFormat)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, title, now, now)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, created_at, updated_at FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.Title, &created, &updated); err != nil {
			return nil, err
		}
		sess.CreatedAt, _ = time.Parse(timeFormat, created)
		sess.UpdatedAt, _ = time.Parse(timeFormat, updated)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Session returns a session by ID, or (Session{}, false, nil) if not found.
func (s *Store) Session(ctx context.Context, id string) (Session, bool, error) {
	var sess Session
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &created, &updated)
	if err == sql.ErrNoRows {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("read session: %w", err)
	}
	sess.CreatedAt, _ = time.Parse(timeFormat, created)
	sess.UpdatedAt, _ = time.Parse(timeFormat, updated)
	return sess, true, nil
}

// AppendMessage assigns the next seq for the session and writes the row.
func (s *Store) AppendMessage(ctx context.Context, m Message) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, m.SessionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("next seq: %w", err)
	}
	now := time.Now().UTC().Format(timeFormat)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (session_id, seq, role, blocks, model, call_site, tokens_in, tokens_out, cost_usd, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, next, m.Role, string(m.BlocksJSON), m.Model, m.CallSite,
		m.TokensIn, m.TokensOut, m.CostUSD, now)
	if err != nil {
		return 0, fmt.Errorf("append message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, m.SessionID); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, seq, role, blocks, model, call_site, tokens_in, tokens_out, cost_usd, created_at
		 FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var blocks, created string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &blocks, &m.Model,
			&m.CallSite, &m.TokensIn, &m.TokensOut, &m.CostUSD, &created); err != nil {
			return nil, err
		}
		m.BlocksJSON = []byte(blocks)
		m.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetSummary(ctx context.Context, sessionID, summary string, throughSeq int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO summaries (session_id, text, through_seq, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET text = excluded.text, through_seq = excluded.through_seq, created_at = excluded.created_at`,
		sessionID, summary, throughSeq, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("set summary: %w", err)
	}
	return nil
}

// Summary returns ("", 0, nil) when the session has never been compacted.
func (s *Store) Summary(ctx context.Context, sessionID string) (string, int, error) {
	var text string
	var through int
	err := s.db.QueryRowContext(ctx,
		`SELECT text, through_seq FROM summaries WHERE session_id = ?`, sessionID).Scan(&text, &through)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("read summary: %w", err)
	}
	return text, through, nil
}

// PendingCall is a tool call whose turn is suspended awaiting approval.
type PendingCall struct {
	ID        int64
	SessionID string
	ToolUseID string
	Tool      string
	Profile   string
	Rule      string
	ArgsJSON  []byte
	CreatedAt time.Time
}

// AddPendingCall records a suspension before the turn blocks on an answer.
func (s *Store) AddPendingCall(ctx context.Context, p PendingCall) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_calls (session_id, tool_use_id, tool, args, profile, rule, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		p.SessionID, p.ToolUseID, p.Tool, string(p.ArgsJSON), p.Profile, p.Rule,
		time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("add pending call: %w", err)
	}
	return res.LastInsertId()
}

// PendingCalls returns the session's unanswered approvals, oldest first.
func (s *Store) PendingCalls(ctx context.Context, sessionID string) ([]PendingCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, tool_use_id, tool, args, profile, rule, created_at
		 FROM pending_calls WHERE session_id = ? AND state = 'pending' ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read pending calls: %w", err)
	}
	defer rows.Close()
	var out []PendingCall
	for rows.Next() {
		var p PendingCall
		var args, created string
		if err := rows.Scan(&p.ID, &p.SessionID, &p.ToolUseID, &p.Tool, &args, &p.Profile, &p.Rule, &created); err != nil {
			return nil, err
		}
		p.ArgsJSON = []byte(args)
		p.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolvePendingCall closes a suspension with the decision that ended it:
// allow, deny, timeout or error. The state guard makes it idempotent — a
// second call for an already-answered suspension changes nothing.
func (s *Store) ResolvePendingCall(ctx context.Context, id int64, decision string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_calls SET state = ?, decided_at = ? WHERE id = ? AND state = 'pending'`,
		decision, time.Now().UTC().Format(timeFormat), id)
	if err != nil {
		return fmt.Errorf("resolve pending call %d: %w", id, err)
	}
	return nil
}

// ClaimPendingCall resolves a suspension AND writes its audit row in one
// transaction, returning the call it claimed. The bool reports whether this
// caller won: a second client answering the same suspension concurrently
// finds it no longer pending and gets false, so two clients can never record
// contradictory answers, and a partial failure can never leave a resolved
// row with no audit entry behind it.
func (s *Store) ClaimPendingCall(ctx context.Context, id int64, sessionID, decision, scope string) (PendingCall, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PendingCall{}, false, err
	}
	defer tx.Rollback()

	var p PendingCall
	var args, created string
	err = tx.QueryRowContext(ctx,
		`SELECT id, session_id, tool_use_id, tool, args, profile, rule, created_at
		 FROM pending_calls WHERE id = ? AND session_id = ? AND state = 'pending'`,
		id, sessionID).Scan(&p.ID, &p.SessionID, &p.ToolUseID, &p.Tool, &args, &p.Profile, &p.Rule, &created)
	if err == sql.ErrNoRows {
		return PendingCall{}, false, nil
	}
	if err != nil {
		return PendingCall{}, false, fmt.Errorf("claim pending call %d: %w", id, err)
	}
	p.ArgsJSON = []byte(args)
	p.CreatedAt, _ = time.Parse(timeFormat, created)

	now := time.Now().UTC().Format(timeFormat)
	if _, err := tx.ExecContext(ctx,
		`UPDATE pending_calls SET state = ?, decided_at = ? WHERE id = ? AND state = 'pending'`,
		decision, now, id); err != nil {
		return PendingCall{}, false, fmt.Errorf("claim pending call %d: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO approvals (session_id, tool, args, decision, scope, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, p.Tool, args, decision, scope, now); err != nil {
		return PendingCall{}, false, fmt.Errorf("claim pending call %d: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return PendingCall{}, false, err
	}
	return p, true, nil
}

// RecordApproval appends to the audit log. Only scope "session" is consulted
// later, by SessionDecision; "once" is audit-only and "pattern" is written
// into the config file instead.
func (s *Store) RecordApproval(ctx context.Context, sessionID, tool string, args []byte, decision, scope string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (session_id, tool, args, decision, scope, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, tool, string(args), decision, scope, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("record approval: %w", err)
	}
	return nil
}

// SessionDecision returns the most recent session-scoped answer for a tool in
// this session, if any. "Always this session" is remembered per tool, not per
// argument: the user answered about a capability, not about one path.
func (s *Store) SessionDecision(ctx context.Context, sessionID, tool string) (string, bool, error) {
	var decision string
	err := s.db.QueryRowContext(ctx,
		`SELECT decision FROM approvals
		 WHERE session_id = ? AND tool = ? AND scope = 'session'
		 ORDER BY id DESC LIMIT 1`, sessionID, tool).Scan(&decision)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read session decision: %w", err)
	}
	return decision, true, nil
}
