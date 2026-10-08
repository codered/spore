// Package config loads spore's single TOML configuration file.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	Bridge    BridgeConfig              `toml:"bridge"`
	MCP       MCPConfig                 `toml:"mcp"`
	Recall    RecallConfig              `toml:"recall"`
	Skills    SkillsConfig              `toml:"skills"`
	Subagents SubagentConfig            `toml:"subagents"`
	Kernel    KernelConfig              `toml:"kernel"`
	Refine    RefineConfig              `toml:"refine"`
}

// SkillsConfig chooses where skills are read from and installed to.
type SkillsConfig struct {
	// Scope is "global" -- one directory for every session, the default --
	// or "workspace", a directory under each session's own root.
	//
	// Workspace scope is opt-in for a reason: a session rooted at a cloned
	// repository would load text the operator did not write into its prompt
	// index, and spore's policy model assumes the prompt is yours.
	Scope string `toml:"scope"`
	// Dir overrides the global directory. It is ignored under workspace
	// scope, where the session's root decides.
	Dir string `toml:"dir"`
}

// SubagentConfig bounds a tree of agents. context.max_round_trips bounds one
// agent's round trips; none of it bounds a parent that keeps spawning.
type SubagentConfig struct {
	// MaxDepth is the refusal point for nesting depth. The default 2 means a
	// top-level session may spawn one level of children; those children may not
	// spawn. With max_depth=3, a top-level session spawns children, and each of
	// those may spawn one child.
	MaxDepth int `toml:"max_depth"`
	// MaxCostUSD is the ceiling for a whole tree: the root and every
	// descendant, summed. Depth alone does not see a wide flat fan-out.
	MaxCostUSD float64 `toml:"max_cost_usd"`
	// MaxConcurrent is how many children may run at once under one root. An
	// unbounded spawn batch reaches provider rate limits long before it
	// reaches the cost ceiling.
	MaxConcurrent int `toml:"max_concurrent"`
}

// RefineConfig drives continual refinement: the reviewer pass that turns what
// a session learned into memory facts and project notes.
type RefineConfig struct {
	// Enabled gates the automatic triggers (compaction, idle, the model's
	// refine tool). Unset is true. Manual /refine, review and rollback work
	// either way: turning the automation off must not strand proposals.
	Enabled *bool `toml:"enabled"`
	// IdleMinutes is how long a session sits untouched before the sweeper
	// reviews it.
	IdleMinutes int `toml:"idle_minutes"`
	// MaxEdits caps the edits one round may make or propose.
	MaxEdits int `toml:"max_edits"`
}

// On reports whether the automatic triggers run. Unset is true.
func (r RefineConfig) On() bool { return r.Enabled == nil || *r.Enabled }

// Skills scope names.
const (
	SkillsGlobal    = "global"
	SkillsWorkspace = "workspace"
)

// SkillsDir is the skills directory for a session rooted at workspace. Under
// the default global scope every session shares one directory and workspace
// is ignored. Under workspace scope a session with no root of its own -- the
// web UI, the scheduler, a bridge -- has no skills, reported as an empty path
// rather than as an error.
func (c *Config) SkillsDir(workspace string) string {
	if c.Skills.Scope == SkillsWorkspace {
		if workspace == "" {
			return ""
		}
		return filepath.Join(workspace, ".spore", "skills")
	}
	if c.Skills.Dir != "" {
		return c.Skills.Dir
	}
	return filepath.Join(c.DataDir, "skills")
}

// Recall backend names. sqlitefts ships in every build and needs nothing;
// weaviate is a mirror over the same corpus and can be unreachable, which is
// why choosing it never switches the keyword index off.
const (
	RecallSQLiteFTS = "sqlitefts"
	RecallWeaviate  = "weaviate"
)

