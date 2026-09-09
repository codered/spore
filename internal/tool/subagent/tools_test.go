package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/tool"
)

func TestAgentRunToolIsNotReadOnly(t *testing.T) {
	for _, tl := range New(nil) {
		if tl.ReadOnly() {
			t.Errorf("%s reports ReadOnly, but a child mutates and must not join a parallel batch", tl.Name())
		}
	}
}

func TestAgentRunRequiresAPrompt(t *testing.T) {
	tools := New(nil)
	var run tool.Tool
	for _, tl := range tools {
		if tl.Name() == "agent_run" {
			run = tl
		}
	}
	if run == nil {
		t.Fatal("New did not return agent_run")
	}
	if _, err := run.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("agent_run accepted an empty prompt")
	} else if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("error = %v, want it to name the missing prompt", err)
	}
}
