package agent

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/skill"
)

func userMsg(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: text}}}
}

func TestAssembleOrdersSystemFactsSummaryThenTail(t *testing.T) {
	snap := Snapshot{
		System: "you are spore",
		Facts: []memory.Fact{
			{Name: "user-prefers-go", Description: "the user prefers Go", Type: "user", Body: "Uses Go for most projects."},
			{Name: "user-in-london", Description: "the user is in London", Type: "user", Body: "Based in London, UK."},
		},
		Summary:  "earlier: the user set up spore",
		Messages: []provider.Message{userMsg("first"), userMsg("second")},
	}
	// Use large budget to inline the facts, testing the inline rendering path.
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 10, FactBudget: 1000})

	if !strings.HasPrefix(systemText(req.System), "you are spore") {
		t.Errorf("System does not start with the system prompt: %q", systemText(req.System))
	}
	// With FactBudget large enough, facts are inlined and their body text appears.
	factsAt := strings.Index(systemText(req.System), "Uses Go for most projects.")
	summaryAt := strings.Index(systemText(req.System), "earlier: the user set up spore")
	if factsAt < 0 || summaryAt < 0 || factsAt > summaryAt {
		t.Errorf("facts must precede the summary; system = %q", systemText(req.System))
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "first" {
		t.Errorf("message order changed: %+v", req.Messages)
	}
}

func TestAssembleIncludesEveryLiveMessage(t *testing.T) {
	var msgs []provider.Message
	for _, s := range []string{"m1", "m2", "m3", "m4", "m5"} {
		msgs = append(msgs, userMsg(s))
	}
	req := Assemble(Snapshot{System: "s", Messages: msgs}, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 2})
	if len(req.Messages) != 5 {
		t.Fatalf("Messages = %d, want 5; trimming is compaction's job, not assembly's", len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "m1" || req.Messages[4].Blocks[0].Text != "m5" {
		t.Errorf("message order wrong or not all messages present: %+v", req.Messages)
	}
}

func TestAssembleDoesNotAliasSnapshotMessages(t *testing.T) {
	snap := Snapshot{
		System:   "s",
		Messages: []provider.Message{userMsg("a"), userMsg("b")},
	}
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 10})

	if len(req.Messages) != len(snap.Messages) {
		t.Fatalf("Messages = %d, want %d", len(req.Messages), len(snap.Messages))
	}
	// The request must own its slice: mutating the snapshot afterwards must
	// not change a request already handed to a provider.
	if &req.Messages[0] == &snap.Messages[0] {
		t.Fatal("Assemble aliased the snapshot's backing array; the request must own its messages")
	}
	snap.Messages[0] = userMsg("MUTATED")
	if req.Messages[0].Blocks[0].Text != "a" {
		t.Errorf("mutating the snapshot changed the assembled request: got %q, want %q",
			req.Messages[0].Blocks[0].Text, "a")
	}
}