// RecallConfig selects the search backend. An empty URL means the instance
// `spore recall setup` provisions on loopback; setting it points spore at one
// the operator runs and turns provisioning off.
type RecallConfig struct {
	Backend string `toml:"backend"`
	URL     string `toml:"url"`
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
	// PriceCacheWrite and PriceCacheRead are USD per million tokens for
	// cached input. Unset, they default to 1.25x and 0.10x of PriceIn.
	PriceCacheWrite float64 `toml:"price_cache_write"`
	PriceCacheRead  float64 `toml:"price_cache_read"`
	// Cache turns prompt caching on for an anthropic provider. Unset means
	// true: cheaper and faster is the right default, and an operator should
	// not have to find a flag to get it.
	Cache *bool `toml:"cache"`
}

// CacheEnabled reports whether to send cache breakpoints. Unset is true.
func (p ProviderConfig) CacheEnabled() bool { return p.Cache == nil || *p.Cache }

// Route maps call sites to a model ref. When is a regexp matched against the
// whole call-site name.
type Route struct {
	When  string `toml:"when"`
	Model string `toml:"model"`
}

type ContextConfig struct {
	MaxTokens int     `toml:"max_tokens"`
	CompactAt float64 `toml:"compact_at"`
	// MaxOutputTokens caps one model reply: its text, its tool calls and any
	// hidden reasoning the model does first. A reasoning model can spend the
	// whole cap thinking and reply with nothing, so raise it for those.
	MaxOutputTokens int `toml:"max_output_tokens"`
	// MaxRoundTrips caps one turn's trips to the model: each tool call the
	// model makes costs one more. A turn that reaches it fails. 0 means no
	// cap, for long tasks; such a turn ends only when the model stops calling
	// tools or the turn is stopped.
	MaxRoundTrips int `toml:"max_round_trips"`
	KeepRecent    int `toml:"keep_recent"`
	// FactBudget caps the estimated tokens of inlined fact bodies. Facts past
	// the budget still appear, as one name-and-description line each, so the
	// model always knows they exist.
	FactBudget int `toml:"fact_budget"`
	// SkillBudget caps the estimated tokens of the skills index, which
	// carries one name-and-description line per skill. Skills past the
	// budget are dropped from the index with the count stated, so a long
	// list never crowds out the conversation.
	SkillBudget int `toml:"skill_budget"`
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
	// Learned holds rules written back when a pattern is accepted in review. It
	// lives in a marked section of the same file so policy stays readable.
	Learned LearnedPolicy `toml:"learned"`
	// Profiles override Default/Allow/Ask per trust profile ("local",
	// "remote"). Deny is global and is never overridden.
	Profiles map[string]ProfilePolicy `toml:"profile"`
	// MCPPaths says, per declared MCP server, whether its calls are checked
	// for paths and where it resolves a relative path. Load fills it from
	// [[mcp.server]]; it cannot be set under [policy].
	MCPPaths map[string]MCPPathMode `toml:"-"`
}

type LearnedPolicy struct {
	Allow []string `toml:"allow"`
	Ask   []string `toml:"ask"`
	Deny  []string `toml:"deny"`
}

