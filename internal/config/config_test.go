package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadTestConfig(t *testing.T, toml string) *Config {
	t.Helper()
	p := write(t, "default_model = \"anthropic/claude-opus-5\"\n"+toml)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func loadTestConfigErr(t *testing.T, toml string) (*Config, error) {
	t.Helper()
	p := write(t, "default_model = \"anthropic/claude-opus-5\"\n"+toml)
	return Load(p)
}

func TestLoadInterpolatesEnvAndAppliesDefaults(t *testing.T) {
	t.Setenv("SPORE_TEST_KEY", "sk-secret")
	p := write(t, `
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${SPORE_TEST_KEY}"
price_in = 5.0
price_out = 25.0

[[route]]
when = "compaction|title|classify"
model = "ollama/qwen3:8b"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Providers["anthropic"].APIKey; got != "sk-secret" {
		t.Errorf("APIKey = %q, want interpolated %q", got, "sk-secret")
	}
	if cfg.Context.KeepRecent == 0 || cfg.Context.CompactAt == 0 || cfg.Context.MaxTokens == 0 {
		t.Errorf("defaults not applied to Context: %+v", cfg.Context)
	}
	if len(cfg.Routes) != 1 || cfg.Routes[0].Model != "ollama/qwen3:8b" {
		t.Errorf("routes = %+v", cfg.Routes)
	}
}

func TestLoadRejectsMissingEnvVar(t *testing.T) {
	p := write(t, `
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${SPORE_DEFINITELY_UNSET_VAR}"
`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load succeeded with an unset env var; want error")
	}
}

func TestLoadRejectsBadModelRef(t *testing.T) {
	p := write(t, `default_model = "claude-opus-5"`)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted a model ref with no provider prefix; want error")
	}
}

func TestPolicyDefaultsAndValidation(t *testing.T) {
	t.Setenv("SPORE_TEST_KEY", "sk-secret")
	p := write(t, `
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${SPORE_TEST_KEY}"

[policy]
workspace = "/home/u/dev"
deny = ["shell.exec(matches sudo)"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Policy.Default != "ask" {
		t.Errorf("policy.default = %q, want the \"ask\" default", cfg.Policy.Default)
	}
	if cfg.Policy.Workspace != "/home/u/dev" {
		t.Errorf("workspace = %q", cfg.Policy.Workspace)
	}
	// A config that names deny rules keeps the built-in deny list too: the
	// baseline protections are not opt-out by omission.
	joined := strings.Join(cfg.Policy.Deny, " ")
	if !strings.Contains(joined, "sudo") || !strings.Contains(joined, "path outside workspace") {
		t.Errorf("deny = %v, want both the user rule and the baseline", cfg.Policy.Deny)
	}
	if cfg.Policy.MaxOutput == 0 || cfg.Policy.ApprovalTimeout == "" {
		t.Error("max_output and approval_timeout must have defaults")
	}
}

func TestPolicyRejectsBadDefaultAndTimeout(t *testing.T) {
	for _, body := range []string{
		"[policy]\ndefault = \"maybe\"\n",
		"[policy]\napproval_timeout = \"soon\"\n",
	} {
		p := write(t, "default_model = \"a/b\"\n"+body)
		if _, err := Load(p); err == nil {
			t.Errorf("Load accepted invalid policy config:\n%s", body)
		}
	}
}

func TestDaemonDefaultsAndLoopbackOnly(t *testing.T) {
	p := write(t, `
default_model = "anthropic/claude-opus-5"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Daemon.Addr != "127.0.0.1:7777" {
		t.Errorf("daemon.addr = %q, want the loopback default", cfg.Daemon.Addr)
	}
	if cfg.Daemon.TickSeconds != 30 {
		t.Errorf("daemon.tick_seconds = %d, want 30", cfg.Daemon.TickSeconds)
	}
	if got, want := cfg.PidPath(), filepath.Join(cfg.DataDir, "spore.pid"); got != want {
		t.Errorf("PidPath() = %q, want %q", got, want)
	}

	// spore serves one person on one machine; a config that would expose the
	// API to the network is rejected at load, not quietly honoured.
	bad := write(t, `
default_model = "anthropic/claude-opus-5"

[daemon]
addr = "0.0.0.0:7777"
`)
	if _, err := Load(bad); err == nil {
		t.Fatal("Load accepted a non-loopback daemon.addr, want an error")
	}

	ok := write(t, `
default_model = "anthropic/claude-opus-5"

[daemon]
addr = "localhost:9999"
`)
	if _, err := Load(ok); err != nil {
		t.Fatalf("Load rejected a loopback host: %v", err)
	}
}

func TestLoadDiscordBridge(t *testing.T) {
	t.Setenv("SPORE_TEST_DISCORD_TOKEN", "s3cret")
	cfg := loadTestConfig(t, `
[bridge.discord]
enabled     = true
token       = "${SPORE_TEST_DISCORD_TOKEN}"
guild_id    = "111"
channel_ids = ["222", "333"]
user_ids    = ["444"]
allow_dms   = true
`)
	d := cfg.Bridge.Discord
	if !d.Enabled || d.Token != "s3cret" || d.GuildID != "111" || !d.AllowDMs {
		t.Fatalf("discord config not loaded: %+v", d)
	}
	if len(d.ChannelIDs) != 2 || d.ChannelIDs[0] != "222" {
		t.Fatalf("channel_ids = %v", d.ChannelIDs)
	}
	if len(d.UserIDs) != 1 || d.UserIDs[0] != "444" {
		t.Fatalf("user_ids = %v", d.UserIDs)
	}
}

func TestDiscordBridgeValidation(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "enabled with no token",
			toml: "[bridge.discord]\nenabled = true\nguild_id = \"1\"\nchannel_ids = [\"2\"]\nuser_ids = [\"3\"]\n",
			want: "bridge.discord.token",
		},
		{
			// An empty user allowlist admits nobody, which is safe but is
			// always a mistake — the bridge would sit there ignoring you.
			// Failing at load beats debugging silence.
			name: "enabled with no users",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nchannel_ids = [\"2\"]\n",
			want: "bridge.discord.user_ids",
		},
		{
			name: "enabled with a guild but no channels",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nuser_ids = [\"3\"]\n",
			want: "bridge.discord.channel_ids",
		},
		{
			name: "enabled with no surface at all",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nuser_ids = [\"3\"]\n",
			want: "guild_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTestConfigErr(t, tc.toml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDisabledDiscordBridgeSkipsValidation(t *testing.T) {
	// A block left behind with enabled = false must not block startup.
	if _, err := loadTestConfigErr(t, "[bridge.discord]\nenabled = false\n"); err != nil {
		t.Fatalf("a disabled bridge should not be validated: %v", err)
	}
}
