package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	skillfiles "github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/tool"
)

func fixture(t *testing.T) (*config.Config, *skillfiles.Caches, string) {
	t.Helper()
	data := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = data
	dir := filepath.Join(data, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, skillfiles.NewCaches(), dir
}

func ctxFor(ws string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{ID: "s1", Workspace: ws})
}

func find(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, x := range tools {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

func TestSkillLoadReturnsTheBody(t *testing.T) {
	cfg, caches, dir := fixture(t)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: "release-checklist", Description: "How to cut a release", Body: "Tag from master only.",
	}); err != nil {
		t.Fatal(err)
	}
	load := find(t, New(cfg, caches), "skill_load")
	out, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"release-checklist"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tag from master only.") {
		t.Fatalf("want the body, got %q", out)
	}
	if !load.ReadOnly() {
		t.Fatal("skill_load must be read-only")
	}
}

func TestSkillLoadUnknownNameIsAToolError(t *testing.T) {
	cfg, caches, _ := fixture(t)
	load := find(t, New(cfg, caches), "skill_load")
	if _, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"nope"}`)); err == nil {
		t.Fatal("an unknown skill must be an error the model can read")
	}
}

func TestSkillLoadRejectsTraversal(t *testing.T) {
	cfg, caches, _ := fixture(t)
	load := find(t, New(cfg, caches), "skill_load")
	if _, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"../../etc/passwd"}`)); err == nil {
		t.Fatal("a traversal name must be rejected")
	}
}

func TestSkillInstallWritesAndInvalidates(t *testing.T) {
	cfg, caches, dir := fixture(t)
	tools := New(cfg, caches)
	install := find(t, tools, "skill_install")
	if install.ReadOnly() {
		t.Fatal("skill_install writes files; it must not be read-only")
	}
	_, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"release-checklist","description":"How to cut a release","body":"Tag from master only."}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := skillfiles.Read(dir, "release-checklist")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "Tag from master only." {
		t.Fatalf("bad write: %+v", got)
	}
	// The index the next turn assembles must see it without a restart.
	names := caches.Skills(dir)
	if len(names) != 1 || names[0].Name != "release-checklist" {
		t.Fatalf("the cache must be invalidated by an install: %+v", names)
	}
}

func TestSkillInstallRejectsBadName(t *testing.T) {
	cfg, caches, _ := fixture(t)
	install := find(t, New(cfg, caches), "skill_install")
	if _, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"../evil","description":"d","body":"b"}`)); err == nil {
		t.Fatal("a traversal name must be rejected")
	}
}

func TestSkillInstallSaysWhenItUpdates(t *testing.T) {
	cfg, caches, dir := fixture(t)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: "release-checklist", Description: "old", Body: "old body",
	}); err != nil {
		t.Fatal(err)
	}
	install := find(t, New(cfg, caches), "skill_install")
	out, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"release-checklist","description":"new","body":"new body"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated") {
		t.Fatalf("an overwrite must say it updated: %q", out)
	}
}

func TestToolsAreOffWithoutASkillsDirectory(t *testing.T) {
	cfg, caches, _ := fixture(t)
	cfg.Skills.Scope = config.SkillsWorkspace
	load := find(t, New(cfg, caches), "skill_load")
	// A session with no root of its own has no skills directory.
	if _, err := load.Call(ctxFor(""), json.RawMessage(`{"name":"anything"}`)); err == nil {
		t.Fatal("a session with no skills directory must say so, not read the global one")
	}
}
