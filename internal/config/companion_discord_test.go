package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

func loadCompanionTOML(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(p, []byte("default_model = \"anthropic/claude-opus-5\"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

const companionBridgeTOML = `
[bridge.discord]
enabled = true
token = "t"
guild_id = "G"
channel_ids = ["C1"]
user_ids = ["U", "V"]
`

func TestCompanionDiscordDefaults(t *testing.T) {
	cfg, err := loadCompanionTOML(t, companionBridgeTOML+"\n[companion]\nenabled = true\n")
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Companion.Discord
	if d.Mode != config.CompanionDiscordThread {
		t.Errorf("mode = %q, want thread", d.Mode)
	}
	if !slices.Equal(d.UserIDs, []string{"U", "V"}) {
		t.Errorf("user_ids = %v, want the bridge's", d.UserIDs)
	}
	if got := cfg.CompanionChannel(); got != config.CompanionChannelTerminal {
		t.Errorf("auto with no channel_id = %q, want terminal", got)
	}

	cfg, err = loadCompanionTOML(t, companionBridgeTOML+
		"\n[companion]\nenabled = true\n[companion.discord]\nchannel_id = \"CC\"\nuser_ids = [\"U\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CompanionChannel(); got != config.CompanionChannelDiscord {
		t.Errorf("auto with a channel_id = %q, want discord", got)
	}
}

func TestCompanionDiscordValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"bad mode", companionBridgeTOML + "[companion.discord]\nmode = \"sms\"\n", "companion.discord.mode"},
		{"stranger", companionBridgeTOML + "[companion.discord]\nuser_ids = [\"W\"]\n", "not in bridge.discord.user_ids"},
		{"discord without channel id", companionBridgeTOML + "[companion]\nenabled = true\nchannel = \"discord\"\n", "channel_id is required"},
		{"dm without allow_dms", companionBridgeTOML + "[companion]\nenabled = true\nchannel = \"discord\"\n[companion.discord]\nmode = \"dm\"\n", "allow_dms"},
		{"discord without bridge", "[companion]\nenabled = true\nchannel = \"discord\"\n", "[bridge.discord]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadCompanionTOML(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Watch jobs run unattended: read-only tools and spore's own notes, nothing
// else, and never the companion's own tools.
func TestCompanionProfilePolicy(t *testing.T) {
	if string(policy.ProfileCompanion) != config.CompanionProfile {
		t.Fatalf("policy.ProfileCompanion %q != config.CompanionProfile %q", policy.ProfileCompanion, config.CompanionProfile)
	}
	cfg, err := loadCompanionTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	sess := policy.Session{ID: "s", Profile: policy.ProfileCompanion, Workspace: cfg.Policy.Workspace}
	for tool, want := range map[string]policy.Decision{
		"shell_exec":         policy.DecisionDeny,
		"fs_write":           policy.DecisionDeny,
		"fs_edit":            policy.DecisionDeny,
		"memory":             policy.DecisionDeny,
		"schedule_create":    policy.DecisionDeny,
		"agent_spawn":        policy.DecisionDeny,
		"skill_install":      policy.DecisionDeny,
		"mcp__gh__issue":     policy.DecisionDeny,
		"interest_update":    policy.DecisionDeny,
		"companion_feedback": policy.DecisionDeny,
		"made_up_tool":       policy.DecisionDeny,
		"web_fetch":          policy.DecisionAllow,
		"recall_search":      policy.DecisionAllow,
		"self_note":          policy.DecisionAllow,
		"fs_list":            policy.DecisionAllow,
	} {
		args := `{}`
		if tool == "fs_list" {
			args = `{"path":"."}`
		}
		if got := e.Evaluate(sess, policy.Call{Tool: tool, Args: json.RawMessage(args)}); got.Decision != want {
			t.Errorf("companion %s = %q (rule %s), want %q", tool, got.Decision, got.Rule, want)
		}
	}
}

func TestCompanionToolsAllowedLocalAndDefaultRemote(t *testing.T) {
	cfg, err := loadCompanionTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []policy.Profile{policy.ProfileLocal, policy.ProfileRemote} {
		for _, tool := range []string{"interest_update", "companion_feedback"} {
			got := e.Evaluate(policy.Session{ID: "s", Profile: p, Workspace: cfg.Policy.Workspace},
				policy.Call{Tool: tool, Args: json.RawMessage(`{}`)})
			if got.Decision != policy.DecisionAllow {
				t.Errorf("%s %s = %q, want allow", p, tool, got.Decision)
			}
		}
	}
}

// An operator's own [policy.profile.companion] replaces the default whole.
func TestOperatorCompanionProfileWins(t *testing.T) {
	cfg, err := loadCompanionTOML(t, "[policy.profile.companion]\ndefault = \"deny\"\nallow = [\"web_*\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Policy.Profiles[config.CompanionProfile].Allow; !slices.Equal(got, []string{"web_*"}) {
		t.Fatalf("companion allow = %v, want the operator's", got)
	}
}
