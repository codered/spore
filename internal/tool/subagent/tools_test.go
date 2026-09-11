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

func TestNewReturnsTheThreeTools(t *testing.T) {
	var names []string
	for _, tl := range New(nil) {
		names = append(names, tl.Name())
	}
	if got, want := strings.Join(names, ","), "agent_run,agent_spawn,agent_result"; got != want {
		t.Errorf("tools = %s, want %s", got, want)
	}
}

func TestAgentResultRequiresAnID(t *testing.T) {
	var result tool.Tool
	for _, tl := range New(nil) {
		if tl.Name() == "agent_result" {
			result = tl
		}
	}
	if result == nil {
		t.Fatal("New did not return agent_result")
	}
	if _, err := result.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("agent_result accepted an empty id")
	} else if !strings.Contains(err.Error(), "id is required") {
		t.Errorf("error = %v, want it to name the missing id", err)
	}
}
