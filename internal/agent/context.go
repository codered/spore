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
	Skills []skill.Skill
	// Self describes spore's own layout on this machine: where skills,
	// facts, the database and the config actually live. It is stable for
	// the life of a session, so it rides in the cached prefix.
	Self string
	// Soul is soul.md: personality, global, the user's alone. Agent is
	// agent.md: standing instructions for this one workspace. Neither is a
	// fact -- they carry no frontmatter and are not indexed -- and both are
	// empty when the file does not exist, which renders no section at all.
	Soul     string
	Agent    string
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

// selfSection tells the model where spore keeps its own files. Without it a
// question as ordinary as "where do skills go?" sends the model grepping
// config.toml and the SQLite database, because the skills index renders
// nothing at all when no skills are installed yet -- which is exactly when
// the user is most likely to ask.
//
// workspace is the session's root, which matters only under workspace skill
// scope, where the skills directory moves per session.
func selfSection(cfg *config.Config, workspace string) string {
	skills := cfg.SkillsDir(workspace)
	if skills == "" {
		// Workspace scope with no root of its own: there is no directory to
		// name, and naming the global one would be a lie.
		skills = "(this session has no workspace, so no skills directory)"
	}
	var b strings.Builder
	b.WriteString("\n\n## Where spore keeps its files on this machine\n\n")
	fmt.Fprintf(&b, "- Skills: %s -- one directory per skill, each holding a SKILL.md with a name and description in its frontmatter. Write one with skill_install, or the user can add the files by hand.\n", skills)
	fmt.Fprintf(&b, "- Memory facts: %s -- one markdown file per fact, written with the memory tool.\n", cfg.MemoryDir())
	fmt.Fprintf(&b, "- Your personality: %s -- how you speak and what you value. This one is the user's to edit; you cannot write it.\n", cfg.SoulPath())
	// A rootless session has no agent.md. Naming a path that cannot exist
	// would invite the model to describe writing one.
	if p := cfg.AgentPath(workspace); p != "" {
		fmt.Fprintf(&b, "- Standing instructions for this workspace: %s -- what the user has said to always or never do here, added with agent_note.\n", p)
	}
	fmt.Fprintf(&b, "- Database: %s -- sessions, messages and the recall index. It is a binary file: search it with recall_search, never by reading it.\n", cfg.DBPath())
	if cfg.Path != "" {
		fmt.Fprintf(&b, "- Config: %s\n", cfg.Path)
	}
	b.WriteString("\nAnswer questions about where things live from this list rather than searching the filesystem for them.\n")
	// Knowing the path is not knowing that the user may simply ask. Without
	// this the model answers "your skills go in <dir>" when what the user
	// wanted was a skill written for them.
	b.WriteString("\nThe user can ask you to do these things directly: \"write me a skill for X\" is skill_install, \"from now on in this project, always X\" is agent_note, and \"remember that X\" is memory. Each asks for their approval before it writes.\n")
	// The asymmetry is the point: three of these are things spore does on
	// request, and the fourth is a file it only reads.
	b.WriteString("\nsoul.md is the user's, not yours: you cannot write it. When they ask you to change how you behave in general rather than in one project, tell them the path and what to add, and let them make the edit.\n")
	// skill_install takes a body, not a location, so installing from a file
	// or a URL is a two-step the model has to be told about. The last
	// sentence is the load-bearing one: a path outside the workspace is in
	// baselineDeny, which no approval can talk past, so the only useful
	// answer is what the user can do instead of a refusal.
	b.WriteString("\nskill_install takes the skill's text, not a location. To install one from a file or a URL, read it first -- web_fetch for a URL, fs_read for a file in the workspace -- and pass what you read to skill_install. A file outside the workspace cannot be read at all, whoever approves it: say so and offer to install it if the user moves it into the workspace or starts a session rooted where it lives.\n")
	return b.String()
}

// soulSection renders soul.md. It is identity, so it goes with identity:
// directly under the system prompt, which is the operational half of the
// same thing.
func soulSection(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return "\n\n## Who you are\n\n" + strings.TrimRight(body, "\n") + "\n"
}

// agentSection renders agent.md. It goes last in the stable prefix: it is the
// most situational thing in it, so it sits closest to the conversation it
// governs.
func agentSection(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return "\n\n## Working in this project\n\n" + strings.TrimRight(body, "\n") + "\n"
}

// Assemble builds the request ordered by stability rather than by topic: the
// system prompt, the skills index, the memory facts and the compaction summary
// all change rarely, so they sit in front of a cache breakpoint. The
// environment section changes every turn and rides at the very end, after the
// moving breakpoint, where it invalidates nothing.
//
// The environment is never stored. It is injected here and here only, so the
// prefix one turn reads is byte-identical to what the turn before it wrote.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys []provider.Block
	add := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		sys = append(sys, provider.Block{Type: provider.BlockText, Text: text})
	}
	add(snap.System)
	add(soulSection(snap.Soul))
	add(snap.Self)
	add(skillsSection(snap.Skills, cfg.SkillBudget))
	add(factsSection(snap.Facts, cfg.FactBudget))
	add(agentSection(snap.Agent))
	if snap.Summary != "" {
		add("\n\n## Earlier in this conversation\n" + snap.Summary + "\n")
	}
	if len(sys) > 0 {
		sys[len(sys)-1].CacheBreak = true
	}

	// Copy so callers cannot alias the snapshot's backing array.
	msgs := make([]provider.Message, len(snap.Messages))
	copy(msgs, snap.Messages)
	msgs = markTailAndAppendEnvironment(msgs, snap.Environment)

	return provider.Request{System: sys, Messages: msgs, MaxTokens: 4096}
}

// markTailAndAppendEnvironment puts the moving breakpoint on the last block of
// the conversation and the environment after it.
//
// It copies the final message's blocks first. copy() above duplicates the
// Message values but not the slices inside them, so writing through
// msgs[last].Blocks would reach into the caller's snapshot -- and Assemble is
// documented as pure.
func markTailAndAppendEnvironment(msgs []provider.Message, env string) []provider.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := len(msgs) - 1
	blocks := make([]provider.Block, len(msgs[last].Blocks), len(msgs[last].Blocks)+1)
	copy(blocks, msgs[last].Blocks)
	if len(blocks) > 0 {
		blocks[len(blocks)-1].CacheBreak = true
	}
	if strings.TrimSpace(env) != "" {
		blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: env})
	}
	msgs[last].Blocks = blocks
	return msgs
}
