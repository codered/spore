// Package subagent exposes the sub-agent supervisor as tools. It holds no
// state: internal/subagent is the single owner of what is running.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/subagent"
	"github.com/codered/spore/internal/tool"
)

// New returns the sub-agent tools. Every one reports ReadOnly() == false: a
// child mutates, so it must never be dispatched inside the loop's parallel
// read-only batch.
func New(sup *subagent.Supervisor) []tool.Tool {
	return []tool.Tool{&runTool{sup: sup}}
}

type runTool struct{ sup *subagent.Supervisor }

func (t *runTool) Name() string { return "agent_run" }

func (t *runTool) Description() string {
	return "Run a sub-agent on a self-contained task and wait for its answer. " +
		"Use this to keep a long, noisy investigation out of your own context: " +
		"the sub-agent reads what it needs and you get only its conclusion. " +
		"Give it one complete instruction -- it cannot see this conversation."
}

func (t *runTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "prompt": {
	      "type": "string",
	      "description": "The complete, self-contained task for the sub-agent."
	    }
	  },
	  "required": ["prompt"]
	}`)
}

func (t *runTool) ReadOnly() bool { return false }

func (t *runTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_run: %w", err)
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("agent_run: prompt is required")
	}
	sess := policy.SessionFrom(ctx)
	if sess.ID == "" {
		return "", fmt.Errorf("agent_run: no session on the context")
	}
	st, err := t.sup.Run(ctx, sess.ID, in.Prompt)
	if err != nil {
		return "", err
	}
	if st.Result == "" {
		return fmt.Sprintf("sub-agent %s finished without a reply", st.ID), nil
	}
	return st.Result, nil
}
