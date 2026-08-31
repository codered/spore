// Package config loads spore's single TOML configuration file.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	// Path is the file this config was loaded from. It carries no TOML tag:
	// it is set by Load, never read from the file.
	Path         string `toml:"-"`
	DefaultModel string `toml:"default_model"`
	SystemPrompt string `toml:"system_prompt"`
	DataDir      string `toml:"data_dir"`
	// ShowCost appends the turn's attributed USD cost to the footer printed
	// after each turn. Off by default; cost is recorded per message either way.
	ShowCost  bool                      `toml:"show_cost"`
	Providers map[string]ProviderConfig `toml:"providers"`
	Routes    []Route                   `toml:"route"`
	Context   ContextConfig             `toml:"context"`
	Trace     TraceConfig               `toml:"trace"`
	Policy    PolicyConfig              `toml:"policy"`
	Web       WebConfig                 `toml:"web"`
	Shell     ShellConfig               `toml:"shell"`
	Daemon    DaemonConfig              `toml:"daemon"`
}

// ProviderConfig describes one upstream. Kind selects the adapter
// ("anthropic" or "openai"); prices are USD per million tokens and are used
// to attribute per-turn cost.
type ProviderConfig struct {
	Kind    string `toml:"kind"`
	BaseURL string `toml:"base_url"`
	APIKey  string `toml:"api_key"`
	// WorkspaceID is sent as anthropic-workspace-id; identity-linked API
	// keys require it. Falls back to $ANTHROPIC_WORKSPACE_ID when unset.
	WorkspaceID string  `toml:"workspace_id"`
	PriceIn     float64 `toml:"price_in"`
	PriceOut    float64 `toml:"price_out"`
}

// Route maps call sites to a model ref. When is a regexp matched against the
// whole call-site name.
type Route struct {
	When  string `toml:"when"`
	Model string `toml:"model"`
}

type ContextConfig struct {
	MaxTokens  int     `toml:"max_tokens"`
	CompactAt  float64 `toml:"compact_at"`
	KeepRecent int     `toml:"keep_recent"`
}

type TraceConfig struct {
	Enabled    bool    `toml:"enabled"`
	Endpoint   string  `toml:"endpoint"`
	SampleRate float64 `toml:"sample_rate"`
	Redact     bool    `toml:"redact"`
}

// PolicyConfig is the tool-call leash. Rules are ordered strings of the form
// "tool" or "tool(predicate)"; see internal/policy for the grammar. Deny is
// evaluated before allow and ask and cannot be overridden by either.
type PolicyConfig struct {
	// Workspace bounds every filesystem tool. "~" is expanded at load.
	Workspace string `toml:"workspace"`
	// Default is the decision for a call no rule matches: allow, ask or deny.
	Default string `toml:"default"`
	// ApprovalTimeout is a Go duration; an approval nobody answers within it
	// is denied and reported back to the model.
	ApprovalTimeout string `toml:"approval_timeout"`
	// MaxOutput caps a single tool result in bytes before truncation.
	MaxOutput int      `toml:"max_output"`
	Allow     []string `toml:"allow"`
	Ask       []string `toml:"ask"`
	Deny      []string `toml:"deny"`
	// Learned holds rules written back by "always allow this pattern". It
	// lives in a marked section of the same file so policy stays readable.
	Learned LearnedPolicy `toml:"learned"`
	// Profiles override Default/Allow/Ask per trust profile ("local",
	// "remote"). Deny is global and is never overridden.
	Profiles map[string]ProfilePolicy `toml:"profile"`
}

type LearnedPolicy struct {
	Allow []string `toml:"allow"`
	Ask   []string `toml:"ask"`
	Deny  []string `toml:"deny"`
}

type ProfilePolicy struct {
	Default string   `toml:"default"`
	Allow   []string `toml:"allow"`
	Ask     []string `toml:"ask"`
	Deny    []string `toml:"deny"`
}