type ProfilePolicy struct {
	Default string `toml:"default"`
	// Workspace roots every session created on this profile at one
	// directory, instead of at a session directory of its own. It is the
	// operator saying a bridge user is meant to work on something real; it
	// is itself checked against policy.workspace at load.
	Workspace string   `toml:"workspace"`
	Allow     []string `toml:"allow"`
	Ask       []string `toml:"ask"`
	Deny      []string `toml:"deny"`
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

// Kernel modes. In code mode the model is offered only go_run and reaches
// every other tool through the spore package inside its program; in tools
// mode it sees every tool, go_run included.
const (
	KernelModeCode  = "code"
	KernelModeTools = "tools"
)

// KernelConfig configures go_run, the Go kernel.
type KernelConfig struct {
	Mode string `toml:"mode"`
	// TimeoutSeconds is the default interpreter budget for one program. Time
	// spent inside a spore.* helper (an approval wait included) is not
	// charged to it.
	TimeoutSeconds int `toml:"timeout_seconds"`
	// MaxTimeoutSeconds caps what a program may ask for.
	MaxTimeoutSeconds int `toml:"max_timeout_seconds"`
	// CeilingSeconds is a wall-clock stop with no pauses.
	CeilingSeconds int `toml:"ceiling_seconds"`
	// HelperMaxBytes caps one helper result handed to a program.
	HelperMaxBytes int `toml:"helper_max_bytes"`
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

// BridgeConfig groups the chat bridges. Only Discord exists; Telegram is the
// same interface implemented again and is deferred.
type BridgeConfig struct {
	Discord DiscordConfig `toml:"discord"`
}

// DiscordConfig is both the connection and the trust boundary. GuildID,
// ChannelIDs and UserIDs are an allowlist, not a filter: the bridge is the
// first surface someone other than the local human can reach, so anything
// not named here is dropped. UserIDs applies to guild messages and DMs
// alike — the two surfaces must not be able to drift apart.
type DiscordConfig struct {
	Enabled bool   `toml:"enabled"`
	Token   string `toml:"token"`
	// GuildID is the one server the bot serves. Membership of a private
	// guild is the outer boundary; the user allowlist is the inner one.
	GuildID string `toml:"guild_id"`
	// ChannelIDs are the channels a session may be started from. A thread's
	// parent channel is what is matched, so threads need no entry.
	ChannelIDs []string `toml:"channel_ids"`
	UserIDs    []string `toml:"user_ids"`
	// AllowDMs opens the direct-message surface to the same user allowlist.
	AllowDMs bool `toml:"allow_dms"`
}

// MCPConfig declares the MCP servers spore hosts. Declaring a server here is
// the authorization to run it — the same trust as declaring a provider API
// key — so the file is the trust boundary and there is no sandbox. What the
// child does not get is anything it was not given: see MCPServer.Env and
// MCPServer.Inherit.
type MCPConfig struct {
	Servers []MCPServer `toml:"server"`
}

// MCPServer is one hosted server. Transport is "stdio" (Command/Args) or
// "http" (URL); the two sets of fields are mutually exclusive.
type MCPServer struct {
	// Name is the namespace its tools are registered under, as
	// mcp__<name>__<tool>.
	Name      string   `toml:"name"`
	Transport string   `toml:"transport"`
	Command   string   `toml:"command"`
	Args      []string `toml:"args"`
	// Env is passed to the child verbatim. The child's environment is built
	// from scratch, so nothing in spore's own environment — provider API keys
	// above all — reaches it unless it is named in Inherit.
	Env map[string]string `toml:"env"`
	// Inherit names environment variables copied from spore's environment.
	Inherit []string `toml:"inherit"`
	URL     string   `toml:"url"`
	// Timeout is a Go duration bounding one tool call. Defaults to 60s.
	Timeout string `toml:"timeout"`
	// LocalPaths says whether this server's path-shaped arguments name files
	// on this machine. When unset it is true, and the baseline rule
	// mcp__*(any path outside workspace) checks them. Set it to false for a
	// server whose paths are remote, such as repository paths or object keys;
	// its calls are then not checked for paths at all. It is a pointer
	// because a plain bool decodes a missing key as false, which would exempt
	// every server by default.
	LocalPaths *bool `toml:"local_paths"`
}

// PathsChecked reports whether this server's path arguments are judged by the
// baseline MCP containment rule. A missing local_paths key means yes.
func (s MCPServer) PathsChecked() bool { return s.LocalPaths == nil || *s.LocalPaths }

// MCPPathMode says how one declared MCP server's path arguments are judged.
type MCPPathMode struct {
	// Checked is false only for a server the operator marked local_paths =
	// false. A server that is not in the table at all is checked.
	Checked bool
	// Cwd is the directory the server resolves a relative path against. It is
	// the workspace ceiling for stdio servers and empty for http servers,
	// whose working directory spore does not know.
	Cwd string
}

const defaultMCPCallTimeout = 60 * time.Second

// CallTimeout is the parsed Timeout, or the default. Load has already
// rejected an unparsable value, so this cannot fail at call time.
func (s MCPServer) CallTimeout() time.Duration {
	if s.Timeout == "" {
		return defaultMCPCallTimeout
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 {
		return defaultMCPCallTimeout
	}
	return d
}

// validateDiscord fails a half-filled bridge block at load. Every message
// this returns names the exact key to fix, because the failure mode it
// prevents — a bridge that starts and then silently ignores you — is
// otherwise very hard to diagnose from the outside.
func validateDiscord(d DiscordConfig) error {
	if !d.Enabled {
		return nil
	}
	if strings.TrimSpace(d.Token) == "" {
		return fmt.Errorf("bridge.discord.token is required when the bridge is enabled")
	}
	if len(d.UserIDs) == 0 {
		return fmt.Errorf("bridge.discord.user_ids must name at least one Discord user ID; an empty allowlist admits nobody")
	}
	// An empty or whitespace-only entry would become a live allowlist key,
	// silently admitting any message with an empty UserID. Fail at load
	// rather than silently widening the trust boundary.
	for i, u := range d.UserIDs {
		if strings.TrimSpace(u) == "" {
			return fmt.Errorf("bridge.discord.user_ids[%d] is empty; allowlist entries must not be blank", i)
		}
	}
	if strings.TrimSpace(d.GuildID) == "" && !d.AllowDMs {
		return fmt.Errorf("bridge.discord needs a surface: set bridge.discord.guild_id, or bridge.discord.allow_dms = true, or both")
	}
	if d.GuildID != "" && len(d.ChannelIDs) == 0 {
		return fmt.Errorf("bridge.discord.channel_ids must name at least one channel when guild_id is set")
	}
	// Same check for channels: an empty entry would silently admit any thread
	// or channel with a zero-value ID.
	for i, c := range d.ChannelIDs {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("bridge.discord.channel_ids[%d] is empty; allowlist entries must not be blank", i)
		}
	}
	return nil
}

var (
	mcpNameRE = regexp.MustCompile(`\A[a-z0-9_-]{1,24}\z`)
	envNameRE = regexp.MustCompile(`\A[A-Za-z_][A-Za-z0-9_]*\z`)
)

// validateMCP rejects a malformed server declaration at load, so a typo is a
// startup error rather than a server that silently never appears.
func validateMCP(c MCPConfig) error {
	seen := map[string]bool{}
	for i, s := range c.Servers {
		where := fmt.Sprintf("mcp.server[%d]", i)
		if !mcpNameRE.MatchString(s.Name) {
			return fmt.Errorf("%s: name %q must match %s", where, s.Name, mcpNameRE)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s: duplicate server name %q", where, s.Name)
		}
		seen[s.Name] = true

		switch s.Transport {
		case "stdio":
			if s.Command == "" {
				return fmt.Errorf("%s: transport stdio needs a command", where)
			}
			if s.URL != "" {
				return fmt.Errorf("%s: transport stdio takes no url", where)
			}
		case "http":
			if s.URL == "" {
				return fmt.Errorf("%s: transport http needs a url", where)
			}
			u, err := url.Parse(s.URL)
			if err != nil || !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") {
				return fmt.Errorf("%s: url %q must be an absolute http or https URL", where, s.URL)
			}
			if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 || len(s.Inherit) > 0 {
				return fmt.Errorf("%s: transport http takes no command, args, env or inherit", where)
			}
		default:
			return fmt.Errorf("%s: unknown transport %q (want stdio or http)", where, s.Transport)
		}

		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil {
				return fmt.Errorf("%s: timeout %q: %w", where, s.Timeout, err)
			}
			if d <= 0 {
				return fmt.Errorf("%s: timeout %q must be positive", where, s.Timeout)
			}
		}
		for _, name := range s.Inherit {
			if !envNameRE.MatchString(name) {
				return fmt.Errorf("%s: inherit %q is not an environment variable name", where, name)
			}
		}
		for k := range s.Env {
			if !envNameRE.MatchString(k) {
				return fmt.Errorf("%s: env key %q is not an environment variable name", where, k)
			}
		}
	}
	return nil
}

