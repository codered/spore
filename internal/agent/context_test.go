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

func TestAssembleKeepsOnlyRecentMessages(t *testing.T) {
	var msgs []provider.Message
	for _, s := range []string{"m1", "m2", "m3", "m4", "m5"} {
		msgs = append(msgs, userMsg(s))
	}
	req := Assemble(Snapshot{System: "s", Messages: msgs}, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 2})
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "m4" || req.Messages[1].Blocks[0].Text != "m5" {
		t.Errorf("kept the wrong tail: %+v", req.Messages)
	}
}

func TestAssembleIsPure(t *testing.T) {
	snap := Snapshot{System: "s", Messages: []provider.Message{userMsg("a"), userMsg("b")}}
	before := len(snap.Messages)
	Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 1})
	if len(snap.Messages) != before {
		t.Errorf("Assemble mutated its input snapshot")
	}
}

func TestEstimateTokensGrowsWithLength(t *testing.T) {
	short := EstimateTokens("hello")
	long := EstimateTokens(strings.Repeat("hello ", 100))
	if short < 1 || long <= short {
		t.Errorf("EstimateTokens: short=%d long=%d", short, long)
	}
}
