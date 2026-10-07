package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

// A local model that runs out of output tokens mid-call streams arguments
// that are not JSON. Before this guard, the first json.Marshal of the
// conversation failed and took the whole turn down with "unexpected end of
// JSON input", and the model never learned why. The call must instead come
// back as an error the model can act on, without reaching policy or a tool.
func TestInvalidToolArgumentsComeBackAsAnError(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{ToolCalls: []provider.Block{
			{Type: provider.BlockToolUse, ID: "cut", Name: "fs.read", Input: json.RawMessage(`{"path":"go.mod","code":"package main\n\nfunc ma`)},
			{Type: provider.BlockToolUse, ID: "bad", Name: "fs.read", Input: json.RawMessage(`{path: go.mod}`)},
			{Type: provider.BlockToolUse, ID: "ok", Name: "fs.read", Input: json.RawMessage(`{"path":"go.mod"}`)},
		}},
		provider.ScriptTurn{Text: "done"},
	)
	tools := &fakeTools{result: "module x"}
	a, st := harness(t, script, tools)
	sid, _ := st.CreateSession(ctx, "t", "")
	ch, err := a.Run(ctx, sid, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	results := map[string]provider.Block{}
	for _, ev := range collect(t, ch) {
		if ev.Type == EvError {
			t.Fatalf("the turn failed: %v", ev.Err)
		}
		if ev.Type == EvToolResult {
			results[ev.Block.ID] = *ev.Block
		}
	}

	if len(tools.calls) != 1 || tools.calls[0].ID != "ok" {
		t.Fatalf("dispatched %+v; only the valid call may run", tools.calls)
	}
	cut := results["cut"]
	if !cut.IsError || !strings.Contains(cut.Content, "not valid JSON") || !strings.Contains(cut.Content, "shorter") {
		t.Errorf("cut-off call result = %+v, want an error saying the arguments were cut off", cut)
	}
	bad := results["bad"]
	if !bad.IsError || !strings.Contains(bad.Content, "not valid JSON") || strings.Contains(bad.Content, "shorter") {
		t.Errorf("malformed call result = %+v, want an error that does not blame the output limit", bad)
	}

	// Stored history must replay: every tool_use input is valid JSON.
	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 4 {
		t.Fatalf("persisted %d messages, want 4", len(msgs))
	}
	for _, m := range msgs {
		var blocks []provider.Block
		if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
			t.Fatalf("stored blocks do not parse: %v", err)
		}
		for _, b := range blocks {
			if b.Type == provider.BlockToolUse && !json.Valid(b.Input) {
				t.Errorf("stored tool_use %s has invalid input %q", b.ID, b.Input)
			}
		}
	}
	if n := len(script.Requests()); n != 2 {
		t.Errorf("provider called %d times, want 2 (the model must see the errors and finish)", n)
	}
}
