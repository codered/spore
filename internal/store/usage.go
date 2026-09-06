package store

import "context"

// UsageRow is one model's consumption. /usage reports these per session and
// across every session; the numbers come from the columns AppendMessage has
// always written.
type UsageRow struct {
	Model     string  `json:"model"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

const usageSelect = `SELECT model, count(*), sum(tokens_in), sum(tokens_out), sum(cost_usd)
  FROM messages WHERE tokens_in > 0 OR tokens_out > 0 OR cost_usd > 0`

func (s *Store) SessionUsage(ctx context.Context, sessionID string) ([]UsageRow, error) {
	return s.usage(ctx, usageSelect+` AND session_id = ? GROUP BY model ORDER BY model`, sessionID)
}

func (s *Store) TotalUsage(ctx context.Context) ([]UsageRow, error) {
	return s.usage(ctx, usageSelect+` GROUP BY model ORDER BY model`)
}

func (s *Store) usage(ctx context.Context, query string, args ...any) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Model, &r.Turns, &r.TokensIn, &r.TokensOut, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSeq is the newest message's sequence number, or 0 for a session with no
// messages. /clear moves the summary boundary here.
func (s *Store) LastSeq(ctx context.Context, sessionID string) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(max(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&seq)
	return seq, err
}
