package main

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
)

func TestCompactSummaryDistinguishesAFoldFromANoOp(t *testing.T) {
	if got := compactSummary(daemon.CompactJSON{Folded: 0, Before: 900, After: 900}); !strings.Contains(got, "nothing") {
		t.Fatalf("a no-op must say so, got %q", got)
	}
	got := compactSummary(daemon.CompactJSON{Folded: 14, Before: 38_104, After: 9_002})
	for _, want := range []string{"14", "38.1k", "9.0k"} {
		if !strings.Contains(got, want) {
			t.Fatalf("summary %q is missing %q", got, want)
		}
	}
}

func TestFormatContextCountsOnlyMessagesAfterTheSummaryBoundary(t *testing.T) {
	got := formatContext(map[string]any{
		"summary_through": float64(2),
		"messages": []any{
			map[string]any{"seq": float64(1), "tokens_in": float64(100)},
			map[string]any{"seq": float64(2), "tokens_out": float64(200)},
			map[string]any{"seq": float64(3), "tokens_in": float64(7), "tokens_out": float64(5)},
		},
	})
	if !strings.Contains(got, "messages: 1") || !strings.Contains(got, "tokens: ~12") {
		t.Fatalf("context = %q, want one live message and 12 tokens", got)
	}
}

func TestFormatUsageReportsTheCacheShare(t *testing.T) {
	got := formatUsage(map[string]any{"messages": []any{
		map[string]any{"tokens_in": float64(100), "tokens_out": float64(10), "tokens_cache_read": float64(900), "cost_usd": 0.5},
	}}, true)
	for _, want := range []string{"turns: 1", "tokens in: 100", "cache: 900 read, 0 written (90% of input)", "cost: $0.5000"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage %q is missing %q", got, want)
		}
	}
}

func TestFormatSkillsShowsTheLoadedMarkerAndErrors(t *testing.T) {
	got := formatSkills(skillListJSON{
		Skills: []skillJSON{
			{Name: "alpha", Description: "the alpha skill", BodyTokens: 120, Loaded: true},
			{Name: "beta", Description: "the beta skill", BodyTokens: 80},
		},
		Errors: []string{`gamma: unknown frontmatter key "bad"`},
	})
	for _, want := range []string{"alpha", "120", "loaded", "gamma"} {
		if !strings.Contains(got, want) {
			t.Errorf("skills listing is missing %q:\n%s", want, got)
		}
	}
}
