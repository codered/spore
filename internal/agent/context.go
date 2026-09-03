package agent

import (
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
)

// Snapshot is everything context assembly is allowed to see. Taking it as a
// value is what makes Assemble a pure function and therefore testable with no
// store and no network.
type Snapshot struct {
	System string
	// Environment describes the working directory and its files. It is
	// rebuilt per turn rather than stored, because it describes the machine
	// as it is now, not as it was when the session started.
	Environment string
	Facts       []memory.Fact
	Summary     string
	Messages    []provider.Message
}

// EstimateTokens approximates tokens as bytes/4. It is deliberately crude:
// it only has to be monotonic and roughly right to drive the compaction
// trigger, and it costs nothing.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/4 + 1
}

func messageTokens(m provider.Message) int {
	n := 4 // per-message overhead
	for _, b := range m.Blocks {
		n += EstimateTokens(b.Text) + EstimateTokens(string(b.Input)) + EstimateTokens(b.Content)
	}
	return n
}

// SnapshotTokens estimates the assembled size of a snapshot.
// It estimates the fact section using the same rendering code as Assemble
// to ensure the estimate stays synchronized with the actual output.
func SnapshotTokens(snap Snapshot, cfg config.ContextConfig) int {
	n := EstimateTokens(snap.System) + EstimateTokens(snap.Environment) + EstimateTokens(snap.Summary)
	n += EstimateTokens(factsSection(snap.Facts, cfg.FactBudget))
	for _, m := range snap.Messages {
		n += messageTokens(m)
	}
	return n
}

// factInlineCost estimates the token cost of a fact if it were inlined:
// the heading, name, and body only. Used to decide whether the inline form fits
// the budget, so it counts only what an inline fact actually emits.
func factInlineCost(f memory.Fact) int {
	return EstimateTokens(f.Name) + EstimateTokens(f.Body) + 4
}

// factsSection renders the "## What you know about the user" section,
// applying the budget to inline facts and overflowing those that don't fit.
// Returns the rendered section (without the leading fact heading if facts is empty).
// Both Assemble and SnapshotTokens call this to ensure the token estimate
// matches the rendered text.
func factsSection(facts []memory.Fact, budget int) string {
	if len(facts) == 0 {
		return ""
	}
	var section strings.Builder
	section.WriteString("\n\n## What you know about the user\n")
	var overflow []memory.Fact
	used := 0
	for _, f := range facts {
		// An oversized fact overflows on its own account and does not
		// evict the smaller facts after it, so one long file cannot empty
		// the section.
		cost := factInlineCost(f)
		if used+cost > budget {
			overflow = append(overflow, f)
			continue
		}
		used += cost
		section.WriteString("\n### ")
		section.WriteString(f.Name)
		section.WriteString("\n")
		section.WriteString(f.Body)
		section.WriteString("\n")
	}
	if len(overflow) > 0 {
		section.WriteString("\nThese facts did not fit. Retrieve one by name with recall_search:\n")
		for _, f := range overflow {
			section.WriteString("- ")
			section.WriteString(f.Name)
			section.WriteString(": ")
			section.WriteString(f.Description)
			section.WriteString("\n")
		}
	}
	return section.String()
}

// Assemble builds the request in the spec's fixed order: system prompt,
// environment, memory facts, compaction summary, then the live message tail. Facts and the
// summary ride in the system block so they stay pinned regardless of message
// count. The assembled request includes every live message; compaction is
// responsible for keeping the live tail within the token budget.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys strings.Builder
	sys.WriteString(snap.System)
	sys.WriteString(snap.Environment)
	sys.WriteString(factsSection(snap.Facts, cfg.FactBudget))
	if snap.Summary != "" {
		sys.WriteString("\n\n## Earlier in this conversation\n")
		sys.WriteString(snap.Summary)
		sys.WriteString("\n")
	}

	// Copy so callers cannot alias the snapshot's backing array.
	msgs := make([]provider.Message, len(snap.Messages))
	copy(msgs, snap.Messages)

	return provider.Request{
		System:    sys.String(),
		Messages:  msgs,
		MaxTokens: 4096,
	}
}
