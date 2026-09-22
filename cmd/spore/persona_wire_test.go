package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

// agentUnderTest pairs a built agent with the store it was built on, so a
// test can create a session and then take a snapshot of it.
type agentUnderTest struct {
	a  *agent.Agent
	st *store.Store
}

// buildAgentAt builds an agent rooted at a temp data dir, the way the skills
// wiring tests do.
func buildAgentAt(t *testing.T, cfg *config.Config) *agentUnderTest {
	t.Helper()
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a, _, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	return &agentUnderTest{a: a, st: st}
}

// The persona files are only reachable if buildAgent's Snapshot fills them.
// Every other piece -- the reader, the config accessors, the renderers --
// works and is dead weight if this wiring is missing, and the failure is
// silent: the prompt simply never mentions them.
func TestBuildAgentWiresSoulAndAgentFiles(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}

	if err := os.WriteFile(cfg.SoulPath(), []byte("SOUL-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".spore"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.AgentPath(ws), []byte("AGENT-MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}

	u := buildAgentAt(t, cfg)
	sid, err := u.st.CreateSession(context.Background(), "", ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Workspace: ws})
	snap, err := u.a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.Contains(snap.Soul, "SOUL-MARKER") {
		t.Fatalf("soul.md never reaches the prompt: Soul = %q", snap.Soul)
	}
	if !strings.Contains(snap.Agent, "AGENT-MARKER") {
		t.Fatalf("agent.md never reaches the prompt: Agent = %q", snap.Agent)
	}
}

// A session in a workspace with no agent.md must produce no section, not an
// error: that is the ordinary case on almost every machine.
func TestBuildAgentToleratesAbsentPersonaFiles(t *testing.T) {
	dir := t.TempDir()
	ws := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}

	u := buildAgentAt(t, cfg)
	sid, err := u.st.CreateSession(context.Background(), "", ws)
	if err != nil {
		t.Fatal(err)
	}
	ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Workspace: ws})
	snap, err := u.a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("absent persona files must not fail a turn: %v", err)
	}
	if snap.Soul != "" || snap.Agent != "" {
		t.Fatalf("absent files produced content: Soul=%q Agent=%q", snap.Soul, snap.Agent)
	}
}

// A tool nobody registered is a tool the model cannot call, however complete
// its implementation and its policy entries are.
func TestBuildToolsRegistersAgentNote(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}

	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	a, _, _, _, err := buildAgent(cfg, st, terminalApprover{lines: scannerLines{sc: stdinLines}, out: os.Stdout})
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	var found bool
	for _, spec := range a.Tools.Specs() {
		if spec.Name == "agent_note" {
			found = true
		}
	}
	if !found {
		t.Fatal("agent_note is not registered, so the model can never call it")
	}
}
