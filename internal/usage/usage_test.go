package usage

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

func row(model string, turns, in, out, cw, cr int, cost float64) store.UsageRow {
	return store.UsageRow{Model: model, Turns: turns, TokensIn: in, TokensOut: out,
		TokensCacheWrite: cw, TokensCacheRead: cr, CostUSD: cost}
}

func TestSumsModelsAndAddsTheThirtyDayLine(t *testing.T) {
	session := []store.UsageRow{
		row("a", 10, 40000, 6000, 2000, 30000, 0.15),
		row("b", 2, 8210, 904, 400, 1000, 0.0334),
	}
	days := []store.DailyUsageRow{
		{Day: "2026-10-05", UsageRow: row("a", 400, 3000000, 270000, 0, 0, 9)},
		{Day: "2026-10-04", UsageRow: row("b", 12, 100000, 10000, 0, 0, 0.12)},
	}
	got := Session(session, true) + Total(days, true)
	want := "usage, this session\n" +
		"  turns: 12\n" +
		"  tokens in: 48,210  out: 6,904\n" +
		"  cache: 31,000 read, 2,400 written (38% of input)\n" +
		"  cost: $0.1834\n" +
		"last 30 days, all sessions: 412 turns, 3.1M in, 280k out, $9.12\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestHidesCostAndEmptyCache(t *testing.T) {
	got := Session([]store.UsageRow{row("a", 1, 900, 50, 0, 0, 0.5)}, false) + Total(nil, false)
	if strings.Contains(got, "cache") || strings.Contains(got, "$") {
		t.Fatalf("no cache line and no cost when hidden, got:\n%s", got)
	}
	if !strings.Contains(got, "last 30 days, all sessions: 0 turns, 0 in, 0 out\n") {
		t.Fatalf("the 30-day line is always there, got:\n%s", got)
	}
}

func TestTotalAbbreviates(t *testing.T) {
	got := Total([]store.DailyUsageRow{{Day: "2026-10-05", UsageRow: row("a", 3, 1500, 999, 0, 0, 0)}}, false)
	if got != "last 30 days, all sessions: 3 turns, 1.5k in, 999 out\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSessionWithNoTurnsYet(t *testing.T) {
	got := Session(nil, false)
	if !strings.HasPrefix(got, "usage, this session\n  turns: 0\n") {
		t.Fatalf("an empty session still reports itself, got %q", got)
	}
}
