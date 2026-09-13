// Package subagent exposes the sub-agent supervisor as tools. It holds no
// state: internal/subagent is the single owner of what is running.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/subagent"
	"github.com/codered/spore/internal/tool"
)

// New returns the sub-agent tools. Every one reports ReadOnly() == false: a
// child mutates, so it must never be dispatched inside the loop's parallel
// read-only batch.
func New(sup *subagent.Supervisor) []tool.Tool {
	return []tool.Tool{&runTool{sup: sup}, &spawnTool{sup: sup}, &resultTool{sup: sup}}
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

type spawnTool struct{ sup *subagent.Supervisor }

func (t *spawnTool) Name() string { return "agent_spawn" }

func (t *spawnTool) Description() string {
	return "Start a sub-agent on a self-contained task and return immediately " +
		"with its id, without waiting. Use this for work that should continue " +
		"while you carry on -- collect the answer later with agent_result. " +
		"Give it one complete instruction: it cannot see this conversation."
}

func (t *spawnTool) Schema() json.RawMessage {
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

func (t *spawnTool) ReadOnly() bool { return false }

func (t *spawnTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_spawn: %w", err)
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("agent_spawn: prompt is required")
	}
	sess := policy.SessionFrom(ctx)
	if sess.ID == "" {
		return "", fmt.Errorf("agent_spawn: no session on the context")
	}
	id, err := t.sup.Spawn(ctx, sess.ID, in.Prompt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("started sub-agent %s; collect it with agent_result", id), nil
}

type resultTool struct{ sup *subagent.Supervisor }

func (t *resultTool) Name() string { return "agent_result" }

func (t *resultTool) Description() string {
	return "Read a sub-agent's state and, once it has finished, its answer. " +
		"Call it with the id agent_spawn returned. A sub-agent that is still " +
		"running reports running and has no answer yet."
}

func (t *resultTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "id": {"type": "string", "description": "The sub-agent id agent_spawn returned."}
	  },
	  "required": ["id"]
	}`)
}

// ReadOnly is false even though this only reads: it must not join a parallel
// batch alongside the spawns whose ids it reports on.
func (t *resultTool) ReadOnly() bool { return false }

func (t *resultTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_result: %w", err)
	}
	if in.ID == "" {
		return "", fmt.Errorf("agent_result: id is required")
	}
	st, err := t.sup.Result(ctx, in.ID)
	if err != nil {
		return "", err
	}
	switch st.State {
	case "running":
		return fmt.Sprintf("sub-agent %s is still running (%s so far)", st.ID, time.Since(st.Started).Round(time.Second)), nil
	case "done":
		if st.Result == "" {
			return fmt.Sprintf("sub-agent %s finished without a reply", st.ID), nil
		}
		return st.Result, nil
	default:
		return fmt.Sprintf("sub-agent %s %s: %s", st.ID, st.State, st.Error), nil
	}
}