func TestAssembleDoesNotDropHistoryWhenUnderBudget(t *testing.T) {
	// Build a snapshot with 20 short messages and an empty summary
	// (simulating no compaction yet). With the default KeepRecent of 12,
	// the old buggy code would drop the first 8 messages even though
	// compaction hasn't run. This test catches that regression.
	var msgs []provider.Message
	for i := 0; i < 20; i++ {
		msgs = append(msgs, userMsg("FIRST"))
	}
	// Use default config, which has KeepRecent: 12
	cfg := config.Default().Context
	snap := Snapshot{System: "s", Summary: "", Messages: msgs}
	req := Assemble(snap, cfg)

	if len(req.Messages) != 20 {
		t.Fatalf("Messages = %d, want 20 (all messages should be present when under budget)",
			len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "FIRST" {
		t.Errorf("first message was dropped: got %q, want FIRST",
			req.Messages[0].Blocks[0].Text)
	}
}

func TestEstimateTokensGrowsWithLength(t *testing.T) {
	short := EstimateTokens("hello")
	long := EstimateTokens(strings.Repeat("hello ", 100))
	if short < 1 || long <= short {
		t.Errorf("EstimateTokens: short=%d long=%d", short, long)
	}
}

func fact(name, desc, body string) memory.Fact {
	return memory.Fact{Name: name, Description: desc, Type: "user", Body: body}
}

func TestAssembleInlinesFactsUnderBudget(t *testing.T) {
	snap := Snapshot{
		System: "sys",
		Facts:  []memory.Fact{fact("alpha", "first", "Alpha body."), fact("beta", "second", "Beta body.")},
	}
	req := Assemble(snap, config.ContextConfig{FactBudget: 1000})
	for _, want := range []string{"### alpha", "Alpha body.", "### beta", "Beta body."} {
		if !strings.Contains(systemText(req.System), want) {
			t.Fatalf("system block missing %q:\n%s", want, systemText(req.System))
		}
	}
	if strings.Contains(systemText(req.System), "recall_search") {
		t.Fatalf("no overflow expected, but the overflow heading is present:\n%s", systemText(req.System))
	}
}

func TestAssembleOverflowsToDescriptions(t *testing.T) {
	big := strings.Repeat("x ", 2000) // ~1000 tokens
	snap := Snapshot{
		System:  "sys",
		Facts:   []memory.Fact{fact("aaa", "small one", "tiny"), fact("zzz", "the big one", big)},
		Summary: "earlier events",
	}
	req := Assemble(snap, config.ContextConfig{FactBudget: 100})
	if !strings.Contains(systemText(req.System), "tiny") {
		t.Fatalf("the fact that fits was not inlined:\n%s", systemText(req.System))
	}
	if strings.Contains(systemText(req.System), big) {
		t.Fatal("the oversized fact body was inlined despite the budget")
	}
	if !strings.Contains(systemText(req.System), "- zzz: the big one") {
		t.Fatalf("overflow fact missing its description line:\n%s", systemText(req.System))
	}
	if !strings.Contains(systemText(req.System), "recall_search") {
		t.Fatalf("overflow section must tell the model how to retrieve a body:\n%s", systemText(req.System))
	}
	// Verify overflow facts precede the summary, just as inlined facts do.
	overflowAt := strings.Index(systemText(req.System), "- zzz: the big one")
	summaryAt := strings.Index(systemText(req.System), "earlier events")
	if overflowAt < 0 || summaryAt < 0 || overflowAt > summaryAt {
		t.Errorf("overflow facts must precede the summary; system = %q", systemText(req.System))
	}
}

// A fact too large to inline must not evict the smaller facts that follow it.
func TestAssembleKeepsInliningAfterAnOverflow(t *testing.T) {
	big := strings.Repeat("x ", 2000)
	snap := Snapshot{Facts: []memory.Fact{
		fact("aaa", "d", "first small"),
		fact("mmm", "d", big),
		fact("zzz", "d", "last small"),
	}}
	req := Assemble(snap, config.ContextConfig{FactBudget: 100})
	if !strings.Contains(systemText(req.System), "last small") {
		t.Fatalf("a later small fact was dropped by an earlier oversized one:\n%s", systemText(req.System))
	}
}

func TestAssembleZeroBudgetSendsEverythingToOverflow(t *testing.T) {
	snap := Snapshot{Facts: []memory.Fact{fact("aaa", "described", "body text")}}
	req := Assemble(snap, config.ContextConfig{FactBudget: 0})
	if strings.Contains(systemText(req.System), "body text") {
		t.Fatal("a zero budget inlined a body")
	}
	if !strings.Contains(systemText(req.System), "- aaa: described") {
		t.Fatal("a zero budget dropped the fact entirely instead of listing it")
	}
}

// The system block is the prompt-cache prefix. Two assemblies of the same
// snapshot must be byte-identical to allow prompt caching. This test proves
// the absence of call-parity nondeterminism. Name ordering is guaranteed by
// memory.Load sorting the facts (see internal/memory), not by this test.
func TestAssembleIsByteStableAcrossCalls(t *testing.T) {
	snap := Snapshot{System: "sys", Facts: []memory.Fact{
		fact("beta", "b", "B"), fact("alpha", "a", "A"), fact("gamma", "g", "G"),
	}}
	cfg := config.ContextConfig{FactBudget: 1000}
	a := systemText(Assemble(snap, cfg).System)
	b := systemText(Assemble(snap, cfg).System)
	if a != b {
		t.Fatalf("system block not stable:\n%q\n%q", a, b)
	}
}

func TestAssembleNoFactsNoSection(t *testing.T) {
	req := Assemble(Snapshot{System: "sys"}, config.ContextConfig{FactBudget: 1000})
	if systemText(req.System) != "sys" {
		t.Fatalf("empty fact set added a section: %q", systemText(req.System))
	}
}

// SnapshotTokens must estimate using the same rendering code as Assemble,
// so the estimate stays synchronized with the actual output. This test fails
// if the two diverge.
func TestSnapshotTokensMatchesAssembleOutput(t *testing.T) {
	big := strings.Repeat("x ", 2000) // ~1000 tokens
	snap := Snapshot{
		System:   "system text",
		Facts:    []memory.Fact{fact("small", "s", "tiny"), fact("large", "l", big)},
		Summary:  "summary text",
		Messages: []provider.Message{userMsg("m1"), userMsg("m2")},
	}
	cfg := config.ContextConfig{FactBudget: 100}
	estimate := SnapshotTokens(snap, cfg)
	actual := EstimateTokens(systemText(Assemble(snap, cfg).System))
	// Allow small margin for rounding, but they should be close.
	if estimate < actual-10 || estimate > actual+10 {
		t.Errorf("SnapshotTokens estimate diverged from actual: estimate=%d, actual=%d", estimate, actual)
	}
}

func TestAssemblePlacesEnvironmentAfterTheSystemPrompt(t *testing.T) {
	req := Assemble(Snapshot{
		System:      "you are spore",
		Environment: "\n\n## Environment\n\nWorking directory: /w\n",
		Facts:       []memory.Fact{{Name: "n", Body: "b"}},
		Messages:    []provider.Message{userMsg("hello")},
	}, config.ContextConfig{FactBudget: 1000})

	sysIdx := strings.Index(systemText(req.System), "you are spore")
	factIdx := strings.Index(systemText(req.System), "What you know about the user")
	if sysIdx < 0 || factIdx < 0 {
		t.Fatalf("missing a section in system block:\n%s", systemText(req.System))
	}
	if sysIdx >= factIdx {
		t.Errorf("wrong section order (system %d, facts %d):\n%s", sysIdx, factIdx, systemText(req.System))
	}
	// Environment is now at the tail of the final message, not in the system block.
	envIdx := -1
	for _, blk := range req.Messages[len(req.Messages)-1].Blocks {
		if strings.Contains(blk.Text, "Working directory: /w") {
			envIdx = strings.Index(blk.Text, "Working directory: /w")
			break
		}
	}
	if envIdx < 0 {
		t.Fatalf("environment not found in message blocks")
	}
}

func TestSnapshotTokensCountsEnvironment(t *testing.T) {
	cfg := config.ContextConfig{FactBudget: 1000}
	bare := SnapshotTokens(Snapshot{System: "s"}, cfg)
	withEnv := SnapshotTokens(Snapshot{System: "s", Environment: strings.Repeat("x", 400)}, cfg)
	if withEnv <= bare {
		t.Errorf("environment not counted: %d <= %d", withEnv, bare)
	}
}

func TestSkillsSectionListsNamesAndDescriptions(t *testing.T) {
	snap := Snapshot{Skills: []skill.Skill{
		{Name: "release-checklist", Description: "How to cut a release", Body: "long body here"},
	}}
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, SkillBudget: 500})
	if !strings.Contains(systemText(req.System), "release-checklist") ||
		!strings.Contains(systemText(req.System), "How to cut a release") {
		t.Fatalf("the skills index must carry name and description:\n%s", systemText(req.System))
	}
	if strings.Contains(systemText(req.System), "long body here") {
		t.Fatal("a skill body must never be assembled; it arrives as a skill_load result")
	}
	if !strings.Contains(systemText(req.System), "skill_load") {
		t.Fatal("the index must tell the model how to load a skill")
	}
}

