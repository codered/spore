// Package sqlitefts implements recall.Recall over the recall_fts table in
// spore's own database. It is the default backend: no setup, no service, and
// available wherever the binary runs.
package sqlitefts

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/recall"
)

// Queryer is the slice of *sql.DB this backend needs. Taking an interface
// keeps the backend testable against a bare in-memory database, without the
// store's session machinery.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Backend struct{ db Queryer }

func New(db Queryer) *Backend { return &Backend{db: db} }

// timeFormat matches the store's, so parsing a created_at written by either
// side yields the same instant.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Index writes chunks directly. The store already indexes messages and
// summaries inside their own transactions; this path exists for facts and for
// tests, and replaces any row with the same kind and id.
func (b *Backend) Index(ctx context.Context, chunks []recall.Chunk) error {
	for _, c := range chunks {
		if _, err := b.db.ExecContext(ctx,
			`DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?`, c.Kind, c.ID); err != nil {
			return fmt.Errorf("replace %s %s: %w", c.Kind, c.ID, err)
		}
		created := c.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, err := b.db.ExecContext(ctx,
			`INSERT INTO recall_fts (text, kind, ref_id, session_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.Text, c.Kind, c.ID, c.SessionID, created.UTC().Format(timeFormat)); err != nil {
			return fmt.Errorf("index %s %s: %w", c.Kind, c.ID, err)
		}
	}
	return nil
}

func (b *Backend) Search(ctx context.Context, q recall.Query) ([]recall.Hit, error) {
	match := recall.Tokenize(q.Text)
	if match == "" {
		// An empty MATCH expression is a syntax error, and a query with no
		// searchable characters has no hits by definition.
		return nil, nil
	}

	// bm25 is negative and more-negative is a better match, so best-first is
	// ascending. Ordering descending here would silently return the worst hits.
	sb := strings.Builder{}
	sb.WriteString(`SELECT text, kind, ref_id, session_id, created_at,
		bm25(recall_fts) AS score, snippet(recall_fts, 0, '', '', '…', 12)
		FROM recall_fts WHERE recall_fts MATCH ?`)
	args := []any{match}
	if len(q.Kinds) > 0 {
		sb.WriteString(" AND kind IN (")
		for i, k := range q.Kinds {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
			args = append(args, k)
		}
		sb.WriteByte(')')
	}
	if q.SessionID != "" {
		sb.WriteString(" AND session_id = ?")
		args = append(args, q.SessionID)
	}
	sb.WriteString(" ORDER BY score LIMIT ?")
	args = append(args, recall.ClampK(q.K))

	rows, err := b.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("recall search: %w", err)
	}
	defer rows.Close()

	var hits []recall.Hit
	for rows.Next() {
		var h recall.Hit
		var created string
		if err := rows.Scan(&h.Text, &h.Kind, &h.ID, &h.SessionID, &created, &h.Score, &h.Excerpt); err != nil {
			return nil, err
		}
		h.CreatedAt, _ = time.Parse(timeFormat, created)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (b *Backend) Status(ctx context.Context) (recall.Status, error) {
	st := recall.Status{Backend: "sqlitefts", Counts: map[string]int{}}
	rows, err := b.db.QueryContext(ctx, `SELECT kind, count(*) FROM recall_fts GROUP BY kind`)
	if err != nil {
		return st, fmt.Errorf("recall status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return st, err
		}
		st.Counts[kind] = n
	}
	return st, rows.Err()
}
