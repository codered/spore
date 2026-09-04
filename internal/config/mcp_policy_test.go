package config_test

// This file is package config_test (external), not config, specifically so
// it can import internal/policy: internal/policy imports internal/config,
// so a test inside package config that imported policy would be an import
// cycle. See TestOperatorCanOverrideTheRemoteMCPDeny in config_test.go for
// the note on why the config-level assertion alone is not load-bearing.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

func writeMCPTestConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(p, []byte("default_model = \"anthropic/claude-opus-5\"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOperatorCanOverrideTheRemoteMCPDeny asserts the override where it
// actually matters: what internal/policy.Engine.Evaluate decides for the
// remote profile. A config-level check (cfg.Policy.Profiles["remote"].Deny)
// passes even if Default() declared no remote profile at all, because TOML
// decode builds Profiles["remote"] fresh from the file either way — that
// assertion is not load-bearing. Evaluate is: it is what the Discord bridge
// actually calls.
func TestOperatorCanOverrideTheRemoteMCPDeny(t *testing.T) {
	path := writeMCPTestConfig(t, "[policy.profile.remote]\ndeny = []\nallow = [\"fs_read\"]\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	tmpDir := filepath.Dir(path)
	call := policy.Call{Tool: "mcp__whatever", Args: json.RawMessage(`{}`)}
	if got := e.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileRemote, Workspace: tmpDir}, call); got.Decision == policy.DecisionDeny {
		t.Errorf("remote mcp__whatever = %q after an explicit override, want it not to be denied", got.Decision)
	}

	// The default config, with no override, must still deny it — otherwise
	// this test would pass even if the override did nothing.
	defaultPath := writeMCPTestConfig(t, "")
	defaultCfg, err := config.Load(defaultPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	defaultEngine, err := policy.NewEngine(defaultCfg.Policy)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defaultTmpDir := filepath.Dir(defaultPath)
	if got := defaultEngine.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileRemote, Workspace: defaultTmpDir}, call); got.Decision != policy.DecisionDeny {
		t.Errorf("remote mcp__whatever with no override = %q, want deny", got.Decision)
	}
}