func TestSkillsSectionOverflowStatesTheCount(t *testing.T) {
	var many []skill.Skill
	for i := 0; i < 50; i++ {
		many = append(many, skill.Skill{
			Name:        fmt.Sprintf("skill-%02d", i),
			Description: strings.Repeat("x", 100),
		})
	}
	req := Assemble(Snapshot{Skills: many}, config.ContextConfig{MaxTokens: 1000, SkillBudget: 200})
	if !strings.Contains(systemText(req.System), "more skills did not fit") {
		t.Fatalf("overflow must be stated:\n%s", systemText(req.System))
	}
	if strings.Contains(systemText(req.System), "skill-49") {
		t.Fatal("the budget must actually drop skills")
	}
}

func TestSkillsSectionEmptyWhenNoSkills(t *testing.T) {
	req := Assemble(Snapshot{}, config.ContextConfig{MaxTokens: 1000, SkillBudget: 500})
	if strings.Contains(systemText(req.System), "Skills") {
		t.Fatalf("no skills means no section:\n%s", systemText(req.System))
	}
}

func TestBreakdownSumsToSnapshotTokens(t *testing.T) {
	cfg := config.ContextConfig{MaxTokens: 10000, FactBudget: 200, SkillBudget: 200}
	snap := Snapshot{
		System:      "you are spore",
		Environment: "cwd: /tmp",
		Summary:     "we talked about go",
		Facts:       []memory.Fact{{Name: "prefers-tabs", Description: "d", Type: "user", Body: "tabs"}},
		Skills:      []skill.Skill{{Name: "release-checklist", Description: "How to cut a release", Body: "b"}},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hello"}}},
		},
	}
	b := SnapshotBreakdown(snap, cfg)
	if b.Total() != SnapshotTokens(snap, cfg) {
		t.Fatalf("the breakdown must sum to the total: %d vs %d", b.Total(), SnapshotTokens(snap, cfg))
	}
	if b.System == 0 || b.Environment == 0 || b.Facts == 0 || b.Skills == 0 || b.Summary == 0 || b.Messages == 0 {
		t.Fatalf("every part with content must be counted: %+v", b)
	}
}

