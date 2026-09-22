package tui

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/store"
)

// usageRes shows the selected session's totals, then every session's per day.
type usageRes struct{}

func (usageRes) Name() string   { return "usage" }
func (usageRes) Hotkey() string { return "U" }
func (usageRes) Scoped() bool   { return true }
func (usageRes) Columns() []Column {
	return []Column{
		{Title: "DAY", Min: 12},
		{Title: "MODEL", Min: 12, Flex: true},
		{Title: "TURNS", Min: 5, Right: true},
		{Title: "IN", Min: 6, Right: true},
		{Title: "OUT", Min: 6, Right: true},
		{Title: "CACHE %", Min: 7, Right: true},
		{Title: "COST", Min: 9, Right: true},
	}
}

func (usageRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
	u, err := v.Usage(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(u.Session)+len(u.Days))
	for _, r := range u.Session {
		rows = append(rows, Row{ID: "session/" + r.Model, Cells: usageCells("this session", r), Data: r})
	}
	for _, d := range u.Days {
		rows = append(rows, Row{ID: "day/" + d.Day + "/" + d.Model, Cells: usageCells(d.Day, d.UsageRow), Data: d.UsageRow})
	}
	return rows, nil
}

func usageCells(day string, r store.UsageRow) []string {
	return []string{day, r.Model, fmt.Sprint(r.Turns), humanTokens(r.TokensIn), humanTokens(r.TokensOut),
		cachePct(r), fmt.Sprintf("$%.4f", r.CostUSD)}
}

// cachePct is the share of input served from the prompt cache. Since caching
// shipped TokensIn is only the uncached remainder, so the whole input is the
// sum of all three.
func cachePct(r store.UsageRow) string {
	total := r.TokensIn + r.TokensCacheRead + r.TokensCacheWrite
	if r.TokensCacheRead == 0 || total == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", r.TokensCacheRead*100/total)
}

func (usageRes) Detail(r Row) string {
	u, ok := r.Data.(store.UsageRow)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%s\n\nturns: %d\ntokens in (uncached): %d\ntokens out: %d\ncache read: %d\ncache write: %d\ncache hit: %s\ncost: $%.4f",
		u.Model, u.Turns, u.TokensIn, u.TokensOut, u.TokensCacheRead, u.TokensCacheWrite, cachePct(u), u.CostUSD)
}

func (usageRes) Actions() []Action { return nil }
