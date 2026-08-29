package provider

import (
	"context"
	"fmt"
	"sync"
)

// ScriptTurn is one canned assistant response.
type ScriptTurn struct {
	Text      string
	ToolCalls []Block
	Usage     Usage
	Err       error
}

// Script is a Provider that replays canned turns in order. It is the test
// double behind every agent-loop test, so the loop can be exercised with no
// network and byte-exact expectations.
type Script struct {
	mu       sync.Mutex
	turns    []ScriptTurn
	next     int
	requests []Request
}

func NewScript(turns ...ScriptTurn) *Script { return &Script{turns: turns} }

func (s *Script) Name() string { return "script" }

// Requests returns every request the loop has sent, in order.
func (s *Script) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Script) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	s.mu.Lock()
	if s.next >= len(s.turns) {
		s.mu.Unlock()
		return nil, fmt.Errorf("script exhausted after %d turns", len(s.turns))
	}
	turn := s.turns[s.next]
	s.next++
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	ch := make(chan Event, len(turn.ToolCalls)+2)
	go func() {
		defer close(ch)
		if turn.Err != nil {
			ch <- Event{Type: EventError, Err: turn.Err}
			return
		}
		if turn.Text != "" {
			ch <- Event{Type: EventTextDelta, Text: turn.Text}
		}
		for i := range turn.ToolCalls {
			b := turn.ToolCalls[i]
			ch <- Event{Type: EventToolCall, Block: &b}
		}
		u := turn.Usage
		ch <- Event{Type: EventDone, Usage: &u}
	}()
	return ch, nil
}
