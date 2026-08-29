package agent

import (
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

// Snapshot is everything context assembly is allowed to see. Taking it as a
// value is what makes Assemble a pure function and therefore testable with no
// store and no network.
type Snapshot struct {
	System   string
	Facts    []string
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

// SnapshotTokens estimates the assembled size of a snapshot.
func SnapshotTokens(snap Snapshot) int {
	n := EstimateTokens(snap.System) + EstimateTokens(snap.Summary)
	for _, f := range snap.Facts {
		n += EstimateTokens(f)
	}
	for _, m := range snap.Messages {
		n += messageTokens(m)
	}
	return n
}

// Assemble builds the request in the spec's fixed order: system prompt,
// memory facts, compaction summary, then the live message tail. Facts and the
// summary ride in the system block so they stay pinned regardless of message
// count. The assembled request includes every live message; compaction is
// responsible for keeping the live tail within the token budget.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys strings.Builder
	sys.WriteString(snap.System)
	if len(snap.Facts) > 0 {
		sys.WriteString("\n\n## What you know about the user\n")
		for _, f := range snap.Facts {
			sys.WriteString("- ")
			sys.WriteString(f)
			sys.WriteString("\n")
		}
	}
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
