package provider

import (
	"math"
	"testing"
)

// Sub-agent budgets are enforced on recorded cost, so pricing reads at the
// full input rate would cut trees off on spend that never happened.
func TestCostBillsEachBucketAtItsOwnRate(t *testing.T) {
	p := ProviderPrice{In: 5, Out: 25, CacheWrite: 6.25, CacheRead: 0.5}
	u := Usage{InputTokens: 1e6, OutputTokens: 1e6, CacheWriteTokens: 1e6, CacheReadTokens: 1e6}

	if got, want := p.Cost(u), 36.75; math.Abs(got-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v (5 + 25 + 6.25 + 0.5)", got, want)
	}
}

// An operator who configured prices before caching existed still gets correct
// costs, with no edit.
func TestCostDefaultsTheCacheRatesFromPriceIn(t *testing.T) {
	p := ProviderPrice{In: 10, Out: 50}
	u := Usage{CacheWriteTokens: 1e6, CacheReadTokens: 1e6}

	if got, want := p.Cost(u), 13.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v (1.25x + 0.10x of price_in)", got, want)
	}
}
