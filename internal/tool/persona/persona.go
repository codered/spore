// Package persona holds the write paths into agent.md, the standing
// instructions for one workspace, and self.md, spore's own notes. soul.md has
// no tool: it is the user's own file, and a model revising its own
// personality is a feedback loop nothing yet needs. self.md is spore's own,
// written with self_note when the companion is on.
//
// Reading both files is internal/persona; this package only writes.
package persona

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/refine"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

// SelfUpdater is the ledgered self.md writer; *refine.Refiner implements it.
type SelfUpdater interface {
	UpdateSelf(ctx context.Context, sessionID string, trig refine.Trigger, rationale string, edit func(string) (string, error)) (store.Refinement, error)
}

// New builds the persona tools. self_note exists only while the companion is
// on, so turning it off leaves the tool list exactly as it was.
func New(cfg *config.Config, self SelfUpdater) []tool.Tool {
	out := []tool.Tool{newAgentNote(cfg)}
	if cfg.Companion.Enabled {
		out = append(out, newSelfNote(cfg, self))
	}
	return out
}

type selfNote struct {
	cfg  *config.Config
	self SelfUpdater
}

func newSelfNote(cfg *config.Config, self SelfUpdater) selfNote {
	return selfNote{cfg: cfg, self: self}
}

func (selfNote) Name() string { return "self_note" }

func (selfNote) Description() string {
	return "Add one line to your own notes (self.md), which are in front of you in every conversation. " +
		"Use it for what you are curious about, an open thread with the user worth following up, " +
		"how they like to be talked to, or an opinion you have formed. Facts about the user go in memory instead."
}

func (selfNote) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "heading": {"type": "string", "enum": ["What I'm curious about", "Threads with you", "How you like to be talked to", "Opinions I've formed"]},
	    "text": {"type": "string", "description": "One line, in your own voice."}
	  },
	  "required": ["heading", "text"]
	}`)
}

// ReadOnly is false: it writes a file.
func (selfNote) ReadOnly() bool { return false }

func (s selfNote) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Heading string `json:"heading"`
		Text    string `json:"text"`
	}
	if len(args) == 0 {
		return "", fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	sid := policy.SessionFrom(ctx).ID
	if sid == "" {
		return "", fmt.Errorf("self_note needs a session to record the change against")
	}
	if len([]rune(strings.TrimSpace(in.Text))) > 300 {
		return "", fmt.Errorf("a note is one line of at most 300 characters")
	}
	_, err := s.self.UpdateSelf(ctx, sid, refine.TriggerTool, "self_note: "+in.Heading, func(cur string) (string, error) {
		return companion.AppendSelfNote(cur, in.Heading, in.Text)
	})
	if errors.Is(err, refine.ErrSelfTooLarge) {
		return "", fmt.Errorf("%w. Tell the user: they can trim %s or raise companion.self_max_bytes", err, s.cfg.SelfPath())
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("noted under %q in %s", in.Heading, s.cfg.SelfPath()), nil
}

type agentNote struct{ cfg *config.Config }

func newAgentNote(cfg *config.Config) agentNote { return agentNote{cfg: cfg} }

func (agentNote) Name() string { return "agent_note" }

func (a agentNote) Description() string {
	return "Record a standing order for this workspace (\"always run make lint before pushing\"). " +
		"It goes into the workspace's agent.md and is in front of you in every future turn here. " +
		"Use memory for observations instead. Only when the user asks for it."
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
