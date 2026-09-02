package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

func engine(t *testing.T, pc config.PolicyConfig) *Engine {
	t.Helper()
	if pc.Default == "" {
		pc.Default = "ask"
	}
	if pc.ApprovalTimeout == "" {
		pc.ApprovalTimeout = "5m"
	}
	if pc.Workspace == "" {
		pc.Workspace = "/ws"
	}
	e, err := NewEngine(pc)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func TestDenyBeatsAllowRegardlessOfOrder(t *testing.T) {
	// The allow rule is listed first and is more specific. Deny still wins:
	// deny is evaluated before anything else and is absolute.
	e := engine(t, config.PolicyConfig{
		Allow: []string{"shell_exec"},
		Deny:  []string{"shell_exec(matches sudo)"},
	})
	args, _ := json.Marshal(map[string]string{"command": "sudo rm x"})
	got := e.Evaluate(ProfileLocal, Call{Tool: "shell_exec", Args: args})
	if got.Decision != DecisionDeny {
		t.Fatalf("Decision = %q, want deny (rule %q)", got.Decision, got.Rule)
	}
	if got.Rule != "shell_exec(matches sudo)" {
		t.Errorf("Rule = %q, want the deny rule reported", got.Rule)
	}
	// A shell call that trips no deny rule still reaches the allow rule.
	args, _ = json.Marshal(map[string]string{"command": "ls"})
	if got := e.Evaluate(ProfileLocal, Call{Tool: "shell_exec", Args: args}); got.Decision != DecisionAllow {
		t.Errorf("Decision = %q, want allow", got.Decision)
	}
}

func TestFirstMatchWinsBetweenAllowAndAsk(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Allow: []string{"fs_read"},
		Ask:   []string{"fs_*"},
	})
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_read", Args: json.RawMessage(`{}`)}); got.Decision != DecisionAllow {
		t.Errorf("fs_read = %q, want allow (the earlier list wins)", got.Decision)
	}
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_write", Args: json.RawMessage(`{}`)}); got.Decision != DecisionAsk {
		t.Errorf("fs_write = %q, want ask", got.Decision)
	}
}

func TestUnmatchedFallsBackToDefault(t *testing.T) {
	e := engine(t, config.PolicyConfig{Default: "deny", Allow: []string{"fs_read"}})
	got := e.Evaluate(ProfileLocal, Call{Tool: "mcp__x__y", Args: json.RawMessage(`{}`)})
	if got.Decision != DecisionDeny || got.Rule != "policy.default" {
		t.Errorf("got %+v, want deny via policy.default", got)
	}
}

func TestLearnedRulesApplyAfterConfiguredOnes(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Ask:     []string{"fs_write"},
		Learned: config.LearnedPolicy{Allow: []string{"fs_write"}},
	})
	// The hand-written ask rule is listed first, so it still wins: a learned
	// rule cannot silently loosen an explicit one.
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_write", Args: json.RawMessage(`{}`)}); got.Decision != DecisionAsk {
		t.Errorf("Decision = %q, want ask", got.Decision)
	}
}

func TestLearnedDenyIsAbsoluteToo(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Allow:   []string{"fs_write"},
		Learned: config.LearnedPolicy{Deny: []string{"fs_write"}},
	})
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_write", Args: json.RawMessage(`{}`)}); got.Decision != DecisionDeny {
		t.Errorf("Decision = %q, want deny", got.Decision)
	}
}

func TestLearnedAllowDoesNotCrossIntoAnotherProfile(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Learned:  config.LearnedPolicy{Allow: []string{"fs_write"}},
		Profiles: map[string]config.ProfilePolicy{"remote": {Default: "ask"}},
	})
	args := json.RawMessage(`{"path":"/ws/a"}`)
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_write", Args: args}); got.Decision != DecisionAllow {
		t.Errorf("local = %q, want allow — the learned rule applies to the base ruleset", got.Decision)
	}
	// An approval answered at the terminal must not silently extend the
	// permission to a bridge running under a different trust profile.
	if got := e.Evaluate(ProfileRemote, Call{Tool: "fs_write", Args: args}); got.Decision != DecisionAsk {
		t.Errorf("remote = %q, want ask — a rule learned in one trust context must not carry to another", got.Decision)
	}
}

func TestLearnedDenyCrossesEveryProfile(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Learned:  config.LearnedPolicy{Deny: []string{"fs_write"}},
		Profiles: map[string]config.ProfilePolicy{"remote": {Default: "allow", Allow: []string{"fs_write"}}},
	})
	args := json.RawMessage(`{"path":"/ws/a"}`)
	for _, p := range []Profile{ProfileLocal, ProfileRemote} {
		if got := e.Evaluate(p, Call{Tool: "fs_write", Args: args}); got.Decision != DecisionDeny {
			t.Errorf("profile %q = %q, want deny — learned deny is global", p, got.Decision)
		}
	}
}

