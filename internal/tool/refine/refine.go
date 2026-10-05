// Package refine is the model's handle on continual refinement: a tool that
// asks for a review of the conversation once the current turn ends. The
// review itself, and every rule about what it may write, is internal/refine.
package refine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

// Requester is the slice of *refine.Refiner this tool needs.
type Requester interface {
	Request(ctx context.Context, sessionID, instructions string) error
}

type refineTool struct{ r Requester }

func New(r Requester) tool.Tool { return refineTool{r: r} }

func (refineTool) Name() string { return "refine" }

func (refineTool) Description() string {
	return "Ask for a review of this conversation after this turn; it may record or fix a memory " +
		"fact or project note. Call it when the user corrects you or states how they want you " +
		"to work. Returns at once."
}

func (refineTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "instructions": {"type": "string", "description": "Optional: what the review should focus on."}
	  }
	}`)
}

// ReadOnly is true: it only sets a flag the turn's end reads, and Request is
// safe for concurrent use.
func (refineTool) ReadOnly() bool { return true }

func (t refineTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Instructions string `json:"instructions"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("bad arguments: %w", err)
		}
	}
	sid := policy.SessionFrom(ctx).ID
	if sid == "" {
		return "", fmt.Errorf("no session is attached to this call")
	}
	if err := t.r.Request(ctx, sid, in.Instructions); err != nil {
		return "", err
	}
	return "scheduled: the review runs when this turn ends", nil
}
