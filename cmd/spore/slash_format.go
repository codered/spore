package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/daemon"
)

func compactSummary(res daemon.CompactJSON) string {
	if res.Folded == 0 {
		return "nothing outside the protected recent window to fold"
	}
	return fmt.Sprintf("compacted: folded %d messages, ~%s → ~%s tokens",
		res.Folded, humanTokens(res.Before), humanTokens(res.After))
}

// humanTokens keeps an estimate short: 38k reads better than 38104, and the
// estimate is crude enough that the digits are noise.
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// formatAgents renders the /agents listing. It is shared by the TUI and the
// plain loop so both surfaces say the same thing.
func formatAgents(list agentListJSON) string {
	if len(list.Agents) == 0 {
		return "  no sub-agents in this session\n"
	}
	var b strings.Builder
	for _, a := range list.Agents {
		elapsed := time.Since(a.Started).Round(time.Second)
		if !a.Ended.IsZero() {
			elapsed = a.Ended.Sub(a.Started).Round(time.Second)
		}
		fmt.Fprintf(&b, "  %s  %-11s  %6s  $%.4f  %s\n",
			a.ID, a.State, elapsed, a.CostUSD, a.Prompt)
	}
	return b.String()
}

// formatSkills is shared by the Bubble Tea and plain chat loops.
func formatSkills(list skillListJSON) string {
	var b strings.Builder
	b.WriteString("  skills\n")
	if len(list.Skills) == 0 && len(list.Errors) == 0 {
		b.WriteString("    none\n")
	}
	for _, sk := range list.Skills {
		marker := "  "
		if sk.Loaded {
			marker = "· "
		}
		fmt.Fprintf(&b, "  %s%s — %s (~%d tokens%s)\n", marker, sk.Name, sk.Description, sk.BodyTokens, loadedSuffix(sk.Loaded))
	}
	for _, e := range list.Errors {
		fmt.Fprintf(&b, "  ! %s\n", e)
	}
	return b.String()
}

// loadedSuffix marks a skill already pulled into the transcript.
func loadedSuffix(loaded bool) string {
	if loaded {
		return ", loaded"
	}
	return ""
}

// castInt safely converts an any to int, returning 0 on failure.
func castInt(v any) int {
	if i, ok := v.(int); ok {
		return i
	}
	if f, ok := v.(float64); ok {
		return int(f)
	}
	return 0
}

// castFloat safely converts an any to float64, returning 0.0 on failure.
func castFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	if i, ok := v.(int); ok {
		return float64(i)
	}
	return 0.0
}

// formatContext is the /context report: the live messages after the summary
// boundary and a rough token count for them.
func formatContext(data map[string]any) string {
	through := castInt(data["summary_through"])
	total, count := 0, 0
	if arr, ok := data["messages"].([]any); ok {
		for _, raw := range arr {
			msg, ok := raw.(map[string]any)
			if !ok || castInt(msg["seq"]) <= through {
				continue
			}
			total += castInt(msg["tokens_in"]) + castInt(msg["tokens_out"])
			count++
		}
	}
	return fmt.Sprintf("context snapshot\n  messages: %d\n  tokens: ~%d", count, total)
}

// formatUsage is the /usage report: token and cost totals for the session.
func formatUsage(data map[string]any, showCost bool) string {
	var in, out, cacheRead, cacheWrite, turns int
	var cost float64
	if arr, ok := data["messages"].([]any); ok {
		for _, raw := range arr {
			msg, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			in += castInt(msg["tokens_in"])
			out += castInt(msg["tokens_out"])
			cacheRead += castInt(msg["tokens_cache_read"])
			cacheWrite += castInt(msg["tokens_cache_write"])
			cost += castFloat(msg["cost_usd"])
			turns++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "usage\n  turns: %d\n  tokens in: %d\n  tokens out: %d", turns, in, out)
	if cacheRead+cacheWrite > 0 {
		share := cacheRead * 100 / (in + cacheRead + cacheWrite)
		fmt.Fprintf(&b, "\n  cache: %d read, %d written (%d%% of input)", cacheRead, cacheWrite, share)
	}
	if showCost {
		fmt.Fprintf(&b, "\n  cost: $%.4f", cost)
	}
	return b.String()
}
