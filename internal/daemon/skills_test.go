package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/store"
)

// TestSkillsEndpoint lists skills with body sizes, loaded markers from the
// transcript, and per-file load errors. The endpoint reads through the agent's
// skills cache, so a skill_install is visible immediately.
func TestSkillsEndpoint(t *testing.T) {
	s, ts := newTestServer(t)

	// Two valid skills and one with broken frontmatter, so the error path is
	// exercised alongside the listing.
	dir := t.TempDir()
	if err := skill.Write(dir, skill.Skill{Name: "alpha", Description: "the alpha skill", Body: "do alpha things"}); err != nil {
		t.Fatal(err)
	}
	if err := skill.Write(dir, skill.Skill{Name: "beta", Description: "the beta skill", Body: "do beta things"}); err != nil {
		t.Fatal(err)
	}
	// A broken skill: a directory with a SKILL.md whose frontmatter has an
	// unknown key, so Load reports a per-file error.
	brokenDir := filepath.Join(dir, "gamma")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "SKILL.md"), []byte("---\nname: gamma\ndescription: broken\nbad: key\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.agent.Skills = skill.NewCaches()
	s.cfg.Skills.Dir = dir

	// Create a session and append a tool_use block for skill_load so "alpha"
	// is marked loaded.
	ctx := context.Background()
	sid, err := s.store.CreateSession(ctx, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	// A loaded skill has both the skill_load tool_use and its successful
	// tool_result. A call without a result was interrupted and must not be
	// reported as loaded.
	blocks := []provider.Block{
		{Type: provider.BlockToolUse, ID: "call_1", Name: "skill_load",
			Input: json.RawMessage(`{"name":"alpha"}`)},
		{Type: provider.BlockToolResult, ID: "call_1", Content: "# alpha\n\ndo alpha things"},
	}
	raw, _ := json.Marshal(blocks)
	if _, err := s.store.AppendMessage(ctx, store.Message{
		SessionID: sid, Role: "assistant", BlocksJSON: raw,
	}); err != nil {
		t.Fatal(err)
	}
	// An interrupted load and an error result must not mark beta loaded.
	failed := []provider.Block{
		{Type: provider.BlockToolUse, ID: "call_2", Name: "skill_load",
			Input: json.RawMessage(`{"name":"beta"}`)},
		{Type: provider.BlockToolResult, ID: "call_2", Content: "no skill named beta", IsError: true},
		{Type: provider.BlockToolUse, ID: "call_3", Name: "skill_load",
			Input: json.RawMessage(`{"name":"beta"}`)},
	}
	failedRaw, _ := json.Marshal(failed)
	if _, err := s.store.AppendMessage(ctx, store.Message{
		SessionID: sid, Role: "assistant", BlocksJSON: failedRaw,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := http.Get(ts.URL + "/api/sessions/" + sid + "/skills")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want 200: %s", res.StatusCode, body)
	}
	var out SkillsJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	if len(out.Skills) != 2 {
		t.Fatalf("skills = %d, want 2 (alpha and beta; gamma is broken): %+v", len(out.Skills), out.Skills)
	}
	if out.Skills[0].Name != "alpha" {
		t.Errorf("first skill = %q, want alpha", out.Skills[0].Name)
	}
	if !out.Skills[0].Loaded {
		t.Errorf("alpha must be marked loaded (skill_load appeared in the transcript)")
	}
	if out.Skills[0].BodyTokens <= 0 {
		t.Errorf("alpha body_tokens = %d, want > 0", out.Skills[0].BodyTokens)
	}
	if out.Skills[1].Loaded {
		t.Errorf("beta must not be marked loaded")
	}
	if len(out.Errors) != 1 {
		t.Fatalf("errors = %d, want 1 (gamma has bad frontmatter): %+v", len(out.Errors), out.Errors)
	}
	if err := s.store.SetSummaryThrough(ctx, sid, 2); err != nil {
		t.Fatal(err)
	}
	res, err = http.Get(ts.URL + "/api/sessions/" + sid + "/skills")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var afterClear SkillsJSON
	if err := json.NewDecoder(res.Body).Decode(&afterClear); err != nil {
		t.Fatal(err)
	}
	if afterClear.Skills[0].Loaded {
		t.Error("alpha must not be loaded after its tool result is folded out of the live context")
	}
}