// renderPrefix is the bytes the provider would cache: every system block, then
// every message block, stopping after the last block marked CacheBreak. Two
// requests whose renderPrefix output matches share a cache entry.
func renderPrefix(req provider.Request) string {
	var b strings.Builder
	for _, blk := range req.System {
		b.WriteString(blk.Text)
		if blk.CacheBreak {
			b.WriteString("|#|")
		}
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			b.WriteString(blk.Text)
			if blk.CacheBreak {
				return b.String()
			}
		}
	}
	return b.String()
}

func countBreaks(req provider.Request) int {
	n := 0
	for _, blk := range req.System {
		if blk.CacheBreak {
			n++
		}
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			if blk.CacheBreak {
				n++
			}
		}
	}
	return n
}

func snapWithEnv(env string) Snapshot {
	return Snapshot{
		System:      "SYSTEM",
		Environment: env,
		Summary:     "SUMMARY",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hello"}}},
		},
	}
}

// The property the whole design rests on: the environment changes every turn
// and must change nothing the provider reads from cache.
func TestAssembleKeepsThePrefixStableAcrossEnvironments(t *testing.T) {
	a := Assemble(snapWithEnv("files: one.go"), config.ContextConfig{})
	b := Assemble(snapWithEnv("files: one.go two.go three.go"), config.ContextConfig{})

	if renderPrefix(a) != renderPrefix(b) {
		t.Fatalf("the cached prefix moved when the environment changed:\n a: %q\n b: %q",
			renderPrefix(a), renderPrefix(b))
	}
}

