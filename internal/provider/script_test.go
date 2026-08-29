package provider

import (
	"context"
	"encoding/json"
	"testing"
)

func drain(t *testing.T, ch <-chan Event) (text string, calls []Block, usage Usage) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			text += ev.Text
		case EventToolCall:
			calls = append(calls, *ev.Block)
		case EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	return
}

func TestScriptReplaysTurnsInOrder(t *testing.T) {
	s := NewScript(
		ScriptTurn{
			ToolCalls: []Block{{Type: BlockToolUse, ID: "c1", Name: "fs.read", Input: json.RawMessage(`{"path":"go.mod"}`)}},
			Usage:     Usage{InputTokens: 100, OutputTokens: 20},
		},
		ScriptTurn{Text: "module github.com/codered/spore", Usage: Usage{InputTokens: 150, OutputTokens: 8}},
	)
	ctx := context.Background()

	ch, err := s.Stream(ctx, Request{Model: "fake"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, calls, usage := drain(t, ch)
	if len(calls) != 1 || calls[0].Name != "fs.read" {
		t.Fatalf("first turn calls = %+v", calls)
	}
	if usage.InputTokens != 100 {
		t.Errorf("usage = %+v", usage)
	}

	ch, _ = s.Stream(ctx, Request{Model: "fake"})
	text, calls, _ := drain(t, ch)
	if text != "module github.com/codered/spore" || len(calls) != 0 {
		t.Errorf("second turn text = %q, calls = %+v", text, calls)
	}
}

func TestScriptRecordsRequests(t *testing.T) {
	s := NewScript(ScriptTurn{Text: "ok"})
	ch, _ := s.Stream(context.Background(), Request{Model: "fake", System: "sys"})
	drain(t, ch)
	if got := s.Requests(); len(got) != 1 || got[0].System != "sys" {
		t.Fatalf("Requests() = %+v", got)
	}
}

func TestRegistryResolvesRefAndCost(t *testing.T) {
	r := NewRegistry()
	s := NewScript()
	r.Register("anthropic", s, ProviderPrice{In: 5, Out: 25})

	p, model, price, err := r.Resolve("anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p != Provider(s) || model != "claude-opus-5" {
		t.Errorf("Resolve gave (%v, %q)", p, model)
	}
	// 1M in at $5 + 1M out at $25.
	if got := price.Cost(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); got != 30 {
		t.Errorf("Cost = %v, want 30", got)
	}
	if _, _, _, err := r.Resolve("nope/model"); err == nil {
		t.Error("Resolve accepted an unregistered provider")
	}
}
