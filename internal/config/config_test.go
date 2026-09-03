package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeConfig is write plus the default_model every config needs to pass
// Validate; the MCP tests below care about MCP validation, not this baseline
// requirement, so it is supplied for them the same way loadTestConfig
// supplies it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return write(t, "default_model = \"anthropic/claude-opus-5\"\n"+body)
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
		{
			// A whitespace-only guild_id must not pass as a real surface: it
			// would clear this check and then fail every later comparison in
			// admit, producing exactly the "starts and then silently ignores
			// you" failure this function exists to prevent.
			name: "enabled with a whitespace-only guild_id and no DMs",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"   \"\nuser_ids = [\"3\"]\n",
			want: "bridge.discord.guild_id",
		},
		{
			// An empty user_id entry would become a live allowlist key,
			// silently admitting any message with a zero UserID. Fail at load.
			name: "enabled with an empty user_id entry",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nchannel_ids = [\"2\"]\nuser_ids = [\"3\", \"\", \"4\"]\n",
			want: "user_ids[1]",
		},
		{
			// An empty channel_id entry would become a live allowlist key,
			// silently admitting any thread with a zero ParentID. Fail at load.
			name: "enabled with an empty channel_id entry",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nchannel_ids = [\"2\", \"\", \"3\"]\nuser_ids = [\"4\"]\n",
			want: "channel_ids[1]",
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

func TestMCPServerValidation(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "valid stdio server",
			body: `
[[mcp.server]]
name = "notion"
transport = "stdio"
command = "npx"
args = ["-y", "server"]
env = { NOTION_TOKEN = "t" }
inherit = ["HOME"]
`,
		},
		{
			name: "valid http server",
			body: `
[[mcp.server]]
name = "remote-1"
transport = "http"
url = "https://example.com/mcp"
`,
		},
		{
			name:    "missing name",
			body:    "[[mcp.server]]\ntransport = \"stdio\"\ncommand = \"x\"\n",
			wantErr: "name",
		},
		{
			name:    "name with illegal characters",
			body:    "[[mcp.server]]\nname = \"No/tion\"\ntransport = \"stdio\"\ncommand = \"x\"\n",
			wantErr: "name",
		},
		{
			name:    "duplicate names",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\n[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"y\"\n",
			wantErr: "duplicate",
		},
		{
			name:    "unknown transport",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"carrier-pigeon\"\n",
			wantErr: "transport",
		},
		{
			name:    "stdio without command",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\n",
			wantErr: "command",
		},
		{
			name:    "stdio with url",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nurl = \"https://example.com\"\n",
			wantErr: "url",
		},
		{
			name:    "http without url",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\n",
			wantErr: "url",
		},
		{
			name:    "http with command",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"https://example.com\"\ncommand = \"x\"\n",
			wantErr: "command",
		},
		{
			name:    "http url must be absolute",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"http\"\nurl = \"/mcp\"\n",
			wantErr: "url",
		},
		{
			name:    "unparsable timeout",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\ntimeout = \"soon\"\n",
			wantErr: "timeout",
		},
		{
			name:    "inherit name is not an environment variable name",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\ninherit = [\"not a name\"]\n",
			wantErr: "inherit",
		},
		{
			name:    "empty env key",
			body:    "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\nenv = { \"\" = \"v\" }\n",
			wantErr: "env",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, tc.body)
			_, err := Load(path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Load: %v, want no error", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v, want one mentioning %q", err, tc.wantErr)
			}
		})
	}
}

func TestMCPTimeoutDefaults(t *testing.T) {
	path := writeConfig(t, "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MCP.Servers[0].CallTimeout(); got != 60*time.Second {
		t.Errorf("CallTimeout() = %v, want 60s", got)
	}

	path = writeConfig(t, "[[mcp.server]]\nname = \"a\"\ntransport = \"stdio\"\ncommand = \"x\"\ntimeout = \"5s\"\n")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MCP.Servers[0].CallTimeout(); got != 5*time.Second {
		t.Errorf("CallTimeout() = %v, want 5s", got)
	}
}

// The remote trust profile must not reach an MCP server by default: a Discord
// user is not the operator who declared it.
func TestDefaultDeniesMCPForRemote(t *testing.T) {
	path := writeConfig(t, "")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	remote, ok := cfg.Policy.Profiles["remote"]
	if !ok {
		t.Fatal("no remote profile in the default policy")
	}
	if !slices.Contains(remote.Deny, "mcp__*") {
		t.Errorf("remote profile deny = %v, want it to contain mcp__*", remote.Deny)
	}
}

// TestOperatorCanOverrideTheRemoteMCPDeny lives in mcp_policy_test.go
// (package config_test) rather than here: asserting the override at the
// config.Policy.Profiles level is not load-bearing, because TOML decode
// builds Profiles["remote"] fresh from the file regardless of what
// Default() put there — the assertion that matters is what an
// internal/policy.Engine built from the result actually decides, and this
// package cannot import internal/policy without an import cycle (policy
// imports config).

func TestDefaultFactBudget(t *testing.T) {
	if got := Default().Context.FactBudget; got != 2000 {
		t.Fatalf("fact_budget default = %d, want 2000", got)
	}
}

func TestNegativeFactBudgetIsRejected(t *testing.T) {
	c := Default()
	c.DefaultModel = "m"
	c.Providers = map[string]ProviderConfig{"p": {Kind: "anthropic"}}
	c.Context.FactBudget = -1
	if err := c.Validate(); err == nil {
		t.Fatal("a negative fact_budget was accepted")
	}
}
