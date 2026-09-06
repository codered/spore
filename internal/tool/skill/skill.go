// Package skill exposes the skills directory to the model: skill_load reads
// one in, and skill_install is the single write path into a directory the
// filesystem tools cannot reach.
package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	skillfiles "github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/tool"
)

// New builds both skill tools around one cache set. The directory is resolved
// per call from the calling session's workspace, because under
// skills.scope = "workspace" each session reads a directory of its own.
func New(cfg *config.Config, caches *skillfiles.Caches) []tool.Tool {
	return []tool.Tool{loadTool{cfg: cfg, caches: caches}, installTool{cfg: cfg, caches: caches}}
}

func dirFor(cfg *config.Config, ctx context.Context) (string, error) {
	dir := cfg.SkillsDir(policy.WorkspaceFrom(ctx))
	if dir == "" {
		return "", fmt.Errorf("this session has no skills directory: skills.scope is \"workspace\" and the session has no workspace of its own")
	}
	return dir, nil
}

func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type loadTool struct {
	cfg    *config.Config
	caches *skillfiles.Caches
}

func (loadTool) Name() string { return "skill_load" }

func (loadTool) Description() string {
	return "Read one skill in full: a markdown document of instructions the user wrote for a " +
		"particular kind of work. The skills index in your context lists what is available by " +
		"name and description; load one before you act on the work it covers."
}

func (loadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "description": "The skill's name, as listed in the skills index."}
	  },
	  "required": ["name"]
	}`)
}

// ReadOnly is true: this reads a file the user wrote and changes nothing.
func (loadTool) ReadOnly() bool { return true }

func (t loadTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir, err := dirFor(t.cfg, ctx)
	if err != nil {
		return "", err
	}
	if err := skillfiles.ValidName(a.Name); err != nil {
		return "", err
	}
	s, err := skillfiles.Read(dir, a.Name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s\n\n%s\n", s.Name, s.Body), nil
}

type installTool struct {
	cfg    *config.Config
	caches *skillfiles.Caches
}

func (installTool) Name() string { return "skill_install" }

func (installTool) Description() string {
	return "Install a skill: write a markdown document of instructions into the user's skills " +
		"directory, where it is listed in every future conversation and can be loaded with " +
		"skill_load. Install one only when the user asks for it."
}

func (installTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "description": "Lowercase kebab-case identifier, e.g. release-checklist."},
	    "description": {"type": "string", "description": "One line saying when to load this skill."},
	    "body": {"type": "string", "description": "The skill itself, in markdown."}
	  },
	  "required": ["name", "description", "body"]
	}`)
}

// ReadOnly is false: this writes files, so the loop must not dispatch it
// alongside other calls.
func (installTool) ReadOnly() bool { return false }

func (t installTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir, err := dirFor(t.cfg, ctx)
	if err != nil {
		return "", err
	}
	if err := skillfiles.ValidName(a.Name); err != nil {
		return "", err
	}
	updated := skillfiles.Exists(dir, a.Name)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: a.Name, Description: a.Description, Body: a.Body,
	}); err != nil {
		return "", err
	}
	// The index the next turn assembles is built from the cache, so an
	// install that did not invalidate it would be invisible until the TTL.
	t.caches.Invalidate(dir)
	verb := "installed"
	if updated {
		verb = "updated"
	}
	return fmt.Sprintf("%s skill %q", verb, a.Name), nil
}