func TestAssembleEmitsTwoBreakpoints(t *testing.T) {
	req := Assemble(snapWithEnv("files: one.go"), config.ContextConfig{})
	if n := countBreaks(req); n != 2 {
		t.Fatalf("got %d breakpoints, want 2 (system prefix and message tail)", n)
	}
}

// The environment rides at the very end, after the moving breakpoint, so it is
// outside every cache entry.
func TestAssemblePutsEnvironmentLastAndUncached(t *testing.T) {
	req := Assemble(snapWithEnv("ENVIRONMENT"), config.ContextConfig{})

	for _, blk := range req.System {
		if strings.Contains(blk.Text, "ENVIRONMENT") {
			t.Fatal("the environment is still in the system block")
		}
	}
	last := req.Messages[len(req.Messages)-1]
	tail := last.Blocks[len(last.Blocks)-1]
	if tail.Text != "ENVIRONMENT" {
		t.Fatalf("last block is %q, want the environment", tail.Text)
	}
	if tail.CacheBreak {
		t.Fatal("the environment block is marked as a breakpoint; it changes every turn")
	}
	if !last.Blocks[len(last.Blocks)-2].CacheBreak {
		t.Fatal("the breakpoint does not sit immediately before the environment")
	}
}

// Assemble is documented as a pure function. copy() duplicates the Message
// values but not their Blocks arrays, so a careless append writes through into
// the caller's snapshot -- and from there into whatever the store hands out
// next.
func TestAssembleDoesNotMutateTheSnapshot(t *testing.T) {
	snap := snapWithEnv("ENVIRONMENT")
	before := len(snap.Messages[0].Blocks)

	Assemble(snap, config.ContextConfig{})

	if got := len(snap.Messages[0].Blocks); got != before {
		t.Fatalf("the snapshot's blocks grew from %d to %d", before, got)
	}
	if snap.Messages[0].Blocks[0].CacheBreak {
		t.Fatal("Assemble marked a breakpoint on the caller's snapshot")
	}
}

func TestSelfSectionNamesWhereEverythingLives(t *testing.T) {
	// A model asked "where do skills go?" must answer from the prompt. With
	// nothing in it naming the paths, it greps config.toml and the database
	// instead and gets nonsense.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	cfg.Path = "/home/u/.spore/config.toml"

	got := selfSection(cfg, "/home/u/work")
	for _, want := range []string{
		"/home/u/.spore/skills",
		"/home/u/.spore/memory",
		"/home/u/.spore/config.toml",
		"SKILL.md",
		"skill_install",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("self section does not mention %q:\n%s", want, got)
		}
	}
}

func TestSelfSectionFollowsWorkspaceSkillScope(t *testing.T) {
	// Under workspace scope the skills directory moves per session. Naming
	// the global one would send the user to a directory spore does not read.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	cfg.Skills.Scope = config.SkillsWorkspace

	got := selfSection(cfg, "/home/u/work")
	if !strings.Contains(got, "/home/u/work/.spore/skills") {
		t.Fatalf("workspace-scoped skills dir not named:\n%s", got)
	}
	if strings.Contains(got, "/home/u/.spore/skills") {
		t.Fatalf("the global skills dir is named although scope is workspace:\n%s", got)
	}
}

func TestSelfSectionSaysWhenNoSkillsAreInstalled(t *testing.T) {
	// skillsSection renders nothing when the index is empty, so without this
	// the prompt is silent about skills exactly when the user is most likely
	// to be asking how to add one.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"

	snap := Snapshot{System: "You are spore.", Self: selfSection(cfg, "")}
	got := systemText(Assemble(snap, cfg.Context).System)
	if !strings.Contains(got, "/home/u/.spore/skills") {
		t.Fatalf("with no skills installed the prompt never names the skills directory:\n%s", got)
	}
}

