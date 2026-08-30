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
