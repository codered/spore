package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// self_note writes text that rides in every later prompt, so the remote
// profile denies it for the same reason it denies memory. Checked through
// config.Load and the engine, never config.Default(): Default skips the
// baseline deny and the profile merge Load performs.
func TestSelfNotePolicyByProfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(p, []byte("default_model = \"anthropic/claude-opus-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	call := policy.Call{Tool: "self_note", Args: json.RawMessage(`{"heading":"Threads with you","text":"x"}`)}
	ws := filepath.Dir(p)
	if got := e.Evaluate(policy.Session{ID: "s", Profile: policy.ProfileLocal, Workspace: ws}, call); got.Decision != policy.DecisionAllow {
		t.Errorf("local self_note = %q, want allow", got.Decision)
	}
	if got := e.Evaluate(policy.Session{ID: "s", Profile: policy.ProfileRemote, Workspace: ws}, call); got.Decision != policy.DecisionDeny {
		t.Errorf("remote self_note = %q, want deny", got.Decision)
	}
}