// baselineDeny is always in force. A user's deny rules extend it; nothing
// removes it. These are the categories no approval prompt should ever be
// able to talk past.
var baselineDeny = []string{
	"fs_*(path outside workspace)",
	// Section 6 of the design spec promises that an MCP tool's path arguments
	// are judged against the calling session's workspace. This is that bound.
	// The predicate is valid only on an mcp__ glob, because it finds paths by
	// shape as well as by key name and that breadth would be wrong anywhere
	// else. An operator exempts a server whose paths are not local files with
	// local_paths = false on its [[mcp.server]] block.
	"mcp__*(any path outside workspace)",
	"fs_*(path matches **/.env, **/.env.*, **/.ssh/**, **/*_rsa, **/*_ed25519, **/.aws/**, **/.gnupg/**, **/daemon.token)",
	// "matches" is plain substring containment after whitespace collapsing,
	// so a needle cannot span the middle of a command: "curl | sh" would
	// never match "curl https://x.sh | sh". The pipe-to-a-shell shape is
	// denied on the pipe itself, which costs the occasional false positive
	// (a pipe into "shuf") and is the right trade for a deny baseline.
	"shell_exec(matches rm -rf /, sudo , mkfs, dd if=, :(){, | sh, |sh, | bash, |bash, git push --force, shutdown, reboot)",
	// The credential files the fs_* rule protects, held against the shell as
	// well: without this, "cat .env" read what fs_read could not, wherever a
	// user had allowed shell_exec. Each word of the command is judged as a
	// path, so "process.env" is not caught.
	"shell_exec(word matches **/.env, **/.env.*, **/.ssh, **/.ssh/**, **/*_rsa, **/*_ed25519, **/.aws, **/.aws/**, **/.gnupg, **/.gnupg/**, **/daemon.token)",
	// rm -rf / is above; the home directory is the other target no task needs
	// to delete whole.
	"shell_exec(matches rm -rf ~, rm -rf $HOME, rm -rf ${HOME}, rm -fr ~, rm -fr /, rm -fr $HOME)",
	// The daemon token is what every /api route checks. fs_read and cat are
	// held off it above; this keeps the model from running the command that
	// opens a signed-in browser. An interpreter run through shell_exec can
	// still read the file -- spore runs as the operator -- so this is a
	// speed bump, and the spec says so.
	"shell_exec(matches spore web)",
}

