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
	return "Read one skill in full. The skills index in your context lists them; load one before " +
		"doing the work it covers. A skill that ships other files lists them after its body; pass " +
		"one as file to read it."
}

func (loadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "description": "The skill's name, as listed in the skills index."},
	    "file": {"type": "string", "description": "Optional: one of the skill's other files, as its body's file list names it. Omit to read the skill itself."}
	  },
	  "required": ["name"]
	}`)
}

// ReadOnly is true: this reads a file the user wrote and changes nothing.
func (loadTool) ReadOnly() bool { return true }

func (t loadTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
		File string `json:"file"`
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
	if a.File != "" {
		body, err := skillfiles.ReadFile(dir, a.Name, a.File)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("# %s/%s\n\n%s", a.Name, a.File, body), nil
	}
	s, err := skillfiles.Read(dir, a.Name)
	if err != nil {
		return "", err
	}
	out := fmt.Sprintf("# %s\n\n%s\n", s.Name, s.Body)
	// The skills directory is outside the workspace, so these files are
	// reachable only through this tool; a body that cites references/x.md
	// is useless unless the model is told it can read it here.
	files, err := skillfiles.Files(dir, a.Name)
	if err != nil || len(files) == 0 {
		return out, nil //nolint:nilerr // the body loaded; a listing failure costs only the list
	}
	out += fmt.Sprintf("\nThis skill's other files. Read one with skill_load {\"name\": %q, \"file\": \"<path>\"}:\n", a.Name)
	for _, f := range files {
		out += "- " + f + "\n"
	}
	return out, nil
}

type installTool struct {
	cfg    *config.Config
	caches *skillfiles.Caches
}

func (installTool) Name() string { return "skill_install" }

// Description names the directory rather than alluding to it. A model that
// cannot say where skills go answers "where do I install skills?" by
// searching the filesystem, which finds config keys and database noise.
//
// Under workspace scope the path depends on the session's root, which is not
// known here -- Description takes no context -- so the shape is described
// instead of a path that would be wrong for most sessions.
func (t installTool) Description() string {
	where := "the user's skills directory"
	if t.cfg != nil {
		if t.cfg.Skills.Scope == config.SkillsWorkspace {
			where = "the .spore/skills directory under the session's own workspace"
		} else if dir := t.cfg.SkillsDir(""); dir != "" {
			where = dir
		}
	}
	return "Install a skill: write <name>/SKILL.md into " + where +
		", listed in every future conversation and loaded with skill_load. Only when the user asks for it."
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