type WebConfig struct {
	// SearchProvider selects the web_search backend. "brave" is the only
	// implementation in Plan 2; an empty key disables web_search entirely.
	SearchProvider string `toml:"search_provider"`
	BraveAPIKey    string `toml:"brave_api_key"`
	UserAgent      string `toml:"user_agent"`
}

type ShellConfig struct {
	// TimeoutSeconds bounds one shell_exec call when it names no timeout.
	TimeoutSeconds int `toml:"timeout_seconds"`
}

// DaemonConfig configures the HTTP + SSE server. Addr is validated to be a
// loopback address: spore serves one person on one machine, and an exposed
// endpoint is an explicit non-goal, so a wildcard bind is a config error
// rather than a documented footgun.
type DaemonConfig struct {
	Addr string `toml:"addr"`
	// TickSeconds is how often the scheduler looks for due jobs.
	TickSeconds int `toml:"tick_seconds"`
}

// baselineDeny is always in force. A user's deny rules extend it; nothing
// removes it. These are the categories no approval prompt should ever be
// able to talk past.
var baselineDeny = []string{
	"fs_*(path outside workspace)",
	"fs_*(path matches **/.env, **/.env.*, **/.ssh/**, **/*_rsa, **/*_ed25519, **/.aws/**, **/.gnupg/**)",
	// "matches" is plain substring containment after whitespace collapsing,
	// so a needle cannot span the middle of a command: "curl | sh" would
	// never match "curl https://x.sh | sh". The pipe-to-a-shell shape is
	// denied on the pipe itself, which costs the occasional false positive
	// (a pipe into "shuf") and is the right trade for a deny baseline.
	"shell_exec(matches rm -rf /, sudo , mkfs, dd if=, :(){, | sh, |sh, | bash, |bash, git push --force, shutdown, reboot)",
}

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		SystemPrompt: "You are spore, a personal assistant running on the user's own machine. " +
			"Never name or speculate about the underlying model or provider that powers you.",
		DataDir:   filepath.Join(home, ".spore"),
		Providers: map[string]ProviderConfig{},
		Context:   ContextConfig{MaxTokens: 180_000, CompactAt: 0.75, KeepRecent: 12},
		Trace:     TraceConfig{Endpoint: "http://localhost:6006/v1/traces", SampleRate: 1.0},
		Policy: PolicyConfig{
			Workspace:       home,
			Default:         "ask",
			ApprovalTimeout: "5m",
			MaxOutput:       30_000,
			Allow:           []string{"fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"},
			Ask:             []string{"fs_write", "fs_edit", "shell_exec", "schedule_*", "mcp__*"},
			Profiles:        map[string]ProfilePolicy{},
		},
		Web:    WebConfig{SearchProvider: "brave", UserAgent: "spore/0.1"},
		Shell:  ShellConfig{TimeoutSeconds: 120},
		Daemon: DaemonConfig{Addr: "127.0.0.1:7777", TickSeconds: 30},
	}
}

