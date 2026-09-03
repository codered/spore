package agent

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
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

	if !strings.HasPrefix(req.System, "you are spore") {
		t.Errorf("System does not start with the system prompt: %q", req.System)
	}
	// With FactBudget large enough, facts are inlined and their body text appears.
	factsAt := strings.Index(req.System, "Uses Go for most projects.")
	summaryAt := strings.Index(req.System, "earlier: the user set up spore")
	if factsAt < 0 || summaryAt < 0 || factsAt > summaryAt {
		t.Errorf("facts must precede the summary; system = %q", req.System)
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
		if !strings.Contains(req.System, want) {
			t.Fatalf("system block missing %q:\n%s", want, req.System)
		}
	}
	if strings.Contains(req.System, "recall_search") {
		t.Fatalf("no overflow expected, but the overflow heading is present:\n%s", req.System)
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
	if !strings.Contains(req.System, "tiny") {
		t.Fatalf("the fact that fits was not inlined:\n%s", req.System)
	}
	if strings.Contains(req.System, big) {
		t.Fatal("the oversized fact body was inlined despite the budget")
	}
	if !strings.Contains(req.System, "- zzz: the big one") {
		t.Fatalf("overflow fact missing its description line:\n%s", req.System)
	}
	if !strings.Contains(req.System, "recall_search") {
		t.Fatalf("overflow section must tell the model how to retrieve a body:\n%s", req.System)
	}
	// Verify overflow facts precede the summary, just as inlined facts do.
	overflowAt := strings.Index(req.System, "- zzz: the big one")
	summaryAt := strings.Index(req.System, "earlier events")
	if overflowAt < 0 || summaryAt < 0 || overflowAt > summaryAt {
		t.Errorf("overflow facts must precede the summary; system = %q", req.System)
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
	if !strings.Contains(req.System, "last small") {
		t.Fatalf("a later small fact was dropped by an earlier oversized one:\n%s", req.System)
	}
}

func TestAssembleZeroBudgetSendsEverythingToOverflow(t *testing.T) {
	snap := Snapshot{Facts: []memory.Fact{fact("aaa", "described", "body text")}}
	req := Assemble(snap, config.ContextConfig{FactBudget: 0})
	if strings.Contains(req.System, "body text") {
		t.Fatal("a zero budget inlined a body")
	}
	if !strings.Contains(req.System, "- aaa: described") {
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
	if a, b := Assemble(snap, cfg).System, Assemble(snap, cfg).System; a != b {
		t.Fatalf("system block not stable:\n%q\n%q", a, b)
	}
}

func TestAssembleNoFactsNoSection(t *testing.T) {
	req := Assemble(Snapshot{System: "sys"}, config.ContextConfig{FactBudget: 1000})
	if req.System != "sys" {
		t.Fatalf("empty fact set added a section: %q", req.System)
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
	actual := EstimateTokens(Assemble(snap, cfg).System)
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
	}, config.ContextConfig{FactBudget: 1000})

	sysIdx := strings.Index(req.System, "you are spore")
	envIdx := strings.Index(req.System, "Working directory: /w")
	factIdx := strings.Index(req.System, "What you know about the user")
	if sysIdx < 0 || envIdx < 0 || factIdx < 0 {
		t.Fatalf("missing a section in system block:\n%s", req.System)
	}
	if !(sysIdx < envIdx && envIdx < factIdx) {
		t.Errorf("wrong section order (system %d, env %d, facts %d):\n%s", sysIdx, envIdx, factIdx, req.System)
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
