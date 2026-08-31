package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/provider"
)

func TestFromAgentCoversEveryEventType(t *testing.T) {
	cases := []struct {
		name string
		in   agent.Event
		want string
	}{
		{"text", agent.Event{Type: agent.EvText, Text: "hello"}, WireText},
		{"tool call", agent.Event{Type: agent.EvToolCall, Block: &provider.Block{
			Type: provider.BlockToolUse, ID: "t1", Name: "fs_read", Input: json.RawMessage(`{"path":"a"}`),
		}}, WireToolCall},
		{"tool result", agent.Event{Type: agent.EvToolResult, Block: &provider.Block{
			Type: provider.BlockToolResult, ID: "t1", Content: "file body",
		}}, WireToolResult},
		{"turn done", agent.Event{Type: agent.EvTurnDone, Model: "anthropic/claude-opus-5",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 4}, Cost: 0.002}, WireTurnDone},
		{"error", agent.Event{Type: agent.EvError, Err: errors.New("provider exploded")}, WireError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FromAgent(tc.in)
			if got.Type != tc.want {
				t.Fatalf("type = %q, want %q", got.Type, tc.want)
			}
			// Everything on the wire must survive a JSON round trip: an
			// agent.Event carries an error value, which does not marshal.
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back WireEvent
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back != got {
				t.Errorf("round trip changed the event:\n got %+v\nwant %+v", back, got)
			}
		})
	}
}

func TestFromAgentKeepsTheErrorText(t *testing.T) {
	ev := FromAgent(agent.Event{Type: agent.EvError, Err: errors.New("provider exploded")})
	if ev.Error != "provider exploded" {
		t.Errorf("Error = %q, want the error's text", ev.Error)
	}
}

func TestFromAgentToleratesAMissingBlock(t *testing.T) {
	// A malformed event must not panic the broadcast goroutine and take the
	// whole daemon down with it.
	ev := FromAgent(agent.Event{Type: agent.EvToolCall})
	if ev.Type != WireToolCall || ev.Tool != "" {
		t.Errorf("got %+v, want an empty tool_call rather than a panic", ev)
	}
}
