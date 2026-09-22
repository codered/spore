package agent

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

// Two turns of the same session must send a byte-identical prefix, or every
// request after the first pays full price and nothing announces it. This is
// the guard against a later change -- a timestamp in the system prompt, a tool
// list that stopped being sorted -- quietly turning caching off.
func TestTwoTurnsShareTheSamePrefix(t *testing.T) {
	first := Assemble(Snapshot{
		System:      "SYSTEM",
		Environment: "files: one.go",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "one"}}},
		},
	}, config.ContextConfig{})

	second := Assemble(Snapshot{
		System:      "SYSTEM",
		Environment: "files: one.go two.go", // the workspace changed
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "one"}}},
			{Role: provider.RoleAssistant, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}},
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "two"}}},
		},
	}, config.ContextConfig{})

	want := renderPrefix(first)
	got := renderPrefix(second)
	if !strings.HasPrefix(got, want) {
		t.Fatalf("turn 2 does not extend turn 1's prefix:\n turn 1: %q\n turn 2: %q", want, got)
	}
}
