package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
)

// loadedEngine builds an engine through config.Load, so the baseline deny
// and the shipped defaults are in force exactly as a real config gets them.
func loadedEngine(t *testing.T, body string) *Engine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("default_model = \"anthropic/claude-opus-5\"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	e, err := NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func kernelCall() Call {
	return Call{Tool: KernelTool, Args: json.RawMessage(`{"code":"package main\nfunc main(){}"}`)}
}

func TestKernelToolAllowedByDefault(t *testing.T) {
	e := loadedEngine(t, "")
	got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: "/ws"}, kernelCall())
	if got.Decision != DecisionAllow || got.Rule != "policy.kernel" {
		t.Errorf("go_run = %q by %q, want allow by policy.kernel", got.Decision, got.Rule)
	}
}

// A remote profile written before go_run existed names its own allow list.
// go_run must not fall to that profile's "ask" default: every program would
// then wait on a Discord tap, although each thing it does is judged anyway.
func TestKernelToolFallsBackToAllowUnderAnExplicitProfile(t *testing.T) {
	e := loadedEngine(t, "[policy.profile.remote]\ndefault = \"ask\"\nallow = [\"fs_read\"]\n")
	got := e.Evaluate(Session{Profile: ProfileRemote, Workspace: "/ws"}, kernelCall())
	if got.Decision != DecisionAllow {
		t.Errorf("remote go_run = %q by %q, want allow", got.Decision, got.Rule)
	}
}

func TestExplicitRulesForKernelToolStillWin(t *testing.T) {
	e := loadedEngine(t, "[policy]\nask = [\"go_run\"]\n")
	if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: "/ws"}, kernelCall()); got.Decision != DecisionAsk {
		t.Errorf("explicit ask: go_run = %q, want ask", got.Decision)
	}
	e = loadedEngine(t, "[policy]\ndeny = [\"go_run\"]\n")
	if got := e.Evaluate(Session{Profile: ProfileLocal, Workspace: "/ws"}, kernelCall()); got.Decision != DecisionDeny {
		t.Errorf("explicit deny: go_run = %q, want deny", got.Decision)
	}
}
