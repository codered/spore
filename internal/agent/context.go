package agent

import (
	"fmt"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/skill"
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
	// Skills is the index of loadable skills: names and descriptions only.
	// Bodies enter the prompt as skill_load tool results, never here.
	Skills   []skill.Skill
	Summary  string
	Messages []provider.Message
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

// Breakdown is the assembled size, part by part. /context reports it, and
// SnapshotTokens is defined as its total, so the number that drives
// compaction and the number the user is shown can never drift.
type Breakdown struct {
	System      int
	Environment int
	Facts       int
	Skills      int
	Summary     int
	Messages    int
}

func (b Breakdown) Total() int {
	return b.System + b.Environment + b.Facts + b.Skills + b.Summary + b.Messages
}

// SnapshotBreakdown builds a Breakdown using the same rendering functions as
// Assemble, so the estimate is synchronized with the actual output.
func SnapshotBreakdown(snap Snapshot, cfg config.ContextConfig) Breakdown {
	msgTotal := 0
	for _, m := range snap.Messages {
		msgTotal += messageTokens(m)
	}
	return Breakdown{
		System:      EstimateTokens(snap.System),
		Environment: EstimateTokens(snap.Environment),
		Facts:       EstimateTokens(factsSection(snap.Facts, cfg.FactBudget)),
		Skills:      EstimateTokens(skillsSection(snap.Skills, cfg.SkillBudget)),
		Summary:     EstimateTokens(snap.Summary),
		Messages:    msgTotal,
	}
}

// SnapshotTokens estimates the assembled size of a snapshot.
// It is defined as SnapshotBreakdown(...).Total(), so compaction and UI
// agree on the number that matters.
func SnapshotTokens(snap Snapshot, cfg config.ContextConfig) int {
	return SnapshotBreakdown(snap, cfg).Total()
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

// skillsSection renders the index. Only names and descriptions: a body is
// pulled in with skill_load, which keeps the cost of an unused skill at one
// line and makes loading visible in the transcript.
func skillsSection(skills []skill.Skill, budget int) string {
	if len(skills) == 0 {
		return ""
	}
	var section strings.Builder
	section.WriteString("\n\n## Skills you can load\n")
	section.WriteString("\nCall skill_load with a name to read one in full before you act on it.\n\n")
	used, dropped := 0, 0
	for _, s := range skills {
		line := "- " + s.Name + ": " + s.Description + "\n"
		cost := EstimateTokens(line)
		if used+cost > budget {
			dropped++
			continue
		}
		used += cost
		section.WriteString(line)
	}
	if dropped > 0 {
		fmt.Fprintf(&section, "\n(%d more skills did not fit this budget.)\n", dropped)
	}
	return section.String()
}

// Assemble builds the request in the spec's fixed order: system prompt,
// environment, memory facts, skills, compaction summary, then the live message
// tail. Facts and the summary ride in the system block so they stay pinned
// regardless of message count. The assembled request includes every live
// message; compaction is responsible for keeping the live tail within the
// token budget.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys strings.Builder
	sys.WriteString(snap.System)
	sys.WriteString(snap.Environment)
	sys.WriteString(factsSection(snap.Facts, cfg.FactBudget))
	sys.WriteString(skillsSection(snap.Skills, cfg.SkillBudget))
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
