package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/provider"
)

// Recall index kinds. They are the same strings internal/recall exports;
// the store cannot import that package without a cycle, so the values are
// repeated here and pinned by a test in internal/recall.
const (
	kindMessage = "message"
	kindSummary = "summary"
	kindFact    = "fact"
)

// DB exposes the handle so internal/recall/sqlitefts can query the index
// without the store growing search methods of its own. The store owns writes
// to recall_fts; the backend only reads.
func (s *Store) DB() *sql.DB { return s.db }

// indexableText extracts the part of a message that may be searched. Text
// blocks only: tool_result blocks carry third-party content -- a fetched page,
// an MCP server's reply -- and indexing them would make an injected string
// permanently retrievable. tool_use blocks are arguments, not prose.
func indexableText(blocksJSON []byte) string {
	var blocks []provider.Block
	if err := json.Unmarshal(blocksJSON, &blocks); err != nil {
		// A message whose blocks will not parse is already broken for every
		// other reader; it simply contributes nothing to the index.
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == provider.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertIndex(ctx context.Context, e execer, kind, refID, sessionID, createdAt, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	_, err := e.ExecContext(ctx,
		`INSERT INTO recall_fts (text, kind, ref_id, session_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		text, kind, refID, sessionID, createdAt)
	if err != nil {
		return fmt.Errorf("index %s %s: %w", kind, refID, err)
	}
	return nil
}

func deleteIndex(ctx context.Context, e execer, kind, refID string) error {
	_, err := e.ExecContext(ctx, `DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?`, kind, refID)
	if err != nil {
		return fmt.Errorf("unindex %s %s: %w", kind, refID, err)
	}
	return nil
}

// IndexFact makes one fact file searchable, replacing whatever was indexed
// under that name. Facts are file-owned, so this is called by whoever loads or
// writes them rather than by a trigger.
func (s *Store) IndexFact(ctx context.Context, name, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteIndex(ctx, tx, kindFact, name); err != nil {
		return err
	}
	if err := insertIndex(ctx, tx, kindFact, name, "", nowString(), text); err != nil {
		return err
	}
	return tx.Commit()
}

// UnindexFact drops a deleted fact from the index.
func (s *Store) UnindexFact(ctx context.Context, name string) error {
	return deleteIndex(ctx, s.db, kindFact, name)
}

// ReindexAll rebuilds the message and summary rows from the source tables and
// reports how many it wrote. Fact rows are left alone: they belong to files
// this package cannot read, and their owner reindexes them. This is the repair
// path for an index that has drifted, and the backfill for a database written
// before recall existed.
func (s *Store) ReindexAll(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recall_fts WHERE kind IN (?, ?)`, kindMessage, kindSummary); err != nil {
		return 0, fmt.Errorf("clear index: %w", err)
	}

	n := 0
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, blocks, created_at FROM messages ORDER BY id`)
	if err != nil {
		return 0, err
	}
	// text holds a message's blocks JSON or a summary's text, depending on
	// which query filled it.
	type row struct {
		id                     int64
		session, text, created string
	}
	var msgs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.session, &r.text, &r.created); err != nil {
			rows.Close()
			return 0, err
		}
		msgs = append(msgs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range msgs {
		text := indexableText([]byte(r.text))
		if strings.TrimSpace(text) == "" {
			continue
		}
		if err := insertIndex(ctx, tx, kindMessage, strconv.FormatInt(r.id, 10), r.session, r.created, text); err != nil {
			return 0, err
		}
		n++
	}

	srows, err := tx.QueryContext(ctx, `SELECT session_id, text, created_at FROM summaries`)
	if err != nil {
		return 0, err
	}
	var sums []row
	for srows.Next() {
		var r row
		if err := srows.Scan(&r.session, &r.text, &r.created); err != nil {
			srows.Close()
			return 0, err
		}
		sums = append(sums, r)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return 0, err
	}
	for _, r := range sums {
		if err := insertIndex(ctx, tx, kindSummary, r.session, r.session, r.created, r.text); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// backfillRecall populates an index that has never been written. The guard is
// deliberately "no rows at all": a database with history and an empty index is
// either an upgrade or a wiped index, and both want the same repair.
func backfillRecall(db *sql.DB) error {
	var indexed, messages int
	if err := db.QueryRow(`SELECT count(*) FROM recall_fts`).Scan(&indexed); err != nil {
		return fmt.Errorf("inspect recall index: %w", err)
	}
	if indexed > 0 {
		return nil
	}
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		return err
	}
	if messages == 0 {
		return nil
	}
	s := &Store{db: db}
	_, err := s.ReindexAll(context.Background())
	return err
}