// BaselineDeny returns the rules Load always prepends to policy.deny. It is a
// copy: the policy view uses it to tell a baseline rule from one a human
// wrote, and must not be able to change what is enforced.
func BaselineDeny() []string { return slices.Clone(baselineDeny) }

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		SystemPrompt: "You are spore, a personal assistant running on the user's own machine. " +
			"Never name or speculate about the underlying model or provider that powers you.",
		DataDir:   filepath.Join(home, ".spore"),
		Providers: map[string]ProviderConfig{},
		Context:   ContextConfig{MaxTokens: 180_000, MaxOutputTokens: 4096, MaxRoundTrips: 30, CompactAt: 0.75, KeepRecent: 12, FactBudget: 2000, SkillBudget: 500},
		Skills:    SkillsConfig{Scope: SkillsGlobal},
		Trace:     TraceConfig{Endpoint: "http://localhost:6006/v1/traces", SampleRate: 1.0},
		Recall:    RecallConfig{Backend: RecallSQLiteFTS},
		Policy: PolicyConfig{
			Workspace:       home,
			Default:         "ask",
			ApprovalTimeout: "5m",
			MaxOutput:       30_000,
			Allow:           []string{"fs_read", "fs_list", "fs_glob", "fs_grep", "web_*", "schedule_list", "job_output", "schedule_notify", "recall_search", "skill_load", "refine"},
			Ask:             []string{"fs_write", "fs_edit", "shell_exec", "schedule_create", "schedule_cancel", "mcp__*", "memory", "skill_install", "agent_note"},
			// The remote profile denies MCP outright: a Discord user is not
			// the operator who declared the server, and an MCP server is
			// reached through credentials that operator supplied. It denies
			// memory for the same shape of reason: a fact written once shapes
			// every later turn in every session, so a single injection through
			// a bridge would otherwise plant permanent context. Both are
			// ordinary config lines an operator may edit -- they are
			// deliberately NOT part of baselineDeny, which is reserved for the
			// rules no approval may ever talk past.
			Profiles: map[string]ProfilePolicy{
				"remote": {Deny: []string{"mcp__*", "memory", "skill_install", "agent_note"}},
			},
		},
		Web:       WebConfig{SearchProvider: "brave", UserAgent: "spore/0.1"},
		Shell:     ShellConfig{TimeoutSeconds: 120},
		Daemon:    DaemonConfig{Addr: "127.0.0.1:7777", TickSeconds: 30},
		Subagents: SubagentConfig{MaxDepth: 2, MaxCostUSD: 1.00, MaxConcurrent: 4},
		Kernel: KernelConfig{Mode: KernelModeCode, TimeoutSeconds: 60, MaxTimeoutSeconds: 300,
			CeilingSeconds: 1800, HelperMaxBytes: 4 << 20},
		Refine: RefineConfig{IdleMinutes: 10, MaxEdits: 5},
	}
}

