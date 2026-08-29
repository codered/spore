package agent

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

func userMsg(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: text}}}
}

func TestAssembleOrdersSystemFactsSummaryThenTail(t *testing.T) {
	snap := Snapshot{
		System:   "you are spore",
		Facts:    []string{"the user prefers Go", "the user is in London"},
		Summary:  "earlier: the user set up spore",
		Messages: []provider.Message{userMsg("first"), userMsg("second")},
	}
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 10})

	if !strings.HasPrefix(req.System, "you are spore") {
		t.Errorf("System does not start with the system prompt: %q", req.System)
	}
	factsAt := strings.Index(req.System, "the user prefers Go")
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
