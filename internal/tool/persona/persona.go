// Package persona holds the write path into agent.md, the standing
// instructions for one workspace. soul.md has no tool: it is the user's own
// file, and a model revising its own personality is a feedback loop nothing
// yet needs.
//
// Reading both files is internal/persona; this package only writes.
package persona

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

// New builds the persona tools.
func New(cfg *config.Config) []tool.Tool { return []tool.Tool{newAgentNote(cfg)} }

type agentNote struct{ cfg *config.Config }

func newAgentNote(cfg *config.Config) agentNote { return agentNote{cfg: cfg} }

func (agentNote) Name() string { return "agent_note" }

func (a agentNote) Description() string {
	return "Record a standing instruction for this workspace: something the user has said to " +
		"always do, or never do, while working here. It is appended to the workspace's " +
		"agent.md and is in front of you in every future turn in this workspace. Use it for " +
		"orders (\"always run make lint before pushing\"); use memory for observations " +
		"(\"this repo uses Turborepo\"). Record one only when the user asks for it."
}

func (agentNote) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "text": {"type": "string", "description": "The instruction, as one line. Write it as an order, not as a note about the conversation."}
	  },
	  "required": ["text"]
	}`)
}

// ReadOnly is false: this writes a file, so the loop must not dispatch it
// alongside other calls.
func (agentNote) ReadOnly() bool { return false }

func (a agentNote) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Text string `json:"text"`
	}
	if len(args) == 0 {
		return "", fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", fmt.Errorf("an empty instruction would add a bare bullet to every future turn")
	}
	// Collapse newlines: the file is a bullet list, and a note spanning lines
	// would break the next append's assumption that one bullet is one line.
	text = strings.Join(strings.Fields(text), " ")

	path := a.cfg.AgentPath(policy.WorkspaceFrom(ctx))
	if path == "" {
		return "", fmt.Errorf("this session has no workspace, so there is nowhere to keep standing instructions for one")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: the path is built from config and the session workspace, not from model output
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("- " + text + "\n"); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return fmt.Sprintf("recorded in %s", path), nil
}
