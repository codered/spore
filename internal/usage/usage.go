// Package usage formats the /usage report, so the TUI, the plain chat loop
// and the Discord bridge print the same text from the same rows. The web UI
// mirrors it in JavaScript (usageReport in web/app.js).
package usage

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/store"
)

// Window is how far back the all-sessions total reaches. The daemon's
// GET /api/usage uses it too, so the line's "last 30 days" stays true.
const Window = 30 * 24 * time.Hour

// Session reports one session's totals, summed across the models it used.
// The cache line appears only when the session used the cache.
func Session(rows []store.UsageRow, showCost bool) string {
	t := sum(rows)
	var b strings.Builder
	fmt.Fprintf(&b, "usage, this session\n  turns: %d\n  tokens in: %s  out: %s\n",
		t.Turns, commas(t.TokensIn), commas(t.TokensOut))
	if t.TokensCacheRead+t.TokensCacheWrite > 0 {
		// TokensIn is the uncached remainder, so the cache's share of input
		// is measured against all three.
		all := t.TokensIn + t.TokensCacheRead + t.TokensCacheWrite
		share := (t.TokensCacheRead*100 + all/2) / all
		fmt.Fprintf(&b, "  cache: %s read, %s written (%d%% of input)\n",
			commas(t.TokensCacheRead), commas(t.TokensCacheWrite), share)
	}
	if showCost {
		fmt.Fprintf(&b, "  cost: $%.4f\n", t.CostUSD)
	}
	return b.String()
}

// Total is one line: every session's usage over the days given, which the
// daemon bounds to the last 30.
func Total(days []store.DailyUsageRow, showCost bool) string {
	rows := make([]store.UsageRow, len(days))
	for i, d := range days {
		rows[i] = d.UsageRow
	}
	t := sum(rows)
	line := fmt.Sprintf("last 30 days, all sessions: %d turns, %s in, %s out",
		t.Turns, short(t.TokensIn), short(t.TokensOut))
	if showCost {
		line += fmt.Sprintf(", $%.2f", t.CostUSD)
	}
	return line + "\n"
}

func sum(rows []store.UsageRow) store.UsageRow {
	var t store.UsageRow
	for _, r := range rows {
		t.Turns += r.Turns
		t.TokensIn += r.TokensIn
		t.TokensOut += r.TokensOut
		t.TokensCacheWrite += r.TokensCacheWrite
		t.TokensCacheRead += r.TokensCacheRead
		t.CostUSD += r.CostUSD
	}
	return t
}

// commas writes n with thousands separators: 48210 is 48,210.
func commas(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return "-" + commas(-n)
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// short abbreviates large counts for the one-line total: 280000 is 280k.
func short(n int) string {
	switch {
	case n >= 1_000_000:
		return trim(float64(n)/1e6) + "M"
	case n >= 1_000:
		return trim(float64(n)/1e3) + "k"
	}
	return strconv.Itoa(n)
}

// trim prints one decimal place and drops it when it is zero.
func trim(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}