// fillKernelDefaults replaces each zero field with its default. A negative
// value is left for Validate to reject.
func fillKernelDefaults(k *KernelConfig, d KernelConfig) {
	if k.Mode == "" {
		k.Mode = d.Mode
	}
	if k.TimeoutSeconds == 0 {
		k.TimeoutSeconds = d.TimeoutSeconds
	}
	if k.MaxTimeoutSeconds == 0 {
		k.MaxTimeoutSeconds = d.MaxTimeoutSeconds
	}
	if k.CeilingSeconds == 0 {
		k.CeilingSeconds = d.CeilingSeconds
	}
	if k.HelperMaxBytes == 0 {
		k.HelperMaxBytes = d.HelperMaxBytes
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

// insideCeiling is a lexical containment check on path boundaries, so
// "/ws-evil" is not inside "/ws". It is deliberately simpler than
// policy.Inside -- config cannot import policy, which imports config -- and
// that is sound here because both paths are operator-written configuration,
// not tool arguments: there is no attacker-supplied symlink to see through.
func insideCeiling(ceiling, p string) bool {
	ceiling = filepath.Clean(ceiling)
	p = filepath.Clean(p)
	if ceiling == p {
		return true
	}
	rel, err := filepath.Rel(ceiling, p)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolate replaces ${VAR} with the environment value, falling back to
// fileEnv, and errors when a referenced variable is set in neither so a
// missing key fails at load rather than at the first API call.
func interpolate(src string, fileEnv map[string]string) (string, error) {
	var missing []string
	out := envRef.ReplaceAllStringFunc(src, func(m string) string {
		name := envRef.FindStringSubmatch(m)[1]
		v, ok := os.LookupEnv(name)
		if !ok {
			v, ok = fileEnv[name]
		}
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

// readEnvFile parses the env file that sits beside config.toml, so ${VAR}
// resolves even when spore is started from a shell that never sourced it
// (a non-interactive shell skips .bashrc). It accepts the sourceable subset:
// blank lines, # comments, and KEY=VALUE with an optional "export " prefix
// and optional matching quotes. The values feed interpolation only; they are
// not put into the process environment, so MCP children cannot inherit them.
// A missing file is not an error.
func readEnvFile(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is beside the config file
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	vars := map[string]string{}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		name, val, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !envName.MatchString(name) {
			return nil, fmt.Errorf("env file %s line %d: want KEY=VALUE", path, i+1)
		}
		val = strings.TrimSpace(val)
		if n := len(val); n >= 2 && (val[0] == '"' || val[0] == '\'') && val[n-1] == val[0] {
			val = val[1 : n-1]
		}
		vars[name] = val
	}
	return vars, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	fileEnv, err := readEnvFile(filepath.Join(filepath.Dir(path), "env"))
	if err != nil {
		return nil, err
	}
	body, err := interpolate(string(raw), fileEnv)
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
	if cfg.Context.MaxOutputTokens == 0 {
		cfg.Context.MaxOutputTokens = Default().Context.MaxOutputTokens
	}
	if cfg.Context.CompactAt == 0 {
		cfg.Context.CompactAt = Default().Context.CompactAt
	}
	if cfg.Context.KeepRecent == 0 {
		cfg.Context.KeepRecent = Default().Context.KeepRecent
	}
	if cfg.Context.SkillBudget == 0 {
		cfg.Context.SkillBudget = Default().Context.SkillBudget
	}
	if cfg.Skills.Scope == "" {
		cfg.Skills.Scope = Default().Skills.Scope
	}
	if expanded, err := expandHome(cfg.Skills.Dir); err == nil {
		cfg.Skills.Dir = expanded
	}
	d := Default()
	fillKernelDefaults(&cfg.Kernel, d.Kernel)
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
	// The MCP path table is derived, never decoded: it is filled here, after
	// policy.workspace has been expanded, because a stdio server resolves a
	// relative path against the ceiling it was started in.
	cfg.Policy.MCPPaths = map[string]MCPPathMode{}
	for _, s := range cfg.MCP.Servers {
		mode := MCPPathMode{Checked: s.PathsChecked()}
		if s.Transport == "stdio" {
			mode.Cwd = cfg.Policy.Workspace
		}
		cfg.Policy.MCPPaths[s.Name] = mode
	}
	if cfg.Policy.Profiles == nil {
		cfg.Policy.Profiles = map[string]ProfilePolicy{}
	}
	// A profile workspace is expanded and bounded at load, so an operator
	// learns about a bad one at startup rather than when a bridge user's
	// first tool call is refused.
	for name, p := range cfg.Policy.Profiles {
		if p.Workspace == "" {
			continue
		}
		expanded, err := expandHome(p.Workspace)
		if err != nil {
			return nil, fmt.Errorf("policy.profile.%s.workspace %q: %w", name, p.Workspace, err)
		}
		if !insideCeiling(cfg.Policy.Workspace, expanded) {
			return nil, fmt.Errorf("policy.profile.%s.workspace %s is outside policy.workspace %s",
				name, expanded, cfg.Policy.Workspace)
		}
		p.Workspace = expanded
		cfg.Policy.Profiles[name] = p
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
	if cfg.Recall.Backend == "" {
		cfg.Recall.Backend = d.Recall.Backend
	}
	if cfg.Daemon.TickSeconds == 0 {
		cfg.Daemon.TickSeconds = d.Daemon.TickSeconds
	}
	// Zero means "not set in the file", not "disabled": a partially written
	// [subagents] block must not silently refuse every spawn.
	if cfg.Subagents.MaxDepth == 0 {
		cfg.Subagents.MaxDepth = 2
	}
	if cfg.Subagents.MaxCostUSD == 0 {
		cfg.Subagents.MaxCostUSD = 1.00
	}
	if cfg.Subagents.MaxConcurrent == 0 {
		cfg.Subagents.MaxConcurrent = 4
	}
	// Zero means "not set in the file", as for [subagents].
	if cfg.Refine.IdleMinutes == 0 {
		cfg.Refine.IdleMinutes = 10
	}
	if cfg.Refine.MaxEdits == 0 {
		cfg.Refine.MaxEdits = 5
	}
	if err := validateDiscord(cfg.Bridge.Discord); err != nil {
		return nil, err
	}
	if err := validateMCP(cfg.MCP); err != nil {
		return nil, err
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
	if c.Context.MaxRoundTrips < 0 {
		return fmt.Errorf("context.max_round_trips must not be negative (0 means no cap)")
	}
	if c.Context.MaxOutputTokens < 0 {
		return fmt.Errorf("context.max_output_tokens must not be negative")
	}
	if c.Context.MaxOutputTokens >= c.Context.MaxTokens {
		return fmt.Errorf("context.max_output_tokens (%d) must be less than context.max_tokens (%d): the reply has to fit in the context window",
			c.Context.MaxOutputTokens, c.Context.MaxTokens)
	}
	if c.Context.FactBudget < 0 {
		return fmt.Errorf("context.fact_budget must not be negative")
	}
	if c.Context.SkillBudget < 0 {
		return fmt.Errorf("context.skill_budget must not be negative")
	}
	switch c.Skills.Scope {
	case "", SkillsGlobal, SkillsWorkspace:
	default:
		return fmt.Errorf("skills.scope must be %s or %s, got %q", SkillsGlobal, SkillsWorkspace, c.Skills.Scope)
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
	switch c.Recall.Backend {
	case RecallSQLiteFTS, RecallWeaviate:
	default:
		return fmt.Errorf("recall.backend must be %s or %s, got %q",
			RecallSQLiteFTS, RecallWeaviate, c.Recall.Backend)
	}
	if c.Recall.URL != "" {
		if _, err := url.Parse(c.Recall.URL); err != nil {
			return fmt.Errorf("recall.url: %w", err)
		}
	}
	switch c.Kernel.Mode {
	case KernelModeCode, KernelModeTools:
	default:
		return fmt.Errorf("kernel.mode must be %s or %s, got %q", KernelModeCode, KernelModeTools, c.Kernel.Mode)
	}
	if c.Kernel.TimeoutSeconds < 0 || c.Kernel.MaxTimeoutSeconds < 0 || c.Kernel.CeilingSeconds < 0 || c.Kernel.HelperMaxBytes < 0 {
		return fmt.Errorf("kernel: timeouts and helper_max_bytes must not be negative")
	}
	if c.Subagents.MaxDepth < 0 || c.Subagents.MaxCostUSD < 0 || c.Subagents.MaxConcurrent < 0 {
		return fmt.Errorf("subagents: max_depth, max_cost_usd and max_concurrent must not be negative")
	}
	if c.Refine.IdleMinutes < 0 || c.Refine.MaxEdits < 0 {
		return fmt.Errorf("refine: idle_minutes and max_edits must not be negative")
	}
	return nil
}

// WeaviateURL is the address the backend dials. The default is the loopback
// address `spore recall setup` binds, so a machine that ran setup needs no
// configuration at all.
func (c *Config) WeaviateURL() string {
	if c.Recall.URL != "" {
		return c.Recall.URL
	}
	return "http://127.0.0.1:8080"
}

// DBPath is the SQLite file backing every session.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "spore.db") }

// MemoryDir is where the fact files live: one markdown file per fact, owned
// by the files rather than the database.
func (c *Config) MemoryDir() string { return filepath.Join(c.DataDir, "memory") }

// SoulPath is the global personality file. It sits beside the other data
// files because it is the user's, not any one workspace's.
func (c *Config) SoulPath() string { return filepath.Join(c.DataDir, "soul.md") }

// AgentPath is the standing instructions for one workspace, and is empty when
// the session has none.
//
// It is deliberately not governed by Skills.Scope. Skills have a scope
// setting because a skill is a library that might reasonably be shared or
// kept globally; standing instructions for this project are meaningless
// anywhere else, so there is no second place this could live and no setting
// to get wrong.
func (c *Config) AgentPath(workspace string) string {
	if workspace == "" {
		return ""
	}
	return filepath.Join(workspace, ".spore", "agent.md")
}

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
