package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/store"
)

// The skills subsystem is only reachable if buildAgent hands the agent a way
// to read the index. Without it Snapshot leaves snap.Skills empty, no index is
// assembled, and the model never learns that a skill exists to load -- every
// other piece (files, caches, tools, policy) works and is dead weight.
func TestBuildAgentWiresTheSkillsIndex(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-x"},
	}

	if err := os.MkdirAll(cfg.SkillsDir(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skill.Write(cfg.SkillsDir(""), skill.Skill{
		Name:        "release-checklist",
		Description: "How to cut a release",
		Body:        "Tag from master only.",
	}); err != nil {
		t.Fatal(err)
	}

	a, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if a.Skills == nil {
		t.Fatal("buildAgent left Agent.Skills nil: no skills index ever reaches a prompt")
	}
	got := a.Skills.Skills(cfg.SkillsDir(""))
	if len(got) != 1 || got[0].Name != "release-checklist" {
		t.Fatalf("skills index = %+v, want the one installed skill", got)
	}
}

// Under workspace scope a session with no root of its own has no skills
// directory, and asking for one must be empty rather than falling back to the
// global directory -- that fallback would leak the operator's skills into a
// scope they deliberately narrowed.
func TestBuildAgentSkillsAreEmptyForARootlessSessionUnderWorkspaceScope(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-x"},
	}
	cfg.Skills.Scope = config.SkillsWorkspace

	// A skill in the global directory, which this scope must not read.
	globalDir := filepath.Join(dir, "skills")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := skill.Write(globalDir, skill.Skill{
		Name: "release-checklist", Description: "d", Body: "b",
	}); err != nil {
		t.Fatal(err)
	}

	a, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if got := a.Skills.Skills(cfg.SkillsDir("")); len(got) != 0 {
		t.Fatalf("a session with no root must see no skills, got %+v", got)
	}
}