func TestSelfSectionRidesInTheCachedPrefix(t *testing.T) {
	// The paths are stable for the life of a session. Putting them after the
	// moving breakpoint would re-send them every turn.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	snap := Snapshot{
		System:   "You are spore.",
		Self:     selfSection(cfg, ""),
		Messages: []provider.Message{{Role: "user", Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}}},
	}
	req := Assemble(snap, cfg.Context)

	var found bool
	for i, blk := range req.System {
		if strings.Contains(blk.Text, "/home/u/.spore/skills") {
			found = true
			if blk.CacheBreak && i != len(req.System)-1 {
				t.Fatal("the self section breaks the cache mid-prefix")
			}
		}
	}
	if !found {
		t.Fatalf("the self section is not a system block: %#v", req.System)
	}
}

func TestMemoryDirMatchesWhereFactsAreLoadedFrom(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	if got, want := cfg.MemoryDir(), filepath.Join("/home/u/.spore", "memory"); got != want {
		t.Fatalf("MemoryDir() = %q, want %q", got, want)
	}
}

func TestSelfSectionSaysWhatTheUserCanAskFor(t *testing.T) {
	// Knowing the path is not knowing that the user may just ask. A model
	// with only the path answers "your skills go in ~/.spore/skills" when
	// the user wanted a skill written.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"

	got := selfSection(cfg, "")
	for _, want := range []string{"ask you", "skill_install", "memory"} {
		if !strings.Contains(got, want) {
			t.Fatalf("self section never mentions %q:\n%s", want, got)
		}
	}
}

func TestSelfSectionExplainsInstallingFromALocation(t *testing.T) {
	// skill_install takes a body, not a path or a URL, so "install the skill
	// at <url>" is a two-step the model has to know about. And a path outside
	// the workspace is in baselineDeny, which no approval can talk past: the
	// useful answer there is what the user can do instead, not a refusal.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"

	got := selfSection(cfg, "")
	for _, want := range []string{"web_fetch", "fs_read", "outside the workspace"} {
		if !strings.Contains(got, want) {
			t.Fatalf("self section never mentions %q:\n%s", want, got)
		}
	}
}

func TestAssembleRendersSoulAndAgent(t *testing.T) {
	snap := Snapshot{
		System: "You are spore.",
		Soul:   "Be blunt with me.",
		Agent:  "Always run make lint before pushing.",
	}
	got := systemText(Assemble(snap, config.Default().Context).System)
	for _, want := range []string{"Who you are", "Be blunt with me.", "Working in this project", "Always run make lint before pushing."} {
		if !strings.Contains(got, want) {
			t.Fatalf("assembled prompt is missing %q:\n%s", want, got)
		}
	}
}

func TestSoulSitsUnderTheSystemPromptAndAgentSitsLast(t *testing.T) {
	// soul.md is identity and belongs with identity. agent.md is the most
	// situational thing in the prefix, so it sits closest to the
	// conversation it governs.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	snap := Snapshot{
		System: "SYSTEM-MARKER",
		Soul:   "SOUL-MARKER",
		Self:   selfSection(cfg, ""),
		Facts:  []memory.Fact{{Name: "f", Description: "d", Body: "FACT-MARKER"}},
		Agent:  "AGENT-MARKER",
	}
	got := systemText(Assemble(snap, cfg.Context).System)

	order := []string{"SYSTEM-MARKER", "SOUL-MARKER", "/home/u/.spore/skills", "FACT-MARKER", "AGENT-MARKER"}
	last := -1
	for _, m := range order {
		i := strings.Index(got, m)
		if i < 0 {
			t.Fatalf("%q missing from the prompt:\n%s", m, got)
		}
		if i < last {
			t.Fatalf("%q is out of order in the prefix:\n%s", m, got)
		}
		last = i
	}
}

func TestAbsentSoulAndAgentRenderNothing(t *testing.T) {
	// A machine with neither file must produce exactly today's prompt.
	got := systemText(Assemble(Snapshot{System: "You are spore."}, config.Default().Context).System)
	for _, unwanted := range []string{"Who you are", "Working in this project"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("an absent file still rendered %q:\n%s", unwanted, got)
		}
	}
}