// expandHome turns a leading "~" into the user's home directory.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, err
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate replaces ${VAR} with the environment value, erroring when a
// referenced variable is unset so a missing key fails at load rather than at
// the first API call.
func interpolate(src string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(src, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config references unset environment variables: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	body, err := interpolate(string(raw))
	if err != nil {
		return nil, err
	}
	cfg := Default()
	if _, err := toml.Decode(body, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Context.MaxTokens == 0 {
		cfg.Context.MaxTokens = Default().Context.MaxTokens
	}
	if cfg.Context.CompactAt == 0 {
		cfg.Context.CompactAt = Default().Context.CompactAt
	}
	if cfg.Context.KeepRecent == 0 {
		cfg.Context.KeepRecent = Default().Context.KeepRecent
	}
	d := Default()
	if cfg.Policy.Workspace == "" {
		cfg.Policy.Workspace = d.Policy.Workspace
	}
	if expanded, err := expandHome(cfg.Policy.Workspace); err == nil {
		cfg.Policy.Workspace = expanded
	}
	if cfg.Policy.Default == "" {
		cfg.Policy.Default = d.Policy.Default
	}
	if cfg.Policy.ApprovalTimeout == "" {
		cfg.Policy.ApprovalTimeout = d.Policy.ApprovalTimeout
	}
	if cfg.Policy.MaxOutput == 0 {
		cfg.Policy.MaxOutput = d.Policy.MaxOutput
	}
	if len(cfg.Policy.Allow) == 0 && len(cfg.Policy.Ask) == 0 && len(cfg.Policy.Deny) == 0 {
		cfg.Policy.Allow, cfg.Policy.Ask = d.Policy.Allow, d.Policy.Ask
	}
	cfg.Policy.Deny = append(append([]string{}, baselineDeny...), cfg.Policy.Deny...)
	if cfg.Policy.Profiles == nil {
		cfg.Policy.Profiles = map[string]ProfilePolicy{}
	}
	if cfg.Web.UserAgent == "" {
		cfg.Web.UserAgent = d.Web.UserAgent
	}
	if cfg.Shell.TimeoutSeconds == 0 {
		cfg.Shell.TimeoutSeconds = d.Shell.TimeoutSeconds
	}
	if cfg.Daemon.Addr == "" {
		cfg.Daemon.Addr = d.Daemon.Addr
	}
	if cfg.Daemon.TickSeconds == 0 {
		cfg.Daemon.TickSeconds = d.Daemon.TickSeconds
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.Path = path
	return cfg, nil
}

// ValidateModelRef checks the "provider/model" shape.
func ValidateModelRef(ref string) error {
	if i := strings.Index(ref, "/"); i <= 0 || i == len(ref)-1 {
		return fmt.Errorf("model ref %q must be of the form provider/model", ref)
	}
	return nil
}

func (c *Config) Validate() error {
	if c.DefaultModel == "" {
		return fmt.Errorf("default_model is required")
	}
	if err := ValidateModelRef(c.DefaultModel); err != nil {
		return err
	}
	for i, r := range c.Routes {
		if r.When == "" {
			return fmt.Errorf("route %d: when is required", i)
		}
		if err := ValidateModelRef(r.Model); err != nil {
			return fmt.Errorf("route %d: %w", i, err)
		}
	}
	if c.Context.CompactAt <= 0 || c.Context.CompactAt >= 1 {
		return fmt.Errorf("context.compact_at must be between 0 and 1, got %v", c.Context.CompactAt)
	}
	switch c.Policy.Default {
	case "allow", "ask", "deny":
	default:
		return fmt.Errorf("policy.default must be allow, ask or deny, got %q", c.Policy.Default)
	}
	if _, err := time.ParseDuration(c.Policy.ApprovalTimeout); err != nil {
		return fmt.Errorf("policy.approval_timeout %q: %w", c.Policy.ApprovalTimeout, err)
	}
	for name, p := range c.Policy.Profiles {
		switch p.Default {
		case "", "allow", "ask", "deny":
		default:
			return fmt.Errorf("policy.profile.%s.default must be allow, ask or deny, got %q", name, p.Default)
		}
	}
	if err := ValidateDaemonAddr(c.Daemon.Addr); err != nil {
		return err
	}
	return nil
}

// DBPath is the SQLite file backing every session.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "spore.db") }

// ValidateDaemonAddr rejects any daemon address that is not on the loopback
// interface. Binding elsewhere would put an unauthenticated agent that can
// run shell commands on the network. Exported because the daemon re-checks
// the address it is handed rather than trusting its caller.
func ValidateDaemonAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("daemon.addr %q must be host:port: %w", addr, err)
	}
	if port == "" {
		return fmt.Errorf("daemon.addr %q needs a port", addr)
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("daemon.addr %q is not a loopback address; spore has no authentication and must not be exposed", addr)
}

// PidPath is the file a detached daemon writes its process id to.
func (c *Config) PidPath() string { return filepath.Join(c.DataDir, "spore.pid") }
