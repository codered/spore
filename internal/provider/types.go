// Package provider defines spore's model vocabulary and the streaming
// interface every adapter implements. It imports no transport and no store.
package provider

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Block is one piece of a message. Which fields are meaningful depends on
// Type: text uses Text; tool_use uses ID, Name and Input; tool_result uses ID,
// Content, IsError and Truncated.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature float64
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type EventType string

const (
	EventTextDelta EventType = "text_delta"
	EventToolCall  EventType = "tool_call"
	EventDone      EventType = "done"
	EventError     EventType = "error"
)

type Event struct {
	Type  EventType
	Text  string
	Block *Block
	Usage *Usage
	Err   error
}

// Provider streams one assistant response. Implementations must close the
// returned channel exactly once, and must emit either EventDone or
// EventError as their final event.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}