func TestSoulAndAgentRideInTheCachedPrefix(t *testing.T) {
	snap := Snapshot{
		System:   "You are spore.",
		Soul:     "SOUL-MARKER",
		Agent:    "AGENT-MARKER",
		Messages: []provider.Message{{Role: "user", Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}}},
	}
	req := Assemble(snap, config.Default().Context)
	for _, m := range []string{"SOUL-MARKER", "AGENT-MARKER"} {
		if !strings.Contains(systemText(req.System), m) {
			t.Fatalf("%q is not a system block, so it is outside the cached prefix", m)
		}
	}
}

func TestSelfSectionNamesTheSoulAndAgentFiles(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	got := selfSection(cfg, "/home/u/work")
	for _, want := range []string{"/home/u/.spore/soul.md", "/home/u/work/.spore/agent.md", "agent_note"} {
		if !strings.Contains(got, want) {
			t.Fatalf("self section does not mention %q:\n%s", want, got)
		}
	}
}

func TestSelfSectionSaysSoulIsNotSporesToWrite(t *testing.T) {
	// soul.md has no tool. Without being told, the model either attempts a
	// write nothing offers or refuses a request it could have satisfied by
	// pointing at the path.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	got := selfSection(cfg, "/home/u/work")
	if !strings.Contains(got, "cannot write it") {
		t.Fatalf("self section does not say soul.md is the user's to edit:\n%s", got)
	}
}

func TestSelfSectionOmitsAgentPathWithoutAWorkspace(t *testing.T) {
	// A rootless session has no agent.md, and naming a path that does not
	// exist invites the model to describe one that can never be written.
	cfg := config.Default()
	cfg.DataDir = "/home/u/.spore"
	if got := selfSection(cfg, ""); strings.Contains(got, "agent.md") {
		t.Fatalf("a rootless session was told about agent.md:\n%s", got)
	}
}

func TestSoulSectionDoesNotDoubleTheHeading(t *testing.T) {
	// A user writing a markdown file naturally titles it. Prepending our own
	// heading then stacks two of them, and a file that guessed the same
	// title stacks the same words twice.
	body := "## Who you are\n\nYou're the friend who knows a lot of things.\n"
	got := soulSection(body)
	if n := strings.Count(got, "## Who you are"); n != 1 {
		t.Fatalf("heading appears %d times, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "friend who knows") {
		t.Fatalf("body was lost:\n%s", got)
	}
}

func TestSoulSectionUsesTheFilesOwnTitleWhateverItSays(t *testing.T) {
	// The rule is "a file that titles itself keeps its title", not "a file
	// that happens to match ours". Any leading heading counts.
	got := soulSection("# My rules\n\nBe blunt.\n")
	if strings.Contains(got, "## Who you are") {
		t.Fatalf("our heading was added above the file's own:\n%s", got)
	}
	if !strings.Contains(got, "# My rules") {
		t.Fatalf("the file's own title was lost:\n%s", got)
	}
}

func TestSoulSectionStillTitlesAnUntitledFile(t *testing.T) {
	got := soulSection("Be blunt. Skip the preamble.\n")
	if !strings.Contains(got, "## Who you are") {
		t.Fatalf("an untitled file got no heading, so the block is unlabelled:\n%s", got)
	}
}

func TestAgentSectionDoesNotDoubleTheHeading(t *testing.T) {
	got := agentSection("## Working in this project\n\n- Always run make lint.\n")
	if n := strings.Count(got, "## Working in this project"); n != 1 {
		t.Fatalf("heading appears %d times, want 1:\n%s", n, got)
	}
	if !strings.Contains(got, "make lint") {
		t.Fatalf("body was lost:\n%s", got)
	}
}

func TestAgentSectionStillTitlesAnUntitledFile(t *testing.T) {
	got := agentSection("- Always run make lint.\n")
	if !strings.Contains(got, "## Working in this project") {
		t.Fatalf("an untitled file got no heading:\n%s", got)
	}
}