func TestProfileOverridesAllowButNotDeny(t *testing.T) {
	e := engine(t, config.PolicyConfig{
		Allow: []string{"fs_write"},
		Deny:  []string{"fs_*(path matches **/.env)"},
		Profiles: map[string]config.ProfilePolicy{
			"remote": {Default: "ask", Ask: []string{"fs_write"}},
		},
	})
	plain, _ := json.Marshal(map[string]string{"path": "/ws/main.go"})
	if got := e.Evaluate(ProfileLocal, Call{Tool: "fs_write", Args: plain}); got.Decision != DecisionAllow {
		t.Errorf("local fs_write = %q, want allow", got.Decision)
	}
	if got := e.Evaluate(ProfileRemote, Call{Tool: "fs_write", Args: plain}); got.Decision != DecisionAsk {
		t.Errorf("remote fs_write = %q, want ask", got.Decision)
	}
	dotenv, _ := json.Marshal(map[string]string{"path": "/ws/.env"})
	for _, p := range []Profile{ProfileLocal, ProfileRemote} {
		if got := e.Evaluate(p, Call{Tool: "fs_write", Args: dotenv}); got.Decision != DecisionDeny {
			t.Errorf("profile %q .env write = %q, want deny", p, got.Decision)
		}
	}
}

// TestDenyOnlyProfileInheritsBaseAllowAndAsk pins the regression this fixes:
// config.Default()'s "remote" profile declares only Deny (mcp__*), and
// before NewEngine treated that as "inherit the base allow/ask" it made
// Evaluate silently fall through to an empty allow/ask set for every
// Discord call, turning fs_read (allowed by the base ruleset) into an ask.
// This loads through config.Load on a real file rather than building
// config.PolicyConfig by hand or calling config.Default() directly, because
// Load is what applies config.Default() and appends baselineDeny — a policy
// test that skips Load exercises a different, weaker policy than what a
// real config.Load call ever produces.
func TestDenyOnlyProfileInheritsBaseAllowAndAsk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(path, []byte("default_model = \"anthropic/claude-opus-5\"\n"), 0o600); err != nil {
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
	if got := e.Evaluate(ProfileRemote, Call{Tool: "fs_read", Args: json.RawMessage(`{}`)}); got.Decision != DecisionAllow {
		t.Errorf("remote fs_read = %q, want allow — a deny-only profile must inherit the base allow/ask, not lose it", got.Decision)
	}
	if got := e.Evaluate(ProfileRemote, Call{Tool: "mcp__whatever", Args: json.RawMessage(`{}`)}); got.Decision != DecisionDeny {
		t.Errorf("remote mcp__whatever = %q, want deny — the profile's own deny rule must still apply", got.Decision)
	}
}

func TestUnknownProfileUsesBaseRules(t *testing.T) {
	e := engine(t, config.PolicyConfig{Allow: []string{"fs_read"}})
	if got := e.Evaluate(Profile("telegram"), Call{Tool: "fs_read", Args: json.RawMessage(`{}`)}); got.Decision != DecisionAllow {
		t.Errorf("Decision = %q, want the base ruleset to apply", got.Decision)
	}
}

func TestMalformedArgsAreNeverAllowed(t *testing.T) {
	// Arguments that do not parse cannot be checked against argument
	// predicates, so they must not slip through an allow rule.
	e := engine(t, config.PolicyConfig{Allow: []string{"fs_read"}})
	for _, args := range []string{`{not json`, ``, `[1,2]`, `"a string"`, `null`} {
		got := e.Evaluate(ProfileLocal, Call{Tool: "fs_read", Args: json.RawMessage(args)})
		if got.Decision != DecisionDeny {
			t.Errorf("args %q: Decision = %q, want deny — an argument payload no\n"+
				"predicate can inspect must never reach a tool", args, got.Decision)
		}
	}
}

func TestNewEngineRejectsBadRuleAndTimeout(t *testing.T) {
	if _, err := NewEngine(config.PolicyConfig{Default: "ask", ApprovalTimeout: "5m", Allow: []string{"fs_read("}}); err == nil {
		t.Error("NewEngine accepted an unparseable rule")
	}
	if _, err := NewEngine(config.PolicyConfig{Default: "ask", ApprovalTimeout: "soon"}); err == nil {
		t.Error("NewEngine accepted an unparseable timeout")
	}
}

func TestApprovalTimeoutParsed(t *testing.T) {
	e := engine(t, config.PolicyConfig{ApprovalTimeout: "90s"})
	if e.ApprovalTimeout() != 90*time.Second {
		t.Errorf("ApprovalTimeout = %v, want 90s", e.ApprovalTimeout())
	}
}
