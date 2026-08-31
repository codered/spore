// Package daemon serves spore over HTTP and SSE. It is a consumer of the
// agent's event channel and of the policy guard; the core knows nothing
// about it (spec invariant 1).
package daemon

import (
	"encoding/json"

	"github.com/codered/spore/internal/agent"
)

// Wire event types. These strings are the API: the web UI and the CLI client
// both switch on them, so they are append-only.
const (
	WireText       = "text"
	WireToolCall   = "tool_call"
	WireToolResult = "tool_result"
	WireTurnDone   = "turn_done"
	WireError      = "error"
	WireApproval   = "approval"
	WireResolved   = "resolved"
)

// WireEvent is one server-sent event. It is comparable on purpose — tests
// assert round-trip equality — so it holds no slices or maps; Args is a
// string carrying JSON rather than a json.RawMessage.
type WireEvent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_call / tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Args      string `json:"args,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	// turn_done
	Model     string  `json:"model,omitempty"`
	TokensIn  int     `json:"tokens_in,omitempty"`
	TokensOut int     `json:"tokens_out,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`

	// error
	Error string `json:"error,omitempty"`

	// approval / resolved
	PendingID int64  `json:"pending_id,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Decision  string `json:"decision,omitempty"`
}

// FromAgent converts a core event into its wire form. The agent's Err field
// is an error value, which does not marshal; it becomes text here.
func FromAgent(ev agent.Event) WireEvent {
	switch ev.Type {
	case agent.EvText:
		return WireEvent{Type: WireText, Text: ev.Text}
	case agent.EvToolCall:
		w := WireEvent{Type: WireToolCall}
		if ev.Block != nil {
			w.ToolUseID, w.Tool, w.Args = ev.Block.ID, ev.Block.Name, string(ev.Block.Input)
		}
		return w
	case agent.EvToolResult:
		w := WireEvent{Type: WireToolResult}
		if ev.Block != nil {
			w.ToolUseID, w.Content = ev.Block.ID, ev.Block.Content
			w.IsError, w.Truncated = ev.Block.IsError, ev.Block.Truncated
		}
		return w
	case agent.EvTurnDone:
		return WireEvent{
			Type: WireTurnDone, Model: ev.Model,
			TokensIn: ev.Usage.InputTokens, TokensOut: ev.Usage.OutputTokens,
			CostUSD: ev.Cost,
		}
	case agent.EvError:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		return WireEvent{Type: WireError, Error: msg}
	default:
		return WireEvent{Type: string(ev.Type)}
	}
}

// Encode renders the event as an SSE data frame body.
func (w WireEvent) Encode() ([]byte, error) { return json.Marshal(w) }
