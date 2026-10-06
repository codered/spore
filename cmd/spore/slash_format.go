package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/daemon"
	usagereport "github.com/codered/spore/internal/usage"
)

// compactSummary says what a compaction actually did. A session with nothing
// outside the protected recent window folds nothing, and reporting that as
// "compacted" would be a lie the operator acts on.
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

// usageReport is the /usage report: this session's totals, then every
// session's over the last 30 days, in the format every surface shares.
func usageReport(u daemon.UsageJSON, showCost bool) string {
	return usagereport.Session(u.Session, showCost) + usagereport.Total(u.Days, showCost)
}
