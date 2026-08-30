# spore Plan 2 — Tools and Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the agent hands and a leash — a tool registry with `fs`, `shell` and `web` builtins, and a policy engine that resolves every call to allow/ask/deny on the tool *and its arguments*, with approvals that suspend a turn, persist to SQLite, and survive a restart.

**Architecture:** Three new packages sit under the `agent.ToolRunner` seam Plan 1 left open. `internal/tool` is a dumb registry: it holds `Tool` implementations, publishes their specs, dispatches a call, recovers panics and truncates output. `internal/policy` wraps that registry in a `Guard` that evaluates each call against ordered rules before letting it through — deny first and absolute, then allow, then ask (which suspends the turn on a persisted `pending_calls` row and waits for an `Approver`). The agent core is untouched apart from one tracing fix; `cmd/spore` supplies the terminal `Approver`. Rule evaluation and path confinement are pure functions over strings, so the security-critical half of this plan tests offline with no filesystem and no model.

**Tech Stack:** Go 1.26.4, stdlib only for policy, tools and the registry, plus `golang.org/x/net/html` (already an indirect dependency, promoted to direct) for fetch-to-markdown. `net/http/httptest` for the web builtins. No new third-party dependencies.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` (sections 6, 7, 9, 10; staging item 2 in section 11)

## Global Constraints

- Module path is `github.com/codered/spore`. Go directive `go 1.26`.
- **Every** `go build`, `go test`, and `go vet` invocation passes `-tags sqlite_fts5`. Use `make build`, `make test`, `make vet`.
- The agent core imports no transport package. `internal/agent` must not import `net/http`, `internal/daemon`, or any bridge. `internal/policy` and `internal/tool` must not import `internal/agent` — they satisfy `agent.ToolRunner` structurally, not by importing it. This is spec invariant 1 and a reviewer gate on every task.
- **Deny is checked before everything else and is absolute.** No approval, learned rule, or trust profile may override a deny match. This is the barrier against prompt injection; a change that lets deny be outranked is a P0 review failure.
- Every LLM call names a call site from the fixed set: `chat`, `compaction`, `title`, `classify`. Plan 2 adds no new call sites.
- **Wire tool names use underscores, policy rules accept either.** Anthropic and OpenAI both constrain tool names to `[a-zA-Z0-9_-]{1,64}`, so a tool's `Name()` is `fs_read`, never `fs.read`. The spec writes rules as `fs.read`; the rule matcher normalises `.` to `_` on both sides so both spellings work and the spec's config samples stay valid.
- No live network calls in tests. The web builtins are tested against `httptest` servers; filesystem tools against `t.TempDir()`.
- Data lives under `~/.spore` by default. Tests always override this to a `t.TempDir()`.
- Timestamps are stored as RFC3339 UTC strings via the existing `store.timeFormat`.
- Commit after every task. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `chore:`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | **Modify** — add `PolicyConfig`, `WebConfig`, `ShellConfig`, defaults and validation |
| `internal/config/write.go` | **Create** — render and splice the spore-managed policy block back into `config.toml` |
| `internal/config/write_test.go` | **Create** — write-back round-trip, marker handling, hand-edits preserved |
| `internal/policy/rule.go` | **Create** — rule grammar: parse `tool(predicate)` into a `Rule`, tool globs, path globs |
| `internal/policy/rule_test.go` | **Create** — table tests for the grammar and both glob matchers |
| `internal/policy/path.go` | **Create** — `Resolve`/`Inside`: `~` expansion, `../` cleaning, symlink resolution, workspace confinement |
| `internal/policy/path_test.go` | **Create** — the adversarial path suite |
| `internal/policy/engine.go` | **Create** — ordered evaluation, deny-first, learned rules, trust profiles |
| `internal/policy/engine_test.go` | **Create** — precedence, profiles, defaults |
| `internal/policy/guard.go` | **Create** — `Guard`: evaluate → allow/deny/ask, approval, audit, trace attributes |
| `internal/policy/guard_test.go` | **Create** — allow/deny/ask paths, remembered decisions, timeout-denies |
| `internal/policy/resume_test.go` | **Create** — approval survives a simulated process restart |
| `internal/tool/tool.go` | **Create** — `Tool` interface, `Registry`, specs, truncation, panic recovery |
| `internal/tool/tool_test.go` | **Create** — dispatch, unknown tool, truncation marker, panic → tool error |
| `internal/tool/fs/fs.go` | **Create** — `fs_read`, `fs_write`, `fs_edit`, `fs_list`, `fs_glob`, `fs_grep` |
| `internal/tool/fs/fs_test.go` | **Create** — per-operation tests against a temp workspace |
| `internal/tool/shell/shell.go` | **Create** — `shell_exec` with timeout and output cap |
| `internal/tool/shell/shell_test.go` | **Create** — exit codes, timeout kill, output cap |
| `internal/tool/web/search.go` | **Create** — `web_search` behind a `SearchProvider` interface; Brave implementation |
| `internal/tool/web/fetch.go` | **Create** — `web_fetch`, HTML → markdown-ish text |
| `internal/tool/web/web_test.go` | **Create** — httptest fixtures for both |
| `internal/store/schema.go` | **Modify** — add `pending_calls`; extend `approvals` usage |
| `internal/store/store.go` | **Modify** — pending-call and approval-decision CRUD |
| `internal/store/store_test.go` | **Modify** — round-trip tests for the new tables |
| `internal/trace/trace.go` | **Modify** — `RecordPolicy`, `RecordToolResult` span attributes |
| `internal/agent/agent.go` | **Modify** — thread the tool span's context into `Tools.Run` (one-line seam fix) |
| `cmd/spore/wire.go` | **Modify** — build the registry, the engine and the guard |
| `cmd/spore/approve.go` | **Create** — terminal `Approver` (once / deny / session / pattern) |
| `cmd/spore/policy.go` | **Create** — `spore policy check <tool> <json>` |
| `cmd/spore/main.go` | **Modify** — register the `policy` subcommand |
| `README.md` | **Modify** — document `[policy]`, `[web]`, the tools, and the approval verbs |

---

### Task 1: Policy configuration surface and the rule grammar

Everything downstream matches rules, so the grammar is written and locked first. A rule is a tool glob with an optional argument predicate: `fs.write`, `web.*`, `mcp__*`, `fs.*(path outside workspace)`, `fs.*(path matches **/.env, **/.ssh/**)`, `shell.exec(matches rm -rf /, sudo)`.

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Create: `internal/policy/rule.go`, `internal/policy/rule_test.go`

**Interfaces:**
- Consumes: `config.Config` (Plan 1 Task 1).
- Produces:
  - `config.PolicyConfig{Workspace, Default, ApprovalTimeout string, MaxOutput int, Allow, Ask, Deny []string, Learned config.LearnedPolicy, Profiles map[string]config.ProfilePolicy}`, reachable as `cfg.Policy`
  - `config.LearnedPolicy{Allow, Ask, Deny []string}`, `config.ProfilePolicy{Default string, Allow, Ask, Deny []string}`
  - `config.WebConfig{SearchProvider, BraveAPIKey, UserAgent string}` as `cfg.Web`; `config.ShellConfig{TimeoutSeconds int}` as `cfg.Shell`
  - `policy.Decision` (`DecisionAllow`, `DecisionAsk`, `DecisionDeny`), `policy.Profile` (`ProfileLocal`, `ProfileRemote`)
  - `policy.Call{Tool string, Args json.RawMessage}`
  - `policy.Rule{Decision Decision, Raw string}` with `(Rule).Match(c Call, env Env) bool`
  - `policy.ParseRule(d Decision, src string) (Rule, error)`
  - `policy.Env{Workspace string}`

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`:

```go
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
```

Add `"strings"` to that file's imports if it is not already there.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run Policy -v`
Expected: FAIL — `cfg.Policy undefined`.

- [ ] **Step 3: Extend the config**

In `internal/config/config.go`, add the fields to `Config` (after `Context`):

```go
	Policy PolicyConfig `toml:"policy"`
	Web    WebConfig    `toml:"web"`
	Shell  ShellConfig  `toml:"shell"`
```

Add the new types and their defaults:

```go
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
```

In `Default()`, add:

```go
		Policy: PolicyConfig{
			Workspace:       home,
			Default:         "ask",
			ApprovalTimeout: "5m",
			MaxOutput:       30_000,
			Allow:           []string{"fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"},
			Ask:             []string{"fs_write", "fs_edit", "shell_exec", "mcp__*"},
			Profiles:        map[string]ProfilePolicy{},
		},
		Web:   WebConfig{SearchProvider: "brave", UserAgent: "spore/0.1"},
		Shell: ShellConfig{TimeoutSeconds: 120},
```

In `Load`, after the existing `Context` fallbacks and before `Validate`, apply the policy fallbacks and merge the baseline:

```go
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
```

Add the helper and the validation:

```go
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
```

In `Validate()`, before the final `return nil`:

```go
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
```

Add `"time"` to the imports.

- [ ] **Step 4: Run the config tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS (all existing tests plus the two new ones).

- [ ] **Step 5: Write the failing rule-grammar test**

Create `internal/policy/rule_test.go`:

```go
package policy

import (
	"encoding/json"
	"testing"
)

func call(tool string, args string) Call {
	return Call{Tool: tool, Args: json.RawMessage(args)}
}

func mustRule(t *testing.T, d Decision, src string) Rule {
	t.Helper()
	r, err := ParseRule(d, src)
	if err != nil {
		t.Fatalf("ParseRule(%q): %v", src, err)
	}
	return r
}

func TestToolGlobMatching(t *testing.T) {
	env := Env{Workspace: "/ws"}
	cases := []struct {
		rule string
		tool string
		want bool
	}{
		{"fs_read", "fs_read", true},
		{"fs_read", "fs_write", false},
		// The spec writes rules with dots; wire names use underscores. Both
		// spellings must match the same tool.
		{"fs.read", "fs_read", true},
		{"fs.*", "fs_write", true},
		{"fs_*", "fs_read", true},
		{"web.*", "web_search", true},
		{"web.*", "fs_read", false},
		{"mcp__*", "mcp__github__list_prs", true},
		{"mcp__*", "fs_read", false},
		{"*", "anything_at_all", true},
	}
	for _, c := range cases {
		r := mustRule(t, DecisionAllow, c.rule)
		if got := r.Match(call(c.tool, `{}`), env); got != c.want {
			t.Errorf("rule %q vs tool %q = %v, want %v", c.rule, c.tool, got, c.want)
		}
	}
}

func TestPathMatchesPredicate(t *testing.T) {
	env := Env{Workspace: "/ws"}
	r := mustRule(t, DecisionDeny, "fs_*(path matches **/.env, **/.ssh/**, **/*_rsa)")
	cases := []struct {
		path string
		want bool
	}{
		{"/ws/.env", true},
		{"/ws/a/b/.env", true},
		{".env", true},
		{"/ws/.envrc", false},
		{"/home/u/.ssh/id_ed25519", true},
		{"/home/u/.ssh/nested/key", true},
		{"/ws/keys/deploy_rsa", true},
		{"/ws/main.go", false},
	}
	for _, c := range cases {
		args, _ := json.Marshal(map[string]string{"path": c.path})
		if got := r.Match(Call{Tool: "fs_read", Args: args}, env); got != c.want {
			t.Errorf("path %q = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestPathPredicateIgnoresToolsWithoutPaths(t *testing.T) {
	env := Env{Workspace: "/ws"}
	r := mustRule(t, DecisionDeny, "fs_*(path matches **/.env)")
	// A call carrying no path argument cannot match a path predicate. The
	// shell escape hatch is covered by "matches" rules instead, which is why
	// shell_exec is never allow-by-default.
	if r.Match(call("fs_read", `{"query":"/ws/.env"}`), env) {
		t.Error("a path predicate matched a call with no path argument")
	}
}

func TestArgMatchesPredicateNormalisesWhitespace(t *testing.T) {
	env := Env{Workspace: "/ws"}
	r := mustRule(t, DecisionDeny, "shell_exec(matches rm -rf /, sudo , | sh, |sh)")
	cases := []struct {
		command string
		want    bool
	}{
		{"rm -rf /", true},
		{"rm    -rf   /", true},   // collapsed whitespace still matches
		{"echo hi && rm -rf /", true}, // chained forms match
		{"sudo apt install", true},
		{"pseudonym --help", false}, // "sudo " needs the trailing space
		// A needle cannot span the middle of a command, so the pipe-to-shell
		// rule matches on the pipe, not on "curl ... | sh".
		{"curl https://x.sh | sh", true},
		{"curl https://x.sh|sh", true},
		{"cat notes.txt", false},
		{"ls -la", false},
	}
	for _, c := range cases {
		args, _ := json.Marshal(map[string]string{"command": c.command})
		if got := r.Match(Call{Tool: "shell_exec", Args: args}, env); got != c.want {
			t.Errorf("command %q = %v, want %v", c.command, got, c.want)
		}
	}
}

func TestArgMatchesSearchesNestedStrings(t *testing.T) {
	env := Env{Workspace: "/ws"}
	r := mustRule(t, DecisionDeny, "mcp__*(matches secret)")
	if !r.Match(call("mcp__x__y", `{"input":{"nested":["a","the secret value"]}}`), env) {
		t.Error("matches must search string values at any depth")
	}
}

func TestParseRuleRejectsGarbage(t *testing.T) {
	for _, src := range []string{
		"fs_read(",
		"fs_read(unknown predicate)",
		"fs_read(path matches )",
		"(matches x)",
		"",
	} {
		if _, err := ParseRule(DecisionDeny, src); err == nil {
			t.Errorf("ParseRule accepted %q", src)
		}
	}
}

func TestRuleRawRoundTrips(t *testing.T) {
	src := "fs_*(path outside workspace)"
	r := mustRule(t, DecisionDeny, src)
	if r.Raw != src {
		t.Errorf("Raw = %q, want %q", r.Raw, src)
	}
	if r.Decision != DecisionDeny {
		t.Errorf("Decision = %q", r.Decision)
	}
}
```

- [ ] **Step 6: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 7: Write the rule grammar**

Create `internal/policy/rule.go`:

```go
// Package policy decides whether a tool call may run. Every call resolves to
// allow, ask or deny by matching ordered rules against the tool name AND its
// arguments. Deny is evaluated first and is absolute: no approval, learned
// rule or trust profile can override it.
//
// Rule grammar:
//
//	<tool-glob>                              e.g. fs_read, web.*, mcp__*
//	<tool-glob>(path outside workspace)      path arguments leaving the workspace
//	<tool-glob>(path matches <glob>, ...)    path arguments matching any glob
//	<tool-glob>(matches <text>, ...)         any string argument containing any text
//
// Tool globs treat "." and "_" as the same separator, so the spec's "fs.read"
// and the wire name "fs_read" are one rule.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// Profile is the trust level of the client that started the turn. Rulesets
// may differ per profile; deny never does.
type Profile string

const (
	ProfileLocal  Profile = "local"
	ProfileRemote Profile = "remote"
)

// Call is one tool invocation under evaluation.
type Call struct {
	Tool string
	Args json.RawMessage
}

// Env is the evaluation environment shared by every rule.
type Env struct{ Workspace string }

// Rule is one parsed policy line.
type Rule struct {
	Decision Decision
	// Raw is the rule exactly as written in config, used in audit records,
	// spans and the message the model sees when a call is denied.
	Raw string

	tool *regexp.Regexp
	pred predicate
}

type predicate interface {
	match(c Call, env Env) bool
}

// ParseRule compiles one rule string under the given decision.
func ParseRule(d Decision, src string) (Rule, error) {
	raw := strings.TrimSpace(src)
	if raw == "" {
		return Rule{}, fmt.Errorf("empty policy rule")
	}
	toolSrc, predSrc := raw, ""
	if i := strings.Index(raw, "("); i >= 0 {
		if !strings.HasSuffix(raw, ")") {
			return Rule{}, fmt.Errorf("policy rule %q: unbalanced parentheses", raw)
		}
		toolSrc = strings.TrimSpace(raw[:i])
		predSrc = strings.TrimSpace(raw[i+1 : len(raw)-1])
	}
	if toolSrc == "" {
		return Rule{}, fmt.Errorf("policy rule %q: missing tool name", raw)
	}
	re, err := compileToolGlob(toolSrc)
	if err != nil {
		return Rule{}, fmt.Errorf("policy rule %q: %w", raw, err)
	}
	r := Rule{Decision: d, Raw: raw, tool: re}
	if predSrc != "" {
		p, err := parsePredicate(predSrc)
		if err != nil {
			return Rule{}, fmt.Errorf("policy rule %q: %w", raw, err)
		}
		r.pred = p
	}
	return r, nil
}

// Match reports whether the rule applies to this call.
func (r Rule) Match(c Call, env Env) bool {
	if !r.tool.MatchString(normaliseToolName(c.Tool)) {
		return false
	}
	if r.pred == nil {
		return true
	}
	return r.pred.match(c, env)
}

func normaliseToolName(s string) string { return strings.ReplaceAll(s, ".", "_") }

// compileToolGlob turns a tool glob into an anchored regexp. "*" matches any
// run of characters; there is no path structure in a tool name.
func compileToolGlob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	for _, ch := range normaliseToolName(g) {
		if ch == '*' {
			b.WriteString(`.*`)
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(ch)))
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// compilePathGlob understands "**" (any number of segments), "*" (within one
// segment) and "?". A leading "**/" also matches a bare filename, so
// "**/.env" matches ".env".
func compilePathGlob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
					continue
				}
				b.WriteString(`.*`)
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parsePredicate(src string) (predicate, error) {
	switch {
	case src == "path outside workspace":
		return outsideWorkspace{}, nil
	case strings.HasPrefix(src, "path matches "):
		globs := splitList(strings.TrimPrefix(src, "path matches "))
		if len(globs) == 0 {
			return nil, fmt.Errorf("predicate %q: no globs listed", src)
		}
		var res []*regexp.Regexp
		for _, g := range globs {
			re, err := compilePathGlob(g)
			if err != nil {
				return nil, fmt.Errorf("predicate %q: bad glob %q: %w", src, g, err)
			}
			res = append(res, re)
		}
		return pathMatches{res}, nil
	case strings.HasPrefix(src, "matches "):
		needles := splitList(strings.TrimPrefix(src, "matches "))
		if len(needles) == 0 {
			return nil, fmt.Errorf("predicate %q: nothing to match", src)
		}
		for i, n := range needles {
			needles[i] = normaliseSpace(n)
		}
		return argMatches{needles}, nil
	default:
		return nil, fmt.Errorf("unknown predicate %q (want \"path outside workspace\", \"path matches ...\" or \"matches ...\")", src)
	}
}

var spaceRun = regexp.MustCompile(`\s+`)

// normaliseSpace collapses runs of whitespace so "rm    -rf /" matches a rule
// written "rm -rf /". Quoting and chaining are handled by searching every
// string argument for the needle rather than parsing the shell.
func normaliseSpace(s string) string { return spaceRun.ReplaceAllString(s, " ") }

// pathArgKeys are the argument names a path predicate inspects. A tool that
// takes a path must name it one of these.
var pathArgKeys = map[string]bool{"path": true, "paths": true, "dir": true, "file": true}

// argPaths returns every path-shaped argument value in the call.
func argPaths(c Call) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(c.Args, &m); err != nil {
		return nil
	}
	var out []string
	for k, raw := range m {
		if !pathArgKeys[k] {
			continue
		}
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			out = append(out, one)
			continue
		}
		var many []string
		if err := json.Unmarshal(raw, &many); err == nil {
			out = append(out, many...)
		}
	}
	return out
}

// argStrings returns every string value in the arguments, at any depth.
func argStrings(c Call) []string {
	var v any
	if err := json.Unmarshal(c.Args, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

type outsideWorkspace struct{}

func (outsideWorkspace) match(c Call, env Env) bool {
	paths := argPaths(c)
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !Inside(env.Workspace, p) {
			return true
		}
	}
	return false
}

type pathMatches struct{ globs []*regexp.Regexp }

func (p pathMatches) match(c Call, env Env) bool {
	for _, raw := range argPaths(c) {
		candidates := []string{raw}
		if resolved, err := Resolve(env.Workspace, raw); err == nil {
			candidates = append(candidates, resolved)
		}
		for _, cand := range candidates {
			for _, re := range p.globs {
				if re.MatchString(cand) {
					return true
				}
			}
		}
	}
	return false
}

type argMatches struct{ needles []string }

func (a argMatches) match(c Call, _ Env) bool {
	for _, s := range argStrings(c) {
		s = normaliseSpace(s)
		for _, n := range a.needles {
			if strings.Contains(s, n) {
				return true
			}
		}
	}
	return false
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/policy/ ./internal/config/ -v`
Expected: the rule tests fail to build — `Resolve` and `Inside` are written in Task 2. Add a temporary stub at the bottom of `rule.go` so the package compiles, and delete it in Task 2 Step 3:

```go
// Temporary: replaced by internal/policy/path.go in Task 2.
func Resolve(workspace, p string) (string, error) { return p, nil }
func Inside(workspace, p string) bool             { return strings.HasPrefix(p, workspace) }
```

Re-run. Expected: PASS (config tests plus 7 policy rule tests).

- [ ] **Step 9: Commit**

```bash
git add internal/config internal/policy
git commit -m "feat(policy): configuration surface and rule grammar"
```

---

### Task 2: Path resolution and workspace confinement

The `path outside workspace` predicate is the single most security-critical function in spore. It gets its own task and its own adversarial suite.

**Files:**
- Create: `internal/policy/path.go`, `internal/policy/path_test.go`
- Modify: `internal/policy/rule.go` (delete the temporary stub)

**Interfaces:**
- Consumes: nothing beyond stdlib.
- Produces: `policy.Resolve(workspace, p string) (string, error)` and `policy.Inside(workspace, p string) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/path_test.go`:

```go
package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// realTemp resolves the temp dir through symlinks so comparisons are stable
// on macOS, where /tmp itself is a link.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInsideAcceptsPathsWithinWorkspace(t *testing.T) {
	ws := realTemp(t)
	if err := os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		ws,
		filepath.Join(ws, "a"),
		filepath.Join(ws, "a", "b", "new-file.go"), // need not exist yet
		"a/b/new-file.go",                          // relative resolves against the workspace
		filepath.Join(ws, "a", "..", "a", "b"),     // harmless traversal that stays inside
	} {
		if !Inside(ws, p) {
			t.Errorf("Inside(%q) = false, want true", p)
		}
	}
}

func TestInsideRejectsTraversal(t *testing.T) {
	ws := realTemp(t)
	for _, p := range []string{
		"..",
		"../outside.txt",
		"a/../../outside.txt",
		filepath.Join(ws, "..", "sibling", "secret"),
		"/etc/passwd",
		filepath.Dir(ws), // the parent is not inside
	} {
		if Inside(ws, p) {
			t.Errorf("Inside(%q) = true, want false", p)
		}
	}
}

func TestInsideRejectsSymlinkEscape(t *testing.T) {
	root := realTemp(t)
	ws := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A link inside the workspace pointing at a file outside it.
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if Inside(ws, link) {
		t.Error("a symlink out of the workspace was reported inside")
	}
	// A link to a directory outside, with a path continuing through it.
	dirLink := filepath.Join(ws, "elsewhere")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if Inside(ws, filepath.Join(dirLink, "secret.txt")) {
		t.Error("a path through a symlinked directory escaped the workspace")
	}
	if Inside(ws, filepath.Join(dirLink, "does-not-exist-yet.txt")) {
		t.Error("a not-yet-existing path through a symlinked directory escaped")
	}
}

func TestInsideRejectsPrefixLookalike(t *testing.T) {
	root := realTemp(t)
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// "/root/ws-evil" shares a string prefix with "/root/ws" but is not inside
	// it. A naive strings.HasPrefix check fails this test.
	if Inside(ws, filepath.Join(root, "ws-evil", "file")) {
		t.Error("a sibling directory sharing a name prefix was reported inside")
	}
}

func TestResolveExpandsHomeAndRelative(t *testing.T) {
	ws := realTemp(t)
	got, err := Resolve(ws, "sub/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, "sub", "file.go"); got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err = Resolve(ws, "~/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "x.txt"); got != want {
		t.Errorf("Resolve(~) = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run Inside -v`
Expected: FAIL — the stub `Inside` reports the symlink escape and the prefix lookalike as inside.

- [ ] **Step 3: Write the implementation**

Delete the temporary `Resolve`/`Inside` stub from `internal/policy/rule.go` and create `internal/policy/path.go`:

```go
package policy

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve turns a tool-supplied path into an absolute, symlink-resolved path.
// It expands a leading "~", resolves relative paths against the workspace,
// cleans "..", and follows symlinks on the longest existing prefix so a link
// cannot hide an escape behind a name that looks local. A path that does not
// exist yet still resolves: its existing ancestor is resolved and the
// remaining names are appended.
func Resolve(workspace, p string) (string, error) {
	if p == "" {
		return "", os.ErrInvalid
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	return resolveExisting(filepath.Clean(p)), nil
}

// resolveExisting walks up until a path component exists, resolves that
// prefix through symlinks, and rejoins the tail.
func resolveExisting(p string) string {
	tail := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without finding anything that exists
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// Inside reports whether p resolves to the workspace itself or something
// beneath it. Comparison is on path boundaries, so "/ws-evil" is not inside
// "/ws".
func Inside(workspace, p string) bool {
	ws, err := Resolve(workspace, workspace)
	if err != nil {
		return false
	}
	abs, err := Resolve(ws, p)
	if err != nil {
		return false
	}
	if abs == ws {
		return true
	}
	rel, err := filepath.Rel(ws, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -v`
Expected: PASS (5 path tests plus the 7 rule tests from Task 1).

- [ ] **Step 5: Commit**

```bash
git add internal/policy
git commit -m "feat(policy): workspace confinement with symlink-aware path resolution"
```

---

### Task 3: The policy engine

**Files:**
- Create: `internal/policy/engine.go`, `internal/policy/engine_test.go`

**Interfaces:**
- Consumes: `policy.Rule`, `policy.ParseRule`, `policy.Env` (Task 1); `config.PolicyConfig` (Task 1).
- Produces:
  - `policy.Result{Decision Decision, Rule string}`
  - `policy.NewEngine(cfg config.PolicyConfig) (*Engine, error)`
  - `(*Engine).Evaluate(profile Profile, c Call) Result`
  - `(*Engine).Workspace() string`
  - `(*Engine).ApprovalTimeout() time.Duration`

- [ ] **Step 1: Write the failing test**

Create `internal/policy/engine_test.go`:

```go
package policy

import (
	"encoding/json"
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
	got := e.Evaluate(ProfileLocal, Call{Tool: "fs_read", Args: json.RawMessage(`{not json`)})
	if got.Decision != DecisionDeny {
		t.Errorf("Decision = %q, want deny for unparseable arguments", got.Decision)
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run Engine -v`
Expected: FAIL — `undefined: NewEngine`.

- [ ] **Step 3: Write the implementation**

Create `internal/policy/engine.go`:

```go
package policy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/codered/spore/internal/config"
)

// Result is one policy decision, carrying the rule that produced it so the
// audit log, the span and the model's error message all name the same thing.
type Result struct {
	Decision Decision
	Rule     string
}

// ruleset is the ordered evaluation list for one trust profile. deny is held
// separately because it is evaluated first and cannot be overridden.
type ruleset struct {
	deny        []Rule
	allowAndAsk []Rule
	fallback    Decision
}

type Engine struct {
	env      Env
	base     ruleset
	profiles map[Profile]ruleset
	timeout  time.Duration
}

func parseAll(d Decision, srcs []string) ([]Rule, error) {
	var out []Rule
	for _, s := range srcs {
		r, err := ParseRule(d, s)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// NewEngine compiles every configured rule up front, so a typo in policy is a
// startup error rather than a surprise at the first tool call.
func NewEngine(cfg config.PolicyConfig) (*Engine, error) {
	timeout, err := time.ParseDuration(cfg.ApprovalTimeout)
	if err != nil {
		return nil, fmt.Errorf("policy.approval_timeout %q: %w", cfg.ApprovalTimeout, err)
	}
	base, err := buildRuleset(cfg.Default, cfg.Allow, cfg.Ask, cfg.Deny, cfg.Learned)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		env:      Env{Workspace: cfg.Workspace},
		base:     base,
		profiles: map[Profile]ruleset{},
		timeout:  timeout,
	}
	for name, p := range cfg.Profiles {
		def := p.Default
		if def == "" {
			def = cfg.Default
		}
		// Deny is global: a profile's own deny rules extend the base set,
		// they never replace it.
		deny := append(append([]string{}, cfg.Deny...), p.Deny...)
		rs, err := buildRuleset(def, p.Allow, p.Ask, deny, cfg.Learned)
		if err != nil {
			return nil, fmt.Errorf("policy.profile.%s: %w", name, err)
		}
		e.profiles[Profile(name)] = rs
	}
	return e, nil
}

func buildRuleset(def string, allow, ask, deny []string, learned config.LearnedPolicy) (ruleset, error) {
	var rs ruleset
	denyRules, err := parseAll(DecisionDeny, append(append([]string{}, deny...), learned.Deny...))
	if err != nil {
		return ruleset{}, err
	}
	rs.deny = denyRules

	allowRules, err := parseAll(DecisionAllow, allow)
	if err != nil {
		return ruleset{}, err
	}
	askRules, err := parseAll(DecisionAsk, ask)
	if err != nil {
		return ruleset{}, err
	}
	learnedAllow, err := parseAll(DecisionAllow, learned.Allow)
	if err != nil {
		return ruleset{}, err
	}
	learnedAsk, err := parseAll(DecisionAsk, learned.Ask)
	if err != nil {
		return ruleset{}, err
	}
	// Hand-written rules are evaluated before learned ones, so a rule the
	// user typed always outranks one an approval prompt wrote.
	rs.allowAndAsk = append(rs.allowAndAsk, allowRules...)
	rs.allowAndAsk = append(rs.allowAndAsk, askRules...)
	rs.allowAndAsk = append(rs.allowAndAsk, learnedAllow...)
	rs.allowAndAsk = append(rs.allowAndAsk, learnedAsk...)

	switch def {
	case "allow":
		rs.fallback = DecisionAllow
	case "deny":
		rs.fallback = DecisionDeny
	default:
		rs.fallback = DecisionAsk
	}
	return rs, nil
}

func (e *Engine) Workspace() string             { return e.env.Workspace }
func (e *Engine) ApprovalTimeout() time.Duration { return e.timeout }

// Evaluate resolves one call. Deny rules are checked first and win outright;
// then allow and ask rules in configured order; then the profile default.
func (e *Engine) Evaluate(profile Profile, c Call) Result {
	// Arguments that do not parse cannot be checked against argument
	// predicates, so a call carrying them is refused outright rather than
	// matched against tool-name-only rules.
	if !json.Valid(c.Args) {
		return Result{Decision: DecisionDeny, Rule: "policy.malformed-arguments"}
	}
	rs, ok := e.profiles[profile]
	if !ok {
		rs = e.base
	}
	for _, r := range rs.deny {
		if r.Match(c, e.env) {
			return Result{Decision: DecisionDeny, Rule: r.Raw}
		}
	}
	for _, r := range rs.allowAndAsk {
		if r.Match(c, e.env) {
			return Result{Decision: r.Decision, Rule: r.Raw}
		}
	}
	return Result{Decision: rs.fallback, Rule: "policy.default"}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -v`
Expected: PASS (10 engine tests plus the rule and path tests).

- [ ] **Step 5: Commit**

```bash
git add internal/policy
git commit -m "feat(policy): ordered evaluation engine with absolute deny and trust profiles"
```

---

### Task 4: Tool interface and registry

The registry is deliberately dumb: it dispatches, recovers panics and truncates. Policy lives in the `Guard` that wraps it (Task 9), so the two can be reviewed and tested apart.

**Files:**
- Create: `internal/tool/tool.go`, `internal/tool/tool_test.go`

**Interfaces:**
- Consumes: `provider.Block`, `provider.ToolSpec` (Plan 1 Task 3).
- Produces:
  - `tool.Tool` interface: `Name() string`, `Description() string`, `Schema() json.RawMessage`, `ReadOnly() bool`, `Call(ctx context.Context, args json.RawMessage) (string, error)`
  - `tool.NewRegistry(maxOutput int) *Registry`
  - `(*Registry).Register(t Tool) error`, `(*Registry).Specs() []provider.ToolSpec`, `(*Registry).ReadOnly(name string) bool`, `(*Registry).Run(ctx context.Context, call provider.Block) provider.Block`, `(*Registry).Names() []string`
  - `tool.Result(id, content string, isErr, truncated bool) provider.Block` — the one place a `tool_result` block is built
  - `tool.ErrResult(id string, err error) provider.Block`

The registry satisfies `agent.ToolRunner` structurally without importing `internal/agent`.

- [ ] **Step 1: Write the failing test**

Create `internal/tool/tool_test.go`:

```go
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

type fake struct {
	name     string
	readOnly bool
	fn       func(ctx context.Context, args json.RawMessage) (string, error)
}

func (f fake) Name() string                { return f.name }
func (f fake) Description() string         { return "fake " + f.name }
func (f fake) Schema() json.RawMessage     { return json.RawMessage(`{"type":"object"}`) }
func (f fake) ReadOnly() bool              { return f.readOnly }
func (f fake) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return f.fn(ctx, args)
}

func echoTool(name string, readOnly bool) fake {
	return fake{name: name, readOnly: readOnly, fn: func(_ context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	}}
}

func call(name, id, args string) provider.Block {
	return provider.Block{Type: provider.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(args)}
}

func TestRunDispatchesAndTagsResult(t *testing.T) {
	r := NewRegistry(100)
	if err := r.Register(echoTool("fs_read", true)); err != nil {
		t.Fatal(err)
	}
	got := r.Run(context.Background(), call("fs_read", "call-1", `{"path":"x"}`))
	if got.Type != provider.BlockToolResult {
		t.Errorf("Type = %q, want tool_result", got.Type)
	}
	if got.ID != "call-1" {
		t.Errorf("ID = %q, want the tool_use id echoed back", got.ID)
	}
	if got.Content != `{"path":"x"}` || got.IsError {
		t.Errorf("Content = %q IsError = %v", got.Content, got.IsError)
	}
}

func TestUnknownToolIsAToolErrorNotACrash(t *testing.T) {
	r := NewRegistry(100)
	got := r.Run(context.Background(), call("nope", "call-1", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "nope") {
		t.Errorf("got %+v, want an error result naming the tool", got)
	}
}

func TestToolErrorIsReportedToTheModel(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(fake{name: "boom", fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("disk on fire")
	}})
	got := r.Run(context.Background(), call("boom", "c", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "disk on fire") {
		t.Errorf("got %+v, want the error text returned as a tool error", got)
	}
}

func TestPanicIsRecoveredAsAToolError(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(fake{name: "panicky", fn: func(context.Context, json.RawMessage) (string, error) {
		panic("nil map write")
	}})
	got := r.Run(context.Background(), call("panicky", "c", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "nil map write") {
		t.Errorf("got %+v, want the panic recovered into a tool error", got)
	}
}

func TestOutputIsTruncatedAndMarked(t *testing.T) {
	r := NewRegistry(20)
	_ = r.Register(fake{name: "big", readOnly: true, fn: func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("x", 500), nil
	}})
	got := r.Run(context.Background(), call("big", "c", `{}`))
	if !got.Truncated {
		t.Error("Truncated = false, want the result marked as clipped")
	}
	if len(got.Content) > 20+len(truncationNote) {
		t.Errorf("content is %d bytes, want it capped near 20", len(got.Content))
	}
	if !strings.Contains(got.Content, "truncated") {
		t.Error("the model must be able to see the output was clipped, not empty")
	}
}

func TestSpecsAndReadOnly(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(echoTool("fs_read", true))
	_ = r.Register(echoTool("fs_write", false))
	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("len(Specs) = %d, want 2", len(specs))
	}
	// Specs are sorted so the prompt prefix stays byte-identical between
	// turns, which keeps provider prompt caching effective.
	if specs[0].Name != "fs_read" || specs[1].Name != "fs_write" {
		t.Errorf("Specs are not sorted by name: %v, %v", specs[0].Name, specs[1].Name)
	}
	if !r.ReadOnly("fs_read") || r.ReadOnly("fs_write") {
		t.Error("ReadOnly is wrong")
	}
	// An unknown tool must never be reported read-only: that would let it
	// join a concurrent batch.
	if r.ReadOnly("unknown") {
		t.Error("ReadOnly(unknown) = true, want false")
	}
}

func TestRegisterRejectsDuplicatesAndBadNames(t *testing.T) {
	r := NewRegistry(100)
	if err := r.Register(echoTool("fs_read", true)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(echoTool("fs_read", true)); err == nil {
		t.Error("Register accepted a duplicate name")
	}
	// Providers constrain tool names to [a-zA-Z0-9_-]{1,64}; a dotted name
	// would be rejected upstream at request time.
	if err := r.Register(echoTool("fs.read", true)); err == nil {
		t.Error("Register accepted a name providers will reject")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/tool/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/tool/tool.go`:

```go
// Package tool holds the tool registry and the shape every builtin
// implements. The registry dispatches, recovers panics and truncates; it
// makes no policy decisions — internal/policy.Guard wraps it for that.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"

	"github.com/codered/spore/internal/provider"
)

// Tool is one callable operation. Name must be wire-safe for both the
// Anthropic and OpenAI tool schemas.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for the tool's arguments object.
	Schema() json.RawMessage
	// ReadOnly reports whether the tool mutates anything. Read-only tools
	// may be dispatched concurrently within one assistant message.
	ReadOnly() bool
	// Call runs the tool. A returned error becomes a tool error the model
	// can read and route around; it never fails the turn.
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// nameRE is the intersection of the Anthropic and OpenAI tool-name rules.
var nameRE = regexp.MustCompile(`\A[a-zA-Z0-9_-]{1,64}\z`)

const truncationNote = "\n\n[truncated: output exceeded the tool output budget]"

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// maxOutput caps one result in bytes before truncation.
	maxOutput int
}

func NewRegistry(maxOutput int) *Registry {
	if maxOutput <= 0 {
		maxOutput = 30_000
	}
	return &Registry{tools: map[string]Tool{}, maxOutput: maxOutput}
}

func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if !nameRE.MatchString(name) {
		return fmt.Errorf("tool name %q must match %s", name, nameRE)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool %q is already registered", name)
	}
	r.tools[name] = t
	return nil
}

// Specs returns every tool's schema, sorted by name so the serialised prompt
// prefix is stable between turns and stays cacheable upstream.
func (r *Registry) Specs() []provider.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]provider.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReadOnly reports false for tools it does not know, so an unknown name can
// never join a concurrent batch.
func (r *Registry) ReadOnly(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return ok && t.ReadOnly()
}

// Result builds a tool_result block. Every result in spore is built here so
// the truncation marker and error flag are set in exactly one place.
func Result(id, content string, isErr, truncated bool) provider.Block {
	return provider.Block{
		Type:      provider.BlockToolResult,
		ID:        id,
		Content:   content,
		IsError:   isErr,
		Truncated: truncated,
	}
}

func ErrResult(id string, err error) provider.Block {
	return Result(id, err.Error(), true, false)
}

// Run dispatches one call. It never returns an error: a failure the model
// should see is returned as an error result so the agent can pick another
// path instead of losing the turn.
func (r *Registry) Run(ctx context.Context, call provider.Block) (out provider.Block) {
	r.mu.RLock()
	t, ok := r.tools[call.Name]
	r.mu.RUnlock()
	if !ok {
		return ErrResult(call.ID, fmt.Errorf("no tool named %q is registered", call.Name))
	}

	defer func() {
		if rec := recover(); rec != nil {
			out = ErrResult(call.ID, fmt.Errorf("tool %s panicked: %v", call.Name, rec))
		}
	}()

	content, err := t.Call(ctx, call.Input)
	if err != nil {
		return ErrResult(call.ID, fmt.Errorf("tool %s: %w", call.Name, err))
	}
	if len(content) > r.maxOutput {
		return Result(call.ID, content[:r.maxOutput]+truncationNote, false, true)
	}
	return Result(call.ID, content, false, false)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/tool/ -v`
Expected: PASS (7 tests).

- [ ] **Step 5: Verify the registry satisfies the agent's seam**

Create `internal/tool/seam_test.go`:

```go
package tool_test

import (
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/tool"
)

// The registry must satisfy agent.ToolRunner without internal/tool importing
// internal/agent — the assertion lives in an external test package so the
// dependency stays one-directional.
func TestRegistrySatisfiesToolRunner(t *testing.T) {
	var _ agent.ToolRunner = tool.NewRegistry(100)
}
```

Run: `go test -tags sqlite_fts5 ./internal/tool/ -v`
Expected: PASS (8 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/tool
git commit -m "feat(tool): registry with truncation, panic recovery and stable specs"
```

---

### Task 5: The `fs` builtins

Six tools, one file. They do **no** confinement of their own — that is the policy engine's job, and duplicating it would create two places for the rule to drift. They do share one thing with policy: `policy.Resolve`, so a relative path means the same thing to the tool that runs it and the rule that judged it.

**Files:**
- Create: `internal/tool/fs/fs.go`, `internal/tool/fs/fs_test.go`

**Interfaces:**
- Consumes: `tool.Tool` (Task 4), `policy.Resolve` (Task 2).
- Produces: `fs.New(workspace string, maxBytes int) []tool.Tool` returning `fs_read`, `fs_write`, `fs_edit`, `fs_list`, `fs_glob`, `fs_grep`.

Argument schemas (the `path` key name matters — `internal/policy` inspects `path`, `paths`, `dir` and `file`):

| Tool | Arguments | Read-only |
|---|---|---|
| `fs_read` | `path` (required), `offset`, `limit` (lines) | yes |
| `fs_write` | `path`, `content` (both required) | no |
| `fs_edit` | `path`, `old`, `new` (required), `replace_all` | no |
| `fs_list` | `path` (defaults to the workspace) | yes |
| `fs_glob` | `pattern` (required), `path` | yes |
| `fs_grep` | `pattern` (required, RE2), `path`, `glob` | yes |

- [ ] **Step 1: Write the failing test**

Create `internal/tool/fs/fs_test.go`:

```go
package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/tool"
)

func tools(t *testing.T) (map[string]tool.Tool, string) {
	t.Helper()
	ws := t.TempDir()
	m := map[string]tool.Tool{}
	for _, tl := range New(ws, 1<<20) {
		m[tl.Name()] = tl
	}
	return m, ws
}

func run(t *testing.T, tl tool.Tool, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.Call(context.Background(), raw)
	if err != nil {
		t.Fatalf("%s: %v", tl.Name(), err)
	}
	return out
}

func runErr(t *testing.T, tl tool.Tool, args any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	_, err := tl.Call(context.Background(), raw)
	return err
}

func TestReadWriteRoundTrip(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], map[string]string{"path": "a/b/hello.txt", "content": "hi\nthere\n"})
	onDisk, err := os.ReadFile(filepath.Join(ws, "a", "b", "hello.txt"))
	if err != nil {
		t.Fatalf("fs_write did not create parent directories: %v", err)
	}
	if string(onDisk) != "hi\nthere\n" {
		t.Errorf("on disk = %q", onDisk)
	}
	if got := run(t, m["fs_read"], map[string]string{"path": "a/b/hello.txt"}); !strings.Contains(got, "there") {
		t.Errorf("fs_read = %q", got)
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	m, _ := tools(t)
	run(t, m["fs_write"], map[string]string{"path": "n.txt", "content": "1\n2\n3\n4\n5\n"})
	got := run(t, m["fs_read"], map[string]any{"path": "n.txt", "offset": 2, "limit": 2})
	if strings.Contains(got, "1") || !strings.Contains(got, "2") || !strings.Contains(got, "3") || strings.Contains(got, "4") {
		t.Errorf("offset/limit window wrong: %q", got)
	}
}

func TestReadMissingFileIsAnError(t *testing.T) {
	m, _ := tools(t)
	if err := runErr(t, m["fs_read"], map[string]string{"path": "nope.txt"}); err == nil {
		t.Error("reading a missing file must be a tool error")
	}
}

func TestEditReplacesOnceAndRequiresUniqueness(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], map[string]string{"path": "e.txt", "content": "a\nb\na\n"})
	// An ambiguous edit must fail rather than guess which occurrence to take.
	if err := runErr(t, m["fs_edit"], map[string]string{"path": "e.txt", "old": "a", "new": "z"}); err == nil {
		t.Error("an edit matching twice must be refused")
	}
	run(t, m["fs_edit"], map[string]any{"path": "e.txt", "old": "a", "new": "z", "replace_all": true})
	got, _ := os.ReadFile(filepath.Join(ws, "e.txt"))
	if string(got) != "z\nb\nz\n" {
		t.Errorf("replace_all = %q", got)
	}
	if err := runErr(t, m["fs_edit"], map[string]string{"path": "e.txt", "old": "absent", "new": "x"}); err == nil {
		t.Error("an edit matching nothing must be refused")
	}
}

func TestListAndGlob(t *testing.T) {
	m, _ := tools(t)
	run(t, m["fs_write"], map[string]string{"path": "src/one.go", "content": "package x"})
	run(t, m["fs_write"], map[string]string{"path": "src/two.md", "content": "# x"})
	listing := run(t, m["fs_list"], map[string]string{"path": "src"})
	if !strings.Contains(listing, "one.go") || !strings.Contains(listing, "two.md") {
		t.Errorf("fs_list = %q", listing)
	}
	globbed := run(t, m["fs_glob"], map[string]string{"pattern": "**/*.go"})
	if !strings.Contains(globbed, "one.go") || strings.Contains(globbed, "two.md") {
		t.Errorf("fs_glob = %q", globbed)
	}
}

func TestGrepReportsFileAndLine(t *testing.T) {
	m, _ := tools(t)
	run(t, m["fs_write"], map[string]string{"path": "src/a.go", "content": "package x\nfunc Target() {}\n"})
	run(t, m["fs_write"], map[string]string{"path": "src/b.md", "content": "Target in markdown\n"})
	got := run(t, m["fs_grep"], map[string]string{"pattern": "func Target", "path": "src"})
	if !strings.Contains(got, "a.go:2") {
		t.Errorf("fs_grep = %q, want a file:line hit", got)
	}
	filtered := run(t, m["fs_grep"], map[string]string{"pattern": "Target", "glob": "**/*.md"})
	if strings.Contains(filtered, "a.go") || !strings.Contains(filtered, "b.md") {
		t.Errorf("glob filter ignored: %q", filtered)
	}
	if err := runErr(t, m["fs_grep"], map[string]string{"pattern": "("}); err == nil {
		t.Error("an invalid regexp must be a tool error, not a panic")
	}
}

func TestEmptyResultsSaySoExplicitly(t *testing.T) {
	m, _ := tools(t)
	// "no matches" must be distinguishable from a clipped or failed call.
	if got := run(t, m["fs_grep"], map[string]string{"pattern": "zzz"}); !strings.Contains(got, "no matches") {
		t.Errorf("fs_grep = %q, want an explicit empty-result message", got)
	}
	if got := run(t, m["fs_glob"], map[string]string{"pattern": "*.nothing"}); !strings.Contains(got, "no matches") {
		t.Errorf("fs_glob = %q, want an explicit empty-result message", got)
	}
}

func TestReadOnlyFlags(t *testing.T) {
	m, _ := tools(t)
	for name, want := range map[string]bool{
		"fs_read": true, "fs_list": true, "fs_glob": true, "fs_grep": true,
		"fs_write": false, "fs_edit": false,
	} {
		if m[name].ReadOnly() != want {
			t.Errorf("%s.ReadOnly() = %v, want %v", name, !want, want)
		}
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	m, _ := tools(t)
	for name, tl := range m {
		if !json.Valid(tl.Schema()) {
			t.Errorf("%s has an invalid schema", name)
		}
		if tl.Description() == "" {
			t.Errorf("%s has no description", name)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/tool/fs/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Export the glob compiler from the policy package**

The filesystem tools must glob with exactly the semantics policy matches with, so the compiler is shared rather than reimplemented. In `internal/policy/rule.go`, split the existing `compilePathGlob` so the source string is reusable, and export it:

```go
// GlobSource turns a path glob into an anchored regexp source. It is exported
// so the filesystem tools search with exactly the semantics policy matches
// with: "**" spans directories, "*" stays within one segment.
func GlobSource(g string) string {
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
					continue
				}
				b.WriteString(`.*`)
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString(`\z`)
	return b.String()
}

func compilePathGlob(g string) (*regexp.Regexp, error) { return regexp.Compile(GlobSource(g)) }
```

Delete the old body of `compilePathGlob`.

- [ ] **Step 4: Write the implementation**

Create `internal/tool/fs/fs.go`:

```go
// Package fs implements spore's filesystem builtins. These tools do no
// confinement of their own: internal/policy decides which paths are legal,
// and both use policy.Resolve so a relative path means the same thing to the
// rule that judged the call and the tool that runs it.
package fs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	// aliased: this package is itself named fs.
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

const noMatches = "no matches"

// New builds the six filesystem tools bound to a workspace. maxBytes caps a
// single file read before the registry's own output budget applies.
func New(workspace string, maxBytes int) []tool.Tool {
	b := base{ws: workspace, maxBytes: maxBytes}
	return []tool.Tool{
		readTool{b}, writeTool{b}, editTool{b},
		listTool{b}, globTool{b}, grepTool{b},
	}
}

type base struct {
	ws       string
	maxBytes int
}

func (b base) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	return policy.Resolve(b.ws, p)
}

// rel renders a path for the model relative to the workspace when possible,
// so transcripts stay short and stable.
func (b base) rel(p string) string {
	if r, err := filepath.Rel(b.ws, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// ---- fs_read ----

type readTool struct{ base }

func (readTool) Name() string { return "fs_read" }
func (readTool) Description() string {
	return "Read a text file. Returns numbered lines. Use offset and limit to page through a large file."
}
func (readTool) ReadOnly() bool { return true }
func (readTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"offset":{"type":"integer","description":"1-based first line to return."},
"limit":{"type":"integer","description":"Maximum number of lines to return."}},
"required":["path"]}`)
}

func (t readTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(raw) > t.maxBytes {
		raw = raw[:t.maxBytes]
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if b.Len() == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

// ---- fs_write ----

type writeTool struct{ base }

func (writeTool) Name() string { return "fs_write" }
func (writeTool) Description() string {
	return "Write a file, creating parent directories and overwriting any existing content."
}
func (writeTool) ReadOnly() bool { return false }
func (writeTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"content":{"type":"string","description":"Full new contents of the file."}},
"required":["path","content"]}`)
}

func (t writeTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Path, Content string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), t.rel(p)), nil
}

// ---- fs_edit ----

type editTool struct{ base }

func (editTool) Name() string { return "fs_edit" }
func (editTool) Description() string {
	return "Replace an exact string in a file. Fails unless the string appears exactly once, unless replace_all is set."
}
func (editTool) ReadOnly() bool { return false }
func (editTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"old":{"type":"string","description":"Exact text to replace, including indentation."},
"new":{"type":"string","description":"Replacement text."},
"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring exactly one."}},
"required":["path","old","new"]}`)
}

func (t editTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old"`
		New        string `json:"new"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Old == "" {
		return "", fmt.Errorf("old must not be empty")
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	body := string(raw)
	n := strings.Count(body, a.Old)
	switch {
	case n == 0:
		return "", fmt.Errorf("%s: the text to replace was not found", t.rel(p))
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("%s: the text to replace appears %d times; pass more surrounding context or set replace_all", t.rel(p), n)
	}
	if a.ReplaceAll {
		body = strings.ReplaceAll(body, a.Old, a.New)
	} else {
		body = strings.Replace(body, a.Old, a.New, 1)
	}
	info, err := os.Stat(p)
	mode := iofs.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", n, t.rel(p)), nil
}

// ---- fs_list ----

type listTool struct{ base }

func (listTool) Name() string        { return "fs_list" }
func (listTool) Description() string { return "List the entries of a directory." }
func (listTool) ReadOnly() bool      { return true }
func (listTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"Directory path. Defaults to the workspace root."}}}`)
}

func (t listTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	var b strings.Builder
	for _, e := range entries {
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		fmt.Fprintf(&b, "%s%s\n", e.Name(), suffix)
	}
	return b.String(), nil
}

// ---- shared walking ----

// walkFiles visits every regular file under root, skipping the noise
// directories that would otherwise dominate every result.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true}

func walkFiles(root string, visit func(path string) error) error {
	return filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is skipped, not fatal
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return visit(p)
	})
}

// ---- fs_glob ----

type globTool struct{ base }

func (globTool) Name() string { return "fs_glob" }
func (globTool) Description() string {
	return "Find files by glob pattern. Supports ** for any number of directories."
}
func (globTool) ReadOnly() bool { return true }
func (globTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"pattern":{"type":"string","description":"Glob such as **/*.go."},
"path":{"type":"string","description":"Directory to search. Defaults to the workspace root."}},
"required":["pattern"]}`)
}

// globRE reuses the policy path-glob semantics so a pattern means the same
// thing in a rule and in a search.
func globRE(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(policy.GlobSource(pattern))
}

func (t globTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Pattern, Path string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	re, err := globRE(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}
	root, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	var hits []string
	err = walkFiles(root, func(p string) error {
		r := t.rel(p)
		if re.MatchString(r) || re.MatchString(filepath.Base(p)) {
			hits = append(hits, r)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return noMatches, nil
	}
	sort.Strings(hits)
	return strings.Join(hits, "\n"), nil
}

// ---- fs_grep ----

type grepTool struct{ base }

func (grepTool) Name() string { return "fs_grep" }
func (grepTool) Description() string {
	return "Search file contents with a regular expression (RE2). Returns file:line: matched-line."
}
func (grepTool) ReadOnly() bool { return true }
func (grepTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"pattern":{"type":"string","description":"RE2 regular expression."},
"path":{"type":"string","description":"Directory to search. Defaults to the workspace root."},
"glob":{"type":"string","description":"Only search files whose path matches this glob."}},
"required":["pattern"]}`)
}

const maxGrepHits = 200

func (t grepTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Pattern, Path, Glob string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}
	var filter *regexp.Regexp
	if a.Glob != "" {
		filter, err = globRE(a.Glob)
		if err != nil {
			return "", fmt.Errorf("invalid glob %q: %w", a.Glob, err)
		}
	}
	root, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	var hits []string
	err = walkFiles(root, func(p string) error {
		r := t.rel(p)
		if filter != nil && !filter.MatchString(r) && !filter.MatchString(filepath.Base(p)) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for line := 1; sc.Scan(); line++ {
			if len(hits) >= maxGrepHits {
				return filepath.SkipAll
			}
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", r, line, strings.TrimSpace(sc.Text())))
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return noMatches, nil
	}
	out := strings.Join(hits, "\n")
	if len(hits) >= maxGrepHits {
		out += fmt.Sprintf("\n\n[stopped after %d matches; narrow the pattern or the path]", maxGrepHits)
	}
	return out, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/tool/... ./internal/policy/ -v`
Expected: PASS (9 fs tests, plus the registry and policy tests still green).

- [ ] **Step 6: Commit**

```bash
git add internal/tool internal/policy
git commit -m "feat(tool): fs builtins sharing policy's path and glob semantics"
```

---

### Task 6: The `shell` builtin

**Files:**
- Create: `internal/tool/shell/shell.go`, `internal/tool/shell/shell_test.go`

**Interfaces:**
- Consumes: `tool.Tool` (Task 4).
- Produces: `shell.New(workspace string, defaultTimeout time.Duration) tool.Tool` returning the `shell_exec` tool.

- [ ] **Step 1: Write the failing test**

Create `internal/tool/shell/shell_test.go`:

```go
package shell

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func call(t *testing.T, tl interface {
	Call(context.Context, json.RawMessage) (string, error)
}, args any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return tl.Call(context.Background(), raw)
}

func TestExecCapturesOutput(t *testing.T) {
	ws := t.TempDir()
	tl := New(ws, 5*time.Second)
	out, err := call(t, tl, map[string]string{"command": "echo hello"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("out = %q", out)
	}
}

func TestExecRunsInTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tl := New(ws, 5*time.Second)
	// Comparing pwd output would be fragile where the temp dir is reached
	// through a symlink; listing the workspace is not.
	out, err := call(t, tl, map[string]string{"command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "marker.txt") {
		t.Errorf("ls = %q, want the command to run in the workspace", out)
	}
}

func TestNonZeroExitIsReportedNotHidden(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	out, err := call(t, tl, map[string]string{"command": "echo to-stderr 1>&2; exit 3"})
	if err != nil {
		t.Fatalf("a failing command must return output, not a Go error: %v", err)
	}
	if !strings.Contains(out, "exit status 3") {
		t.Errorf("out = %q, want the exit status reported", out)
	}
	if !strings.Contains(out, "to-stderr") {
		t.Errorf("out = %q, want stderr captured", out)
	}
}

func TestTimeoutKillsTheCommand(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	start := time.Now()
	out, err := call(t, tl, map[string]any{"command": "sleep 30", "timeout_seconds": 1})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %v, want the timeout to kill it", elapsed)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("out = %q, want the timeout reported to the model", out)
	}
}

func TestEmptyCommandIsAnError(t *testing.T) {
	tl := New(t.TempDir(), 5*time.Second)
	if _, err := call(t, tl, map[string]string{"command": "  "}); err == nil {
		t.Error("an empty command must be a tool error")
	}
}

func TestShellIsNotReadOnly(t *testing.T) {
	if New(t.TempDir(), time.Second).ReadOnly() {
		t.Error("shell_exec must never be read-only: it would join concurrent batches")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/tool/shell/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write the implementation**

Create `internal/tool/shell/shell.go`:

```go
// Package shell implements the shell_exec builtin. It applies a timeout and
// reports exit status back to the model; it makes no policy decision — the
// command string is judged by internal/policy before it reaches here.
package shell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/codered/spore/internal/tool"
)

type execTool struct {
	ws             string
	defaultTimeout time.Duration
}

func New(workspace string, defaultTimeout time.Duration) tool.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	return &execTool{ws: workspace, defaultTimeout: defaultTimeout}
}

func (*execTool) Name() string { return "shell_exec" }
func (*execTool) Description() string {
	return "Run a shell command in the workspace and return its combined output. Long-running commands are killed at the timeout."
}

// ReadOnly is always false. A command's effects cannot be known from its
// text, so shell_exec never joins a concurrent batch.
func (*execTool) ReadOnly() bool { return false }

func (*execTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"command":{"type":"string","description":"Command line, interpreted by bash."},
"timeout_seconds":{"type":"integer","description":"Kill the command after this many seconds."}},
"required":["command"]}`)
}

func (t *execTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("command is required")
	}
	timeout := t.defaultTimeout
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", a.Command)
	cmd.Dir = t.ws
	// Put the child in its own process group and kill the group, so a command
	// that spawns children does not leave them running after a timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		// Returning os.ErrProcessDone tells exec the kill is the expected
		// outcome, so Run reports the deadline rather than the signal.
		return os.ErrProcessDone
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := buf.String()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return out + fmt.Sprintf("\n[timed out after %s and was killed]", timeout), nil
	case err != nil:
		// A non-zero exit is information for the model, not a tool failure.
		return out + fmt.Sprintf("\n[%v]", err), nil
	}
	if out == "" {
		return "(no output; exit status 0)", nil
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/tool/shell/ -v`
Expected: PASS (6 tests). The timeout test takes about one second.

- [ ] **Step 5: Commit**

```bash
git add internal/tool/shell
git commit -m "feat(tool): shell_exec with process-group timeout and exit reporting"
```

---

### Task 7: The `web` builtins

`web_search` sits behind a `SearchProvider` interface with Brave as the first implementation, so Tavily and DDG drop in later without touching the tool. `web_fetch` converts HTML to readable text using `golang.org/x/net/html`, already an indirect dependency.

**Files:**
- Create: `internal/tool/web/search.go`, `internal/tool/web/fetch.go`, `internal/tool/web/web_test.go`
- Modify: `go.mod` (promote `golang.org/x/net` to a direct dependency)

**Interfaces:**
- Consumes: `tool.Tool` (Task 4), `config.WebConfig` (Task 1).
- Produces:
  - `web.SearchProvider` interface: `Search(ctx context.Context, query string, count int) ([]web.Hit, error)`
  - `web.Hit{Title, URL, Snippet string}`
  - `web.NewBrave(apiKey string, hc *http.Client) *Brave` with `(*Brave).BaseURL` overridable for tests
  - `web.NewSearchTool(p SearchProvider) tool.Tool`
  - `web.NewFetchTool(hc *http.Client, userAgent string, maxBytes int) tool.Tool`
  - `web.New(cfg config.WebConfig, maxBytes int) []tool.Tool` — returns `web_fetch` always and `web_search` only when a key is configured

- [ ] **Step 1: Write the failing test**

Create `internal/tool/web/web_test.go`:

```go
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
)

const braveFixture = `{"web":{"results":[
{"title":"Go","url":"https://go.dev","description":"The Go <strong>language</strong>"},
{"title":"SQLite","url":"https://sqlite.org","description":"Embedded database"}]}}`

func TestBraveSearchParsesResults(t *testing.T) {
	var gotKey, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Subscription-Token")
		gotQuery = r.URL.Query().Get("q")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(braveFixture))
	}))
	defer srv.Close()

	b := NewBrave("test-key", srv.Client())
	b.BaseURL = srv.URL
	hits, err := b.Search(context.Background(), "go language", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotKey != "test-key" || gotQuery != "go language" {
		t.Errorf("request carried key %q query %q", gotKey, gotQuery)
	}
	if len(hits) != 2 || hits[0].URL != "https://go.dev" {
		t.Fatalf("hits = %+v", hits)
	}
	// Brave marks matched terms with HTML; the model should see plain text.
	if strings.Contains(hits[0].Snippet, "<strong>") {
		t.Errorf("snippet still contains markup: %q", hits[0].Snippet)
	}
}

func TestBraveSurfacesUpstreamErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()
	b := NewBrave("k", srv.Client())
	b.BaseURL = srv.URL
	if _, err := b.Search(context.Background(), "x", 3); err == nil {
		t.Fatal("Search swallowed a 429")
	}
}

type fakeSearch struct{ hits []Hit }

func (f fakeSearch) Search(context.Context, string, int) ([]Hit, error) { return f.hits, nil }

func TestSearchToolRendersHits(t *testing.T) {
	tl := NewSearchTool(fakeSearch{hits: []Hit{{Title: "Go", URL: "https://go.dev", Snippet: "lang"}}})
	out, err := tl.Call(context.Background(), json.RawMessage(`{"query":"go"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Go", "https://go.dev", "lang"} {
		if !strings.Contains(out, want) {
			t.Errorf("output %q is missing %q", out, want)
		}
	}
	if !tl.ReadOnly() {
		t.Error("web_search must be read-only")
	}
}

func TestSearchToolReportsNoResults(t *testing.T) {
	tl := NewSearchTool(fakeSearch{})
	out, _ := tl.Call(context.Background(), json.RawMessage(`{"query":"zzz"}`))
	if !strings.Contains(out, "no results") {
		t.Errorf("out = %q, want an explicit empty-result message", out)
	}
}

const pageFixture = `<!doctype html>
<html><head><title>A Page</title><style>body{color:red}</style><script>var x=1</script></head>
<body>
<nav>skip me</nav>
<h1>Heading</h1>
<p>First para with <a href="https://go.dev">a link</a>.</p>
<pre>code block</pre>
<ul><li>one</li><li>two</li></ul>
</body></html>`

func TestFetchConvertsHTMLToText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(pageFixture))
	}))
	defer srv.Close()

	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	out, err := tl.Call(context.Background(), args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	for _, want := range []string{"A Page", "Heading", "First para", "a link", "code block", "one", "two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"color:red", "var x=1"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("output leaked %q:\n%s", unwanted, out)
		}
	}
}

func TestFetchRejectsNonHTTPSchemes(t *testing.T) {
	tl := NewFetchTool(http.DefaultClient, "spore-test", 1<<20)
	for _, u := range []string{"file:///etc/passwd", "ftp://x/y", "notaurl"} {
		args, _ := json.Marshal(map[string]string{"url": u})
		if _, err := tl.Call(context.Background(), args); err == nil {
			t.Errorf("web_fetch accepted %q", u)
		}
	}
}

func TestFetchReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	tl := NewFetchTool(srv.Client(), "spore-test", 1<<20)
	args, _ := json.Marshal(map[string]string{"url": srv.URL})
	if _, err := tl.Call(context.Background(), args); err == nil {
		t.Fatal("web_fetch swallowed a 404")
	}
}

func TestNewOmitsSearchWithoutAKey(t *testing.T) {
	names := func(cfg config.WebConfig) []string {
		var out []string
		for _, tl := range New(cfg, 1<<20) {
			out = append(out, tl.Name())
		}
		return out
	}
	got := names(config.WebConfig{SearchProvider: "brave"})
	if len(got) != 1 || got[0] != "web_fetch" {
		t.Errorf("without a key, tools = %v, want only web_fetch", got)
	}
	got = names(config.WebConfig{SearchProvider: "brave", BraveAPIKey: "k"})
	if len(got) != 2 {
		t.Errorf("with a key, tools = %v, want both", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/tool/web/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Promote the HTML parser to a direct dependency**

```bash
go get golang.org/x/net@v0.58.0
go mod tidy
```

- [ ] **Step 4: Write the search implementation**

Create `internal/tool/web/search.go`:

```go
// Package web implements the web_search and web_fetch builtins. Search sits
// behind a provider interface so Tavily or DDG can replace Brave without the
// tool changing.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/tool"
)

type Hit struct {
	Title   string
	URL     string
	Snippet string
}

type SearchProvider interface {
	Search(ctx context.Context, query string, count int) ([]Hit, error)
}

// Brave is the first SearchProvider: a clean paid API, no scraping.
type Brave struct {
	APIKey  string
	BaseURL string
	HC      *http.Client
}

func NewBrave(apiKey string, hc *http.Client) *Brave {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Brave{APIKey: apiKey, BaseURL: "https://api.search.brave.com/res/v1/web/search", HC: hc}
}

func (b *Brave) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	if count <= 0 || count > 20 {
		count = 5
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.APIKey)

	resp, err := b.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search: %s", resp.Status)
	}
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("brave search: decode: %w", err)
	}
	hits := make([]Hit, 0, len(body.Web.Results))
	for _, r := range body.Web.Results {
		hits = append(hits, Hit{
			Title:   stripTags(r.Title),
			URL:     r.URL,
			Snippet: stripTags(r.Description),
		})
	}
	return hits, nil
}

type searchTool struct{ p SearchProvider }

// NewSearchTool wraps any SearchProvider as the web_search builtin.
func NewSearchTool(p SearchProvider) tool.Tool { return searchTool{p: p} }

func (searchTool) Name() string        { return "web_search" }
func (searchTool) Description() string { return "Search the web and return titles, URLs and snippets." }
func (searchTool) ReadOnly() bool      { return true }
func (searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"query":{"type":"string","description":"Search query."},
"count":{"type":"integer","description":"Number of results, 1-20. Defaults to 5."}},
"required":["query"]}`)
}

func (s searchTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	hits, err := s.p.Search(ctx, a.Query, a.Count)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no results", nil
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, h.Title, h.URL, h.Snippet)
	}
	return b.String(), nil
}

// New builds the web tools for a config. web_search is omitted entirely when
// no key is configured, so the model is never offered a tool that must fail.
func New(cfg config.WebConfig, maxBytes int) []tool.Tool {
	hc := &http.Client{Timeout: 30 * time.Second}
	tools := []tool.Tool{NewFetchTool(hc, cfg.UserAgent, maxBytes)}
	if cfg.SearchProvider == "brave" && cfg.BraveAPIKey != "" {
		tools = append(tools, NewSearchTool(NewBrave(cfg.BraveAPIKey, hc)))
	}
	return tools
}
```

- [ ] **Step 5: Write the fetch implementation**

Create `internal/tool/web/fetch.go`:

```go
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/codered/spore/internal/tool"
	"golang.org/x/net/html"
)

type fetchTool struct {
	hc        *http.Client
	userAgent string
	maxBytes  int
}

func NewFetchTool(hc *http.Client, userAgent string, maxBytes int) tool.Tool {
	if hc == nil {
		hc = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = "spore/0.1"
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	return fetchTool{hc: hc, userAgent: userAgent, maxBytes: maxBytes}
}

func (fetchTool) Name() string { return "web_fetch" }
func (fetchTool) Description() string {
	return "Fetch an http or https URL and return its readable text content."
}
func (fetchTool) ReadOnly() bool { return true }
func (fetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"url":{"type":"string","description":"Absolute http or https URL."}},
"required":["url"]}`)
}

func (f fetchTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", a.URL, err)
	}
	// Only http(s). file:// and friends would turn a web tool into an
	// unpoliced filesystem read.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url %q: only http and https are allowed", a.URL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("url %q: missing host", a.URL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")

	resp, err := f.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.maxBytes)))
	if err != nil {
		return "", err
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		return string(raw), nil
	}
	text, err := htmlToText(string(raw))
	if err != nil {
		return "", err
	}
	return text, nil
}

// blockTags force a line break; skipTags and their subtrees are dropped.
var (
	skipTags  = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "nav": true, "footer": true}
	blockTags = map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "tr": true, "section": true,
		"article": true, "pre": true, "blockquote": true, "table": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	}
)

// htmlToText renders a document as readable plain text: headings become
// markdown-style headings, list items get bullets, and script/style content
// is dropped. It is deliberately small — a faithful HTML-to-markdown
// converter is a dependency spore does not need to read a page.
func htmlToText(body string) (string, error) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
			return
		}
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "title":
				b.WriteString("# ")
			case len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6':
				b.WriteString("\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " ")
			case n.Data == "li":
				b.WriteString("\n- ")
			case blockTags[n.Data]:
				b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return collapseBlankLines(b.String()), nil
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// stripTags removes the markup Brave uses to highlight matched terms.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/tool/... -v`
Expected: PASS (8 web tests plus the fs, shell and registry tests).

- [ ] **Step 7: Commit**

```bash
git add internal/tool/web go.mod go.sum
git commit -m "feat(tool): web_search behind a provider interface and web_fetch to text"
```

---

### Task 8: Store — pending calls and approval decisions

Suspension is a persisted state, not an in-memory wait (spec invariant 3). The existing `approvals` table is the decision log; a new `pending_calls` table is the suspension itself.

**Files:**
- Modify: `internal/store/schema.go`, `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: `store.Store` (Plan 1 Task 2).
- Produces:
  - `store.PendingCall{ID int64, SessionID, ToolUseID, Tool, Profile, Rule string, ArgsJSON []byte, CreatedAt time.Time}`
  - `(*Store).AddPendingCall(ctx, PendingCall) (int64, error)`
  - `(*Store).PendingCalls(ctx, sessionID string) ([]PendingCall, error)`
  - `(*Store).ResolvePendingCall(ctx, id int64, decision string) error`
  - `(*Store).RecordApproval(ctx, sessionID, tool string, args []byte, decision, scope string) error`
  - `(*Store).SessionDecision(ctx, sessionID, tool string) (string, bool, error)`

- [ ] **Step 1: Write the failing test**

Append to `internal/store/store_test.go`:

```go
func TestPendingCallLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, err := s.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.AddPendingCall(ctx, PendingCall{
		SessionID: sid, ToolUseID: "call-1", Tool: "shell_exec",
		Profile: "local", Rule: "shell_exec", ArgsJSON: []byte(`{"command":"ls"}`),
	})
	if err != nil {
		t.Fatalf("AddPendingCall: %v", err)
	}
	pending, err := s.PendingCalls(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].ToolUseID != "call-1" {
		t.Fatalf("PendingCalls = %+v", pending)
	}
	if string(pending[0].ArgsJSON) != `{"command":"ls"}` {
		t.Errorf("args = %s", pending[0].ArgsJSON)
	}
	if pending[0].CreatedAt.IsZero() {
		t.Error("CreatedAt was not populated")
	}

	if err := s.ResolvePendingCall(ctx, id, "allow"); err != nil {
		t.Fatal(err)
	}
	// A resolved call is no longer pending: a restart must not re-ask.
	pending, err = s.PendingCalls(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("PendingCalls after resolve = %+v, want empty", pending)
	}
}

func TestPendingCallsAreScopedToASession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	a, _ := s.CreateSession(ctx, "a")
	b, _ := s.CreateSession(ctx, "b")
	if _, err := s.AddPendingCall(ctx, PendingCall{SessionID: a, ToolUseID: "1", Tool: "fs_write", ArgsJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.PendingCalls(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("session b sees %d pending calls from session a", len(got))
	}
}

func TestSessionDecisionRemembersAllowForTheSession(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, _ := s.CreateSession(ctx, "t")

	if _, ok, err := s.SessionDecision(ctx, sid, "fs_write"); err != nil || ok {
		t.Fatalf("SessionDecision before any answer = (ok %v, err %v), want not found", ok, err)
	}
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{"path":"x"}`), "allow", "session"); err != nil {
		t.Fatal(err)
	}
	d, ok, err := s.SessionDecision(ctx, sid, "fs_write")
	if err != nil || !ok || d != "allow" {
		t.Fatalf("SessionDecision = (%q, %v, %v), want (allow, true, nil)", d, ok, err)
	}
	// A "once" answer is audited but never remembered.
	if err := s.RecordApproval(ctx, sid, "shell_exec", []byte(`{}`), "allow", "once"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.SessionDecision(ctx, sid, "shell_exec"); ok {
		t.Error("a once-scoped answer was remembered for the session")
	}
	// A decision in one session must not leak into another.
	other, _ := s.CreateSession(ctx, "other")
	if _, ok, _ := s.SessionDecision(ctx, other, "fs_write"); ok {
		t.Error("a session-scoped decision leaked across sessions")
	}
}

func TestLatestSessionDecisionWins(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	sid, _ := s.CreateSession(ctx, "t")
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{}`), "allow", "session"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordApproval(ctx, sid, "fs_write", []byte(`{}`), "deny", "session"); err != nil {
		t.Fatal(err)
	}
	d, ok, _ := s.SessionDecision(ctx, sid, "fs_write")
	if !ok || d != "deny" {
		t.Errorf("SessionDecision = %q, want the most recent answer (deny)", d)
	}
}
```

If `openTestStore` does not already exist in `store_test.go`, add it and refactor the existing tests to use it:

```go
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run Pending -v`
Expected: FAIL — `undefined: PendingCall`.

- [ ] **Step 3: Extend the schema**

In `internal/store/schema.go`, replace the comment above `approvals` and append the new table:

```sql
-- approvals is the decision log: every answer a human gave, for audit and
-- for "always this session" lookups. scope is once | session | pattern.
CREATE TABLE IF NOT EXISTS approvals (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool       TEXT NOT NULL,
  args       TEXT NOT NULL,
  decision   TEXT NOT NULL,
  scope      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_approvals_session ON approvals(session_id, tool, id);

-- pending_calls is suspension made durable: a turn waiting on an approval
-- writes a row here before it blocks, so the process can restart mid-turn
-- and still know what it was asking about. state is pending until answered.
CREATE TABLE IF NOT EXISTS pending_calls (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool_use_id TEXT NOT NULL,
  tool        TEXT NOT NULL,
  args        TEXT NOT NULL,
  profile     TEXT NOT NULL DEFAULT '',
  rule        TEXT NOT NULL DEFAULT '',
  state       TEXT NOT NULL DEFAULT 'pending',
  created_at  TEXT NOT NULL,
  decided_at  TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_session ON pending_calls(session_id, state);
```

Keep the existing `approvals` definition rather than duplicating it — edit the comment in place and add the index and the new table after it.

- [ ] **Step 4: Write the store methods**

Append to `internal/store/store.go`:

```go
// PendingCall is a tool call whose turn is suspended awaiting approval.
type PendingCall struct {
	ID        int64
	SessionID string
	ToolUseID string
	Tool      string
	Profile   string
	Rule      string
	ArgsJSON  []byte
	CreatedAt time.Time
}

// AddPendingCall records a suspension before the turn blocks on an answer.
func (s *Store) AddPendingCall(ctx context.Context, p PendingCall) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_calls (session_id, tool_use_id, tool, args, profile, rule, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'pending', ?)`,
		p.SessionID, p.ToolUseID, p.Tool, string(p.ArgsJSON), p.Profile, p.Rule,
		time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("add pending call: %w", err)
	}
	return res.LastInsertId()
}

// PendingCalls returns the session's unanswered approvals, oldest first.
func (s *Store) PendingCalls(ctx context.Context, sessionID string) ([]PendingCall, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, tool_use_id, tool, args, profile, rule, created_at
		 FROM pending_calls WHERE session_id = ? AND state = 'pending' ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read pending calls: %w", err)
	}
	defer rows.Close()
	var out []PendingCall
	for rows.Next() {
		var p PendingCall
		var args, created string
		if err := rows.Scan(&p.ID, &p.SessionID, &p.ToolUseID, &p.Tool, &args, &p.Profile, &p.Rule, &created); err != nil {
			return nil, err
		}
		p.ArgsJSON = []byte(args)
		p.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolvePendingCall closes a suspension with the decision that ended it:
// allow, deny, or timeout.
func (s *Store) ResolvePendingCall(ctx context.Context, id int64, decision string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE pending_calls SET state = ?, decided_at = ? WHERE id = ?`,
		decision, time.Now().UTC().Format(timeFormat), id)
	if err != nil {
		return fmt.Errorf("resolve pending call %d: %w", id, err)
	}
	return nil
}

// RecordApproval appends to the audit log. Only scope "session" is consulted
// later, by SessionDecision; "once" is audit-only and "pattern" is written
// into the config file instead.
func (s *Store) RecordApproval(ctx context.Context, sessionID, tool string, args []byte, decision, scope string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO approvals (session_id, tool, args, decision, scope, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, tool, string(args), decision, scope, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("record approval: %w", err)
	}
	return nil
}

// SessionDecision returns the most recent session-scoped answer for a tool in
// this session, if any. "Always this session" is remembered per tool, not per
// argument: the user answered about a capability, not about one path.
func (s *Store) SessionDecision(ctx context.Context, sessionID, tool string) (string, bool, error) {
	var decision string
	err := s.db.QueryRowContext(ctx,
		`SELECT decision FROM approvals
		 WHERE session_id = ? AND tool = ? AND scope = 'session'
		 ORDER BY id DESC LIMIT 1`, sessionID, tool).Scan(&decision)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read session decision: %w", err)
	}
	return decision, true, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v`
Expected: PASS (the four new tests plus every existing store test).

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): persist pending tool calls and approval decisions"
```

---

### Task 9: The policy guard

The guard is where the engine, the store and the approver meet. It wraps the registry and satisfies `agent.ToolRunner` structurally, so `internal/agent` needs no knowledge of policy at all.

Session identity and trust profile travel in the context rather than on the struct, so Plan 3's daemon can share one guard across concurrent sessions.

**Files:**
- Create: `internal/policy/guard.go`, `internal/policy/guard_test.go`
- Modify: `internal/trace/trace.go`, `internal/agent/agent.go`

**Interfaces:**
- Consumes: `policy.Engine` (Task 3), `tool.Registry` (Task 4), `store.Store` (Task 8), `provider.Block` (Plan 1 Task 3).
- Produces:
  - `policy.Runner` interface (`Specs`, `ReadOnly`, `Run`) — the minimal shape the guard wraps
  - `policy.Scope` (`ScopeOnce`, `ScopeSession`, `ScopePattern`)
  - `policy.Ask{SessionID, Tool, Rule, Pattern string, Args json.RawMessage, PendingID int64}`
  - `policy.Answer{Allow bool, Scope Scope}`
  - `policy.Approver` interface: `Ask(ctx context.Context, a Ask) (Answer, error)`
  - `policy.NewGuard(inner Runner, e *Engine, ap Approver, st *store.Store, learn func(decision Decision, rule string) error) *Guard`
  - `policy.WithSession(ctx context.Context, sessionID string, p Profile) context.Context` and `policy.SessionFrom(ctx) (string, Profile)`
  - `policy.PatternFor(c Call) string` — the rule "always this pattern" would write
  - `trace.RecordPolicy(ctx context.Context, decision, rule string)`, `trace.RecordToolResult(span Span, content string, isErr, truncated bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/policy/guard_test.go`:

```go
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type recordingRunner struct {
	mu    sync.Mutex
	calls []string
}

func (r *recordingRunner) Specs() []provider.ToolSpec { return nil }
func (r *recordingRunner) ReadOnly(string) bool       { return true }
func (r *recordingRunner) Run(_ context.Context, c provider.Block) provider.Block {
	r.mu.Lock()
	r.calls = append(r.calls, c.Name)
	r.mu.Unlock()
	return provider.Block{Type: provider.BlockToolResult, ID: c.ID, Content: "ran " + c.Name}
}

type scriptedApprover struct {
	mu      sync.Mutex
	asked   []Ask
	answer  Answer
	err     error
	block   chan struct{} // when non-nil, Ask waits on it (and on ctx)
}

func (a *scriptedApprover) Ask(ctx context.Context, ask Ask) (Answer, error) {
	a.mu.Lock()
	a.asked = append(a.asked, ask)
	blocker := a.block
	a.mu.Unlock()
	if blocker != nil {
		select {
		case <-blocker:
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		}
	}
	return a.answer, a.err
}

func (a *scriptedApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.asked)
}

func guardFixture(t *testing.T, pc config.PolicyConfig, ap Approver) (*Guard, *recordingRunner, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, err := st.CreateSession(context.Background(), "guard test")
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingRunner{}
	g := NewGuard(inner, engine(t, pc), ap, st, nil)
	return g, inner, st, sid
}

func toolCall(name, id, args string) provider.Block {
	return provider.Block{Type: provider.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(args)}
}

func TestAllowRunsWithoutAsking(t *testing.T) {
	ap := &scriptedApprover{}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Allow: []string{"fs_read"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("fs_read", "c1", `{"path":"/ws/a"}`))
	if got.IsError {
		t.Fatalf("allowed call returned an error: %q", got.Content)
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want 1", len(inner.calls))
	}
	if ap.count() != 0 {
		t.Error("an allowed call must not prompt")
	}
}

func TestDenyNeverReachesTheTool(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{
		Allow: []string{"shell_exec"},
		Deny:  []string{"shell_exec(matches sudo)"},
	}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("shell_exec", "c1", `{"command":"sudo rm -rf /tmp/x"}`))
	if !got.IsError {
		t.Fatal("a denied call must return a tool error")
	}
	if !strings.Contains(got.Content, "shell_exec(matches sudo)") {
		t.Errorf("the model must be told which rule denied it: %q", got.Content)
	}
	if len(inner.calls) != 0 {
		t.Error("a denied call reached the tool")
	}
	if ap.count() != 0 {
		t.Error("a denied call must never prompt for approval")
	}
}

func TestAskPromptsAndRunsOnApproval(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	got := g.Run(ctx, toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if got.IsError {
		t.Fatalf("approved call errored: %q", got.Content)
	}
	if ap.count() != 1 || len(inner.calls) != 1 {
		t.Errorf("asked %d times, ran %d times; want 1 and 1", ap.count(), len(inner.calls))
	}
	// The suspension must be resolved, not left dangling.
	pending, err := st.PendingCalls(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("%d pending calls left behind", len(pending))
	}
}

func TestAskDeniedReportsBackToTheModel(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeOnce}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError || !strings.Contains(got.Content, "declined") {
		t.Errorf("got %+v, want a tool error saying the user declined", got)
	}
	if len(inner.calls) != 0 {
		t.Error("a declined call reached the tool")
	}
}

func TestSessionScopeAnswersOnlyOnce(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	for i := 0; i < 3; i++ {
		if got := g.Run(ctx, toolCall("fs_write", "c", `{"path":"/ws/a"}`)); got.IsError {
			t.Fatalf("call %d errored: %q", i, got.Content)
		}
	}
	if ap.count() != 1 {
		t.Errorf("asked %d times, want 1 — the session answer must be remembered", ap.count())
	}
	if len(inner.calls) != 3 {
		t.Errorf("inner ran %d times, want 3", len(inner.calls))
	}
}

func TestSessionScopeDenialIsAlsoRemembered(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: false, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	for i := 0; i < 2; i++ {
		if got := g.Run(ctx, toolCall("fs_write", "c", `{"path":"/ws/a"}`)); !got.IsError {
			t.Fatalf("call %d was allowed after a session denial", i)
		}
	}
	if ap.count() != 1 {
		t.Errorf("asked %d times, want 1", ap.count())
	}
	if len(inner.calls) != 0 {
		t.Error("a session-denied tool ran anyway")
	}
}

func TestRememberedSessionAllowStillCannotBeatDeny(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeSession}}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{
		Ask:  []string{"shell_exec"},
		Deny: []string{"shell_exec(matches sudo)"},
	}, ap)
	ctx := WithSession(context.Background(), sid, ProfileLocal)
	if got := g.Run(ctx, toolCall("shell_exec", "c1", `{"command":"ls"}`)); got.IsError {
		t.Fatalf("benign call errored: %q", got.Content)
	}
	// The session now remembers "allow shell_exec". A denied command must
	// still be refused: deny is checked before the remembered answer.
	got := g.Run(ctx, toolCall("shell_exec", "c2", `{"command":"sudo ls"}`))
	if !got.IsError {
		t.Fatal("a remembered session approval overrode a deny rule")
	}
	if len(inner.calls) != 1 {
		t.Errorf("inner ran %d times, want only the benign call", len(inner.calls))
	}
}

func TestPatternScopeLearnsARule(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, _ := st.CreateSession(context.Background(), "t")
	var learned []string
	g := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), ap, st,
		func(d Decision, rule string) error {
			learned = append(learned, string(d)+" "+rule)
			return nil
		})
	g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`))
	if len(learned) != 1 {
		t.Fatalf("learned = %v, want one rule written back", learned)
	}
	if !strings.HasPrefix(learned[0], "allow fs_write") {
		t.Errorf("learned %q, want an allow rule for fs_write", learned[0])
	}
}

func TestPatternForNarrowsToTheDirectory(t *testing.T) {
	got := PatternFor(Call{Tool: "fs_write", Args: json.RawMessage(`{"path":"/ws/src/a.go"}`)})
	if got != "fs_write(path matches /ws/src/**)" {
		t.Errorf("PatternFor = %q", got)
	}
	// With no path to generalise from, the pattern is the bare tool name.
	if got := PatternFor(Call{Tool: "shell_exec", Args: json.RawMessage(`{"command":"ls"}`)}); got != "shell_exec" {
		t.Errorf("PatternFor(shell) = %q, want the bare tool name", got)
	}
}

func TestUnansweredApprovalDeniesAtTheTimeout(t *testing.T) {
	ap := &scriptedApprover{block: make(chan struct{})} // never answered
	g, inner, st, sid := guardFixture(t, config.PolicyConfig{
		Ask:             []string{"fs_write"},
		ApprovalTimeout: "50ms",
	}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError || !strings.Contains(got.Content, "timed out") {
		t.Errorf("got %+v, want a timeout denial", got)
	}
	if len(inner.calls) != 0 {
		t.Error("a timed-out call ran anyway")
	}
	pending, _ := st.PendingCalls(context.Background(), sid)
	if len(pending) != 0 {
		t.Errorf("%d pending calls left behind after a timeout", len(pending))
	}
}

func TestApproverErrorDenies(t *testing.T) {
	ap := &scriptedApprover{err: errors.New("no tty")}
	g, inner, _, sid := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	got := g.Run(WithSession(context.Background(), sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Error("an approver failure must deny, not allow")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran despite an approver failure")
	}
}

func TestMissingSessionContextDenies(t *testing.T) {
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopeOnce}}
	g, inner, _, _ := guardFixture(t, config.PolicyConfig{Ask: []string{"fs_write"}}, ap)
	// No WithSession: the guard cannot persist a suspension, so it refuses
	// rather than running unaudited.
	got := g.Run(context.Background(), toolCall("fs_write", "c1", `{"path":"/ws/a"}`))
	if !got.IsError {
		t.Error("a call with no session context was allowed")
	}
	if len(inner.calls) != 0 {
		t.Error("the tool ran without a session")
	}
}

func TestGuardDelegatesSpecsAndReadOnly(t *testing.T) {
	ap := &scriptedApprover{}
	g, _, _, _ := guardFixture(t, config.PolicyConfig{}, ap)
	if !g.ReadOnly("anything") {
		t.Error("ReadOnly must delegate to the wrapped runner")
	}
	if g.Specs() != nil {
		t.Error("Specs must delegate to the wrapped runner")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run Guard -v`
Expected: FAIL — `undefined: NewGuard`.

- [ ] **Step 3: Add the trace helpers**

In `internal/trace/trace.go`, add the two attribute keys to the const block:

```go
	attrPolicyDecision = "spore.policy.decision"
	attrPolicyRule     = "spore.policy.rule"
	attrToolIsError    = "spore.tool.is_error"
	attrToolResultLen  = "spore.tool.result_bytes"
```

And append:

```go
// RecordPolicy annotates the current tool span with the decision that let the
// call through — or stopped it. Called from internal/policy, which has no
// other dependency on tracing.
func RecordPolicy(ctx context.Context, decision, rule string) {
	span := oteltrace.SpanFromContext(ctx)
	span.SetAttributes(
		attribute.String(attrPolicyDecision, decision),
		attribute.String(attrPolicyRule, rule),
	)
}

// RecordToolResult records the shape of a tool result. The content itself is
// dropped when redacting, but its size and error flag are always kept.
func RecordToolResult(span Span, content string, isErr, truncated bool) {
	span.SetAttributes(
		attribute.Int(attrToolResultLen, len(content)),
		attribute.Bool(attrToolIsError, isErr),
		attribute.Bool("spore.tool.truncated", truncated),
	)
	if !redact.Load() {
		span.SetAttributes(attribute.String(attrOutput, content))
	}
}
```

- [ ] **Step 4: Thread the tool span's context into the runner**

`internal/agent/agent.go` currently discards the context `StartTool` returns, so nothing below it can annotate the span. In `runTools`, change:

```go
	run := func(call provider.Block) provider.Block {
		_, span := sporetrace.StartTool(ctx, call.Name, call.Input)
		defer span.End()
		return a.Tools.Run(ctx, call)
	}
```

to:

```go
	run := func(call provider.Block) provider.Block {
		toolCtx, span := sporetrace.StartTool(ctx, call.Name, call.Input)
		defer span.End()
		res := a.Tools.Run(toolCtx, call)
		sporetrace.RecordToolResult(span, res.Content, res.IsError, res.Truncated)
		return res
	}
```

- [ ] **Step 5: Write the guard**

Create `internal/policy/guard.go`:

```go
package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// Runner is the minimal shape the guard wraps. It is declared here rather
// than imported from internal/agent so the dependency runs one way: the agent
// knows nothing about policy, and policy knows nothing about the agent.
type Runner interface {
	Specs() []provider.ToolSpec
	ReadOnly(name string) bool
	Run(ctx context.Context, call provider.Block) provider.Block
}

// Scope is how long an approval answer lasts.
type Scope string

const (
	ScopeOnce    Scope = "once"
	ScopeSession Scope = "session"
	ScopePattern Scope = "pattern"
)

// Ask is one approval request handed to a client.
type Ask struct {
	SessionID string
	Tool      string
	Args      json.RawMessage
	// Rule is the policy rule that produced the ask, shown so the human
	// answering knows why they are being asked.
	Rule string
	// PendingID is the persisted suspension this request belongs to.
	PendingID int64
	// Pattern is the rule "always allow this pattern" would write.
	Pattern string
}

type Answer struct {
	Allow bool
	Scope Scope
}

// Approver renders an approval request to a human and returns their answer.
// The CLI implements it against the terminal; Plan 3's daemon implements it
// over SSE, and Plan 4's bridge with inline keyboard buttons.
type Approver interface {
	Ask(ctx context.Context, a Ask) (Answer, error)
}

type sessionKey struct{}

type sessionInfo struct {
	id      string
	profile Profile
}

// WithSession attaches the session and trust profile a turn is running under.
// The guard reads them from the context so one guard can serve concurrent
// sessions in the daemon.
func WithSession(ctx context.Context, sessionID string, p Profile) context.Context {
	return context.WithValue(ctx, sessionKey{}, sessionInfo{id: sessionID, profile: p})
}

// SessionFrom returns the session and profile on the context. The zero
// profile is local.
func SessionFrom(ctx context.Context) (string, Profile) {
	info, _ := ctx.Value(sessionKey{}).(sessionInfo)
	if info.profile == "" {
		info.profile = ProfileLocal
	}
	return info.id, info.profile
}

// Guard evaluates every call before the wrapped runner sees it.
type Guard struct {
	inner    Runner
	engine   *Engine
	approver Approver
	store    *store.Store
	// learn persists a rule accepted with ScopePattern. Nil disables the
	// "always this pattern" answer.
	learn func(d Decision, rule string) error
}

func NewGuard(inner Runner, e *Engine, ap Approver, st *store.Store, learn func(Decision, string) error) *Guard {
	return &Guard{inner: inner, engine: e, approver: ap, store: st, learn: learn}
}

func (g *Guard) Specs() []provider.ToolSpec { return g.inner.Specs() }
func (g *Guard) ReadOnly(name string) bool  { return g.inner.ReadOnly(name) }

func denied(id, format string, args ...any) provider.Block {
	return provider.Block{
		Type:    provider.BlockToolResult,
		ID:      id,
		Content: fmt.Sprintf(format, args...),
		IsError: true,
	}
}

// Run resolves policy, suspends for approval when needed, and only then
// delegates. It never returns an error: a refusal is a tool error the model
// can read and route around.
func (g *Guard) Run(ctx context.Context, call provider.Block) provider.Block {
	c := Call{Tool: call.Name, Args: call.Input}
	sessionID, profile := SessionFrom(ctx)

	res := g.engine.Evaluate(profile, c)
	sporetrace.RecordPolicy(ctx, string(res.Decision), res.Rule)

	switch res.Decision {
	case DecisionDeny:
		// Deny is absolute and is never escalated to a human: offering an
		// approval prompt here is exactly the lever prompt injection wants.
		return denied(call.ID, "denied by policy rule %q. Do not retry this call; choose another approach.", res.Rule)
	case DecisionAllow:
		return g.inner.Run(ctx, call)
	}

	// From here the decision is ask.
	if sessionID == "" {
		return denied(call.ID, "cannot request approval: no session on the context")
	}
	if remembered, ok, err := g.store.SessionDecision(ctx, sessionID, call.Name); err == nil && ok {
		if remembered == string(DecisionAllow) {
			sporetrace.RecordPolicy(ctx, "allow", "approved earlier this session")
			return g.inner.Run(ctx, call)
		}
		sporetrace.RecordPolicy(ctx, "deny", "declined earlier this session")
		return denied(call.ID, "the user declined %s earlier in this session", call.Name)
	}

	if g.approver == nil {
		return denied(call.ID, "%s needs approval but no approver is attached", call.Name)
	}

	pattern := PatternFor(c)
	pendingID, err := g.store.AddPendingCall(ctx, store.PendingCall{
		SessionID: sessionID,
		ToolUseID: call.ID,
		Tool:      call.Name,
		ArgsJSON:  call.Input,
		Profile:   string(profile),
		Rule:      res.Rule,
	})
	if err != nil {
		return denied(call.ID, "could not record the approval request: %v", err)
	}

	// An approval nobody answers denies, so a turn started from a phone
	// cannot sit half-executed forever.
	askCtx, cancel := context.WithTimeout(ctx, g.engine.ApprovalTimeout())
	defer cancel()

	answer, err := g.approver.Ask(askCtx, Ask{
		SessionID: sessionID,
		Tool:      call.Name,
		Args:      call.Input,
		Rule:      res.Rule,
		PendingID: pendingID,
		Pattern:   pattern,
	})
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		_ = g.store.ResolvePendingCall(ctx, pendingID, "timeout")
		_ = g.store.RecordApproval(ctx, sessionID, call.Name, call.Input, "deny", "timeout")
		sporetrace.RecordPolicy(ctx, "deny", "approval timed out")
		return denied(call.ID, "approval for %s timed out after %s and was denied", call.Name, g.engine.ApprovalTimeout())
	case err != nil:
		_ = g.store.ResolvePendingCall(ctx, pendingID, "error")
		sporetrace.RecordPolicy(ctx, "deny", "approver unavailable")
		return denied(call.ID, "could not ask for approval: %v", err)
	}

	decision := DecisionDeny
	if answer.Allow {
		decision = DecisionAllow
	}
	_ = g.store.ResolvePendingCall(ctx, pendingID, string(decision))
	_ = g.store.RecordApproval(ctx, sessionID, call.Name, call.Input, string(decision), string(answer.Scope))

	if answer.Scope == ScopePattern && g.learn != nil {
		if err := g.learn(decision, pattern); err != nil {
			// Failing to persist the rule must not change this call's
			// outcome; the user simply gets asked again next time.
			sporetrace.RecordPolicy(ctx, string(decision), "learned rule not persisted: "+err.Error())
		}
	}

	if !answer.Allow {
		sporetrace.RecordPolicy(ctx, "deny", "user declined")
		return denied(call.ID, "the user declined this %s call", call.Name)
	}
	sporetrace.RecordPolicy(ctx, "allow", "user approved ("+string(answer.Scope)+")")
	return g.inner.Run(ctx, call)
}

// PatternFor proposes the rule an "always allow this pattern" answer writes.
// When the call has a path it generalises to that path's directory; otherwise
// it falls back to the bare tool name rather than guessing.
func PatternFor(c Call) string {
	paths := argPaths(c)
	if len(paths) != 1 {
		return c.Tool
	}
	dir := filepath.Dir(paths[0])
	if dir == "." || dir == string(filepath.Separator) {
		return c.Tool
	}
	return fmt.Sprintf("%s(path matches %s/**)", c.Tool, strings.TrimSuffix(dir, "/"))
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/... -v`
Expected: PASS (13 guard tests plus every earlier package).

- [ ] **Step 7: Verify the dependency direction**

Run:

```bash
go list -tags sqlite_fts5 -deps ./internal/policy | grep -E 'codered/spore/internal/(agent|daemon)' && echo "FAIL: policy must not import the agent" || echo "OK"
go list -tags sqlite_fts5 -deps ./internal/agent | grep -E 'codered/spore/internal/(policy|tool)|net/http' && echo "FAIL: the core imported a transport or policy" || echo "OK"
```

Expected: both print `OK`.

- [ ] **Step 8: Commit**

```bash
git add internal/policy internal/trace internal/agent
git commit -m "feat(policy): guard tool calls with evaluation, approval and audit"
```

---

### Task 10: Approval survives a restart

Spec invariant 3 says a suspended turn is durable. Task 9 wrote the row; this task proves a fresh process can find it and act on it, and adds the API a client needs to answer an approval it did not itself request.

**Files:**
- Create: `internal/policy/resume_test.go`
- Modify: `internal/policy/guard.go`

**Interfaces:**
- Consumes: `store.PendingCall`, `store.PendingCalls`, `store.ResolvePendingCall` (Task 8).
- Produces: `(*Guard).Pending(ctx context.Context, sessionID string) ([]store.PendingCall, error)` and `(*Guard).Resolve(ctx context.Context, sessionID string, pendingID int64, ans Answer) error`.

- [ ] **Step 1: Write the failing test**

Create `internal/policy/resume_test.go`:

```go
package policy

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// TestSuspensionSurvivesARestart simulates the daemon dying mid-approval:
// the first process records a pending call and then goes away; a second
// process, opening the same database file, finds the suspension and answers
// it. The turn's own goroutine is gone, so what must survive is the record.
func TestSuspensionSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "spore.db")

	// --- process 1: a turn suspends, then the process dies ---
	st1, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st1.CreateSession(ctx, "restart test")
	if err != nil {
		t.Fatal(err)
	}
	ap := &scriptedApprover{block: make(chan struct{})} // never answers
	g1 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{
		Ask:             []string{"fs_write"},
		ApprovalTimeout: "50ms",
	}), ap, st1, nil)
	res := g1.Run(WithSession(ctx, sid, ProfileLocal), toolCall("fs_write", "c1", `{"path":"/ws/a.go"}`))
	if !res.IsError {
		t.Fatal("the unanswered call was allowed")
	}
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// --- process 2: a fresh store over the same file ---
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	g2 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), nil, st2, nil)

	// The timed-out call is resolved, not dangling: the restarted process
	// must not re-ask about a request that already expired.
	pending, err := g2.Pending(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("timed-out suspension survived as pending: %+v", pending)
	}

	// A suspension recorded but never resolved — the shape of a hard crash —
	// is still visible and answerable after the restart.
	id, err := st2.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "c2", Tool: "fs_write",
		ArgsJSON: json.RawMessage(`{"path":"/ws/b.go"}`), Profile: "local", Rule: "fs_write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}

	st3, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	g3 := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{Ask: []string{"fs_write"}}), nil, st3, nil)

	pending, err = g3.Pending(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != id || pending[0].Tool != "fs_write" {
		t.Fatalf("Pending after restart = %+v, want the recorded suspension", pending)
	}
	if string(pending[0].ArgsJSON) != `{"path":"/ws/b.go"}` {
		t.Errorf("arguments did not survive: %s", pending[0].ArgsJSON)
	}

	// Answering it clears the suspension and records the decision, so a
	// later turn in this session is not asked again.
	if err := g3.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopeSession}); err != nil {
		t.Fatal(err)
	}
	pending, _ = g3.Pending(ctx, sid)
	if len(pending) != 0 {
		t.Errorf("Pending after Resolve = %+v, want empty", pending)
	}
	d, ok, err := st3.SessionDecision(ctx, sid, "fs_write")
	if err != nil || !ok || d != "allow" {
		t.Errorf("SessionDecision = (%q, %v, %v), want the answer remembered", d, ok, err)
	}
}

func TestResolveRejectsAForeignPendingCall(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.CreateSession(ctx, "a")
	b, _ := st.CreateSession(ctx, "b")
	id, _ := st.AddPendingCall(ctx, store.PendingCall{SessionID: a, ToolUseID: "c", Tool: "fs_write", ArgsJSON: []byte(`{}`)})

	g := NewGuard(&recordingRunner{}, engine(t, config.PolicyConfig{}), nil, st, nil)
	err = g.Resolve(ctx, b, id, Answer{Allow: true, Scope: ScopeOnce})
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Errorf("Resolve across sessions = %v, want a session-mismatch error", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run Suspension -v`
Expected: FAIL — `g2.Pending undefined`.

- [ ] **Step 3: Write the implementation**

Append to `internal/policy/guard.go`:

```go
// Pending lists the session's unanswered approval requests. A client that
// attaches to a session — after a restart, or as a second client — calls this
// to find out what is waiting on a human.
func (g *Guard) Pending(ctx context.Context, sessionID string) ([]store.PendingCall, error) {
	return g.store.PendingCalls(ctx, sessionID)
}

// Resolve answers a pending approval by id. It is the out-of-band path used
// when the answer arrives from somewhere other than the Approver that asked —
// a second client, or a process that restarted while the request was open.
// The session is checked so one session cannot answer another's approvals.
func (g *Guard) Resolve(ctx context.Context, sessionID string, pendingID int64, ans Answer) error {
	pending, err := g.store.PendingCalls(ctx, sessionID)
	if err != nil {
		return err
	}
	var found *store.PendingCall
	for i := range pending {
		if pending[i].ID == pendingID {
			found = &pending[i]
			break
		}
	}
	if found == nil {
		return fmt.Errorf("no pending call %d in session %s (already answered, or another session's)", pendingID, sessionID)
	}
	decision := DecisionDeny
	if ans.Allow {
		decision = DecisionAllow
	}
	if err := g.store.ResolvePendingCall(ctx, pendingID, string(decision)); err != nil {
		return err
	}
	if err := g.store.RecordApproval(ctx, sessionID, found.Tool, found.ArgsJSON, string(decision), string(ans.Scope)); err != nil {
		return err
	}
	if ans.Scope == ScopePattern && g.learn != nil {
		return g.learn(decision, PatternFor(Call{Tool: found.Tool, Args: found.ArgsJSON}))
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -v`
Expected: PASS (2 resume tests plus everything from Tasks 1–9).

- [ ] **Step 5: Commit**

```bash
git add internal/policy
git commit -m "feat(policy): resolve approvals out of band so suspensions survive a restart"
```

---

### Task 11: Learned rules written back to the config file

"Always allow this pattern" writes a rule into a marked section of `config.toml`, so policy stays a readable file the user can edit or revert, rather than an opaque cache (spec §6 and §9).

**Files:**
- Create: `internal/config/write.go`, `internal/config/write_test.go`

**Interfaces:**
- Consumes: `config.LearnedPolicy` (Task 1).
- Produces: `config.LearnRule(path, decision, rule string) error` and the exported marker constants `config.ManagedBegin`, `config.ManagedEnd`.

- [ ] **Step 1: Write the failing test**

Create `internal/config/write_test.go`:

```go
package config

import (
	"os"
	"strings"
	"testing"
)

func TestLearnRuleCreatesTheManagedBlock(t *testing.T) {
	p := write(t, `default_model = "anthropic/claude-opus-5"

[policy]
workspace = "/ws"
ask = ["fs_write"]
`)
	if err := LearnRule(p, "allow", `fs_write(path matches /ws/src/**)`); err != nil {
		t.Fatalf("LearnRule: %v", err)
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, ManagedBegin) || !strings.Contains(text, ManagedEnd) {
		t.Fatalf("markers missing:\n%s", text)
	}
	// The user's own configuration is untouched above the marker.
	if !strings.Contains(text, `ask = ["fs_write"]`) {
		t.Errorf("hand-written config was lost:\n%s", text)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("the rewritten file no longer parses: %v", err)
	}
	if len(cfg.Policy.Learned.Allow) != 1 || !strings.Contains(cfg.Policy.Learned.Allow[0], "/ws/src/**") {
		t.Errorf("learned rules = %+v", cfg.Policy.Learned)
	}
}

func TestLearnRuleAppendsAndDeduplicates(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n")
	for i := 0; i < 2; i++ {
		if err := LearnRule(p, "allow", "fs_write"); err != nil {
			t.Fatal(err)
		}
	}
	if err := LearnRule(p, "deny", "shell_exec"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Policy.Learned.Allow) != 1 {
		t.Errorf("allow = %v, want the duplicate collapsed", cfg.Policy.Learned.Allow)
	}
	if len(cfg.Policy.Learned.Deny) != 1 {
		t.Errorf("deny = %v", cfg.Policy.Learned.Deny)
	}
	body, _ := os.ReadFile(p)
	if strings.Count(string(body), ManagedBegin) != 1 {
		t.Errorf("the managed block was duplicated:\n%s", body)
	}
}

func TestLearnRulePreservesTextAfterTheBlock(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n"+
		ManagedBegin+"\n[policy.learned]\nallow = [\"fs_read\"]\n"+ManagedEnd+"\n\n[trace]\nenabled = true\n")
	if err := LearnRule(p, "allow", "fs_write"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace.Enabled {
		t.Error("configuration after the managed block was lost")
	}
	if len(cfg.Policy.Learned.Allow) != 2 {
		t.Errorf("allow = %v, want both the existing and the new rule", cfg.Policy.Learned.Allow)
	}
}

func TestLearnRuleRejectsAnUnparseableRule(t *testing.T) {
	p := write(t, "default_model = \"a/b\"\n")
	// A rule that cannot be quoted safely must be refused rather than
	// corrupting the file.
	if err := LearnRule(p, "allow", "fs_write(path matches \"x\")"); err == nil {
		t.Error("LearnRule accepted a rule containing a quote")
	}
	if err := LearnRule(p, "sometimes", "fs_write"); err == nil {
		t.Error("LearnRule accepted an unknown decision")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run Learn -v`
Expected: FAIL — `undefined: LearnRule`.

- [ ] **Step 3: Write the implementation**

Create `internal/config/write.go`:

```go
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// The spore-managed policy block. Rules accepted with "always this pattern"
// are written between these markers; everything outside them is the user's,
// and is preserved byte for byte.
const (
	ManagedBegin = "# >>> spore-managed policy — written by \"always allow this pattern\"; edit or delete freely"
	ManagedEnd   = "# <<< spore-managed policy"
)

// LearnRule adds one rule to the managed block of a config file, creating the
// block if it is absent. Existing learned rules are preserved and duplicates
// are collapsed.
func LearnRule(path, decision, rule string) error {
	switch decision {
	case "allow", "ask", "deny":
	default:
		return fmt.Errorf("learned rule decision must be allow, ask or deny, got %q", decision)
	}
	// The block is rendered with basic TOML strings, so a rule containing a
	// quote, a backslash or a newline is refused rather than escaped.
	if strings.ContainsAny(rule, "\"\\\n\r") {
		return fmt.Errorf("learned rule %q contains characters that cannot be written to config", rule)
	}
	if strings.TrimSpace(rule) == "" {
		return fmt.Errorf("learned rule is empty")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	body := string(raw)

	before, existing, after, found := splitManaged(body)
	learned := LearnedPolicy{}
	if found {
		// The managed block is parsed on its own: it is the only part of the
		// file spore rewrites, so a syntax error elsewhere cannot be made
		// worse by this write.
		var doc struct {
			Policy struct {
				Learned LearnedPolicy `toml:"learned"`
			} `toml:"policy"`
		}
		if _, err := toml.Decode(existing, &doc); err != nil {
			return fmt.Errorf("the spore-managed policy block is not valid TOML: %w", err)
		}
		learned = doc.Policy.Learned
	}

	switch decision {
	case "allow":
		learned.Allow = appendUnique(learned.Allow, rule)
	case "ask":
		learned.Ask = appendUnique(learned.Ask, rule)
	case "deny":
		learned.Deny = appendUnique(learned.Deny, rule)
	}

	block := renderManaged(learned)
	var out string
	if found {
		out = before + block + after
	} else {
		out = strings.TrimRight(body, "\n") + "\n\n" + block
	}

	// Write through a temp file in the same directory so an interrupted
	// write cannot leave a half-rewritten config behind.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// splitManaged returns the text before the managed block, the block's inner
// body, the text after it, and whether a block was found.
func splitManaged(body string) (before, inner, after string, found bool) {
	i := strings.Index(body, ManagedBegin)
	if i < 0 {
		return body, "", "", false
	}
	rest := body[i+len(ManagedBegin):]
	j := strings.Index(rest, ManagedEnd)
	if j < 0 {
		return body, "", "", false
	}
	return body[:i], rest[:j], rest[j+len(ManagedEnd):], true
}

func renderManaged(l LearnedPolicy) string {
	var b strings.Builder
	b.WriteString(ManagedBegin)
	b.WriteString("\n[policy.learned]\n")
	writeList := func(name string, vs []string) {
		b.WriteString(name + " = [")
		for i, v := range vs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("\"" + v + "\"")
		}
		b.WriteString("]\n")
	}
	writeList("allow", l.Allow)
	writeList("ask", l.Ask)
	writeList("deny", l.Deny)
	b.WriteString(ManagedEnd)
	b.WriteString("\n")
	return b.String()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS (4 new tests plus every existing config test).

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): write learned policy rules into a marked config block"
```

---

### Task 12: Wire it up — tools, the terminal approver, and `spore policy`

**Files:**
- Modify: `cmd/spore/wire.go`, `cmd/spore/main.go`, `README.md`
- Create: `cmd/spore/approve.go`, `cmd/spore/policy.go`, `cmd/spore/approve_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–11.
- Produces:
  - `buildTools(cfg *config.Config, st *store.Store) (*policy.Guard, error)` in `wire.go`
  - `terminalApprover{lines *bufio.Scanner, out io.Writer}` implementing `policy.Approver`
  - `stdinLines`, the process's single `*bufio.Scanner` over `os.Stdin`, shared by the chat loop and the approval prompt
  - `cmdPolicyCheck(cfg *config.Config, tool, argsJSON string) error`

- [ ] **Step 1: Write the failing approver test**

Create `cmd/spore/approve_test.go`:

```go
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/policy"
)

func ask(t *testing.T, input string) (policy.Answer, string, error) {
	t.Helper()
	var out bytes.Buffer
	ap := terminalApprover{lines: bufio.NewScanner(strings.NewReader(input)), out: &out}
	ans, err := ap.Ask(context.Background(), policy.Ask{
		SessionID: "s1",
		Tool:      "fs_write",
		Args:      json.RawMessage(`{"path":"/ws/a.go"}`),
		Rule:      "fs_write",
		Pattern:   "fs_write(path matches /ws/**)",
	})
	return ans, out.String(), err
}

func TestTerminalApproverReadsAnswers(t *testing.T) {
	cases := []struct {
		input string
		want  policy.Answer
	}{
		{"y\n", policy.Answer{Allow: true, Scope: policy.ScopeOnce}},
		{"n\n", policy.Answer{Allow: false, Scope: policy.ScopeOnce}},
		{"s\n", policy.Answer{Allow: true, Scope: policy.ScopeSession}},
		{"p\n", policy.Answer{Allow: true, Scope: policy.ScopePattern}},
	}
	for _, c := range cases {
		got, _, err := ask(t, c.input)
		if err != nil {
			t.Fatalf("input %q: %v", c.input, err)
		}
		if got != c.want {
			t.Errorf("input %q = %+v, want %+v", c.input, got, c.want)
		}
	}
}

func TestTerminalApproverShowsTheCallAndTheRule(t *testing.T) {
	_, out, err := ask(t, "y\n")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fs_write", "/ws/a.go", "fs_write(path matches /ws/**)"} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt is missing %q:\n%s", want, out)
		}
	}
}

func TestTerminalApproverDeniesOnEOF(t *testing.T) {
	// A non-interactive run (spore once in a pipeline) has no one to ask.
	// Closing input must deny, never allow.
	got, _, err := ask(t, "")
	if err == nil && got.Allow {
		t.Error("EOF was treated as approval")
	}
}

func TestTerminalApproverRepromptsOnGarbage(t *testing.T) {
	got, out, err := ask(t, "what?\nmaybe\ny\n")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Allow {
		t.Error("a valid answer after two invalid ones was not accepted")
	}
	if strings.Count(out, "[y]es") < 3 {
		t.Errorf("the prompt was not repeated for each invalid answer:\n%s", out)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -v`
Expected: FAIL — `undefined: terminalApprover`.

- [ ] **Step 3: Write the terminal approver**

Create `cmd/spore/approve.go`:

```go
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/codered/spore/internal/policy"
)

// terminalApprover asks on the terminal. It is the CLI's implementation of
// policy.Approver; the daemon and the Telegram bridge implement the same
// interface over SSE and inline keyboards in Plans 3 and 4.
type terminalApprover struct {
	// lines is the shared stdin scanner (see input.go). The chat loop reads
	// from the same one: two scanners over one file descriptor would each
	// buffer ahead and swallow the other's input.
	lines *bufio.Scanner
	out   io.Writer
}

func (t terminalApprover) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	args := string(a.Args)
	if pretty, err := json.MarshalIndent(json.RawMessage(a.Args), "  ", "  "); err == nil {
		args = string(pretty)
	}
	fmt.Fprintf(t.out, "\n\033[1mspore wants to run %s\033[0m  (matched policy rule %q)\n  %s\n", a.Tool, a.Rule, args)

	sc := t.lines
	for {
		fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)  [p]attern (always %s)\n> ",
			a.Tool, a.Pattern)
		// Honour cancellation between prompts: an approval that timed out
		// upstream should not keep a dead prompt on screen.
		select {
		case <-ctx.Done():
			return policy.Answer{}, ctx.Err()
		default:
		}
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return policy.Answer{}, err
			}
			// No terminal to ask: deny rather than assume consent.
			fmt.Fprintln(t.out, "no input available; denying")
			return policy.Answer{}, errors.New("no input available to answer the approval request")
		}
		switch strings.ToLower(strings.TrimSpace(sc.Text())) {
		case "y", "yes":
			return policy.Answer{Allow: true, Scope: policy.ScopeOnce}, nil
		case "n", "no":
			return policy.Answer{Allow: false, Scope: policy.ScopeOnce}, nil
		case "s", "session":
			return policy.Answer{Allow: true, Scope: policy.ScopeSession}, nil
		case "p", "pattern":
			return policy.Answer{Allow: true, Scope: policy.ScopePattern}, nil
		default:
			fmt.Fprintln(t.out, "please answer y, n, s or p")
		}
	}
}
```

- [ ] **Step 4: Give the CLI one reader over stdin**

`cmdChat` already owns a `bufio.Scanner` on `os.Stdin`. The approval prompt reads from the terminal too, and two scanners over one file descriptor each read ahead and steal the other's input — an approval answer would vanish into the chat loop's buffer. Create `cmd/spore/input.go`:

```go
package main

import (
	"bufio"
	"os"
)

// stdinLines is the process's single reader over standard input. Both the
// chat loop and the approval prompt take lines from it.
var stdinLines = newStdinScanner()

func newStdinScanner() *bufio.Scanner {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	return sc
}
```

In `cmd/spore/chat.go`, delete the local scanner and use the shared one:

```go
	sc := stdinLines
```

removing the two lines that constructed and sized it, and dropping the now-unused `bufio` import.

- [ ] **Step 5: Run the approver tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run Terminal -v`
Expected: PASS (4 tests).

- [ ] **Step 6: Wire the registry and the guard into the agent**

In `cmd/spore/wire.go`, add the builder and pass it to `agent.New`:

```go
// buildTools assembles the registry, the policy engine and the guard that
// wraps them. The agent receives the guard, so no tool call reaches a builtin
// without a policy decision behind it.
func buildTools(cfg *config.Config, st *store.Store) (*policy.Guard, error) {
	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	tools := fs.New(cfg.Policy.Workspace, cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(cfg.Policy.Workspace, time.Duration(cfg.Shell.TimeoutSeconds)*time.Second))
	tools = append(tools, web.New(cfg.Web, cfg.Policy.MaxOutput)...)
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return nil, err
		}
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return nil, err
	}
	approver := terminalApprover{lines: stdinLines, out: os.Stdout}
	learn := func(d policy.Decision, rule string) error {
		return config.LearnRule(cfg.Path, string(d), rule)
	}
	return policy.NewGuard(reg, engine, approver, st, learn), nil
}
```

Change the end of `buildAgent` from `return agent.New(st, reg, rt, cfg, nil), nil` to:

```go
	tools, err := buildTools(cfg, st)
	if err != nil {
		return nil, err
	}
	return agent.New(st, reg, rt, cfg, tools), nil
```

Add the imports: `os`, `time`, `internal/config`, `internal/policy`, `internal/tool`, `internal/tool/fs`, `internal/tool/shell`, `internal/tool/web`.

`config.LearnRule` needs the file it was loaded from. Add a `Path` field to `config.Config` (untagged, so TOML never sets it) and populate it in `Load`:

```go
type Config struct {
	// Path is the file this config was loaded from. It carries no TOML tag:
	// it is set by Load, never read from the file.
	Path string `toml:"-"`
	...
}
```

In `Load`, after `Validate` succeeds: `cfg.Path = path`.

- [ ] **Step 7: Attach the session to the context in the CLI**

Every turn must carry its session and trust profile so the guard can persist suspensions. In `cmd/spore/once.go`, change the `cmdOnce` body after the session is created:

```go
	ctx = policy.WithSession(ctx, sid, policy.ProfileLocal)
	ch, err := a.Run(ctx, sid, prompt)
```

Make the same change in `cmd/spore/chat.go`, wrapping the context once, after the session id is known and before the first `a.Run`. Add the `internal/policy` import to both files.

- [ ] **Step 8: Add `spore policy check`**

Create `cmd/spore/policy.go`:

```go
package main

import (
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// cmdPolicyCheck answers "what would spore do with this call?" without
// running anything — the way to test a ruleset after editing it.
func cmdPolicyCheck(cfg *config.Config, profile, toolName, argsJSON string) error {
	if argsJSON == "" {
		argsJSON = "{}"
	}
	if !json.Valid([]byte(argsJSON)) {
		return fmt.Errorf("arguments are not valid JSON: %s", argsJSON)
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		return err
	}
	res := engine.Evaluate(policy.Profile(profile), policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)})
	fmt.Printf("%s\t%s\t%s\n", res.Decision, toolName, res.Rule)
	if res.Decision == policy.DecisionAsk {
		fmt.Printf("  \"always this pattern\" would write: %s\n", policy.PatternFor(
			policy.Call{Tool: toolName, Args: json.RawMessage(argsJSON)}))
	}
	return nil
}
```

In `cmd/spore/main.go`, add a case to the `switch args[0]` in `run`, beside `once`, `chat` and `session`:

```go
	case "policy":
		// spore policy check <tool> [json-args] [-profile local|remote]
		if len(args) < 3 || args[1] != "check" {
			return fmt.Errorf("usage: spore policy check <tool> [json-args] [-profile local|remote]")
		}
		profile, jsonArgs := "local", "{}"
		rest := args[3:]
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-profile" && i+1 < len(rest) {
				profile = rest[i+1]
				i++
				continue
			}
			jsonArgs = rest[i]
		}
		return cmdPolicyCheck(cfg, profile, args[2], jsonArgs)
```

And add the verb to the `usage` const, after the `session show` line:

```
  spore policy check <tool> [json-args]
                               print the decision a tool call would get
```

- [ ] **Step 9: Run everything**

```bash
make vet
make test
make build
```

Expected: vet clean, all packages PASS, the binary builds.

- [ ] **Step 10: Exercise it end to end by hand**

```bash
mkdir -p /tmp/spore-demo && cd /tmp/spore-demo && git init -q 2>/dev/null; true
cat > /tmp/spore-demo/config.toml <<'CFG'
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind = "anthropic"
api_key = "${ANTHROPIC_API_KEY}"

data_dir = "/tmp/spore-demo"

[policy]
workspace = "/tmp/spore-demo"
CFG
```

Then, from the repo:

```bash
CFG=/tmp/spore-demo/config.toml
./spore -config $CFG policy check fs_read '{"path":"/tmp/spore-demo/x.go"}'
./spore -config $CFG policy check fs_read '{"path":"/etc/passwd"}'
./spore -config $CFG policy check fs_write '{"path":"/tmp/spore-demo/x.go"}'
./spore -config $CFG policy check shell_exec '{"command":"sudo rm -rf /"}'
./spore -config $CFG policy check fs_write '{"path":"/tmp/spore-demo/x.go"}' -profile remote
```

Expected, in order: `allow`; `deny` (workspace escape); `ask`; `deny` (baseline shell rule); `ask`. Note that `spore policy check` needs `data_dir` to be writable, since `run` opens the store before dispatching — point `data_dir` at `/tmp/spore-demo` in the config above if the default is not wanted.

With a real API key, confirm a full turn: `./spore -config $CFG once "list the files in the workspace, then write a file called hello.txt"` runs `fs_list` with no prompt and stops for approval before `fs_write`.

- [ ] **Step 11: Update the README**

Replace the Status paragraph:

```markdown
Status: **Plan 2 (tools and policy)** — everything in Plan 1, plus a tool
registry with `fs`, `shell` and `web` builtins and a policy engine that
resolves every call to allow, ask or deny on the tool and its arguments.
Approvals suspend the turn, persist to SQLite, and survive a restart. The
daemon and web UI, MCP, Telegram, and Weaviate recall land in Plans 3–5.
```

Add a section after **Configure**:

```markdown
## Tools and policy

spore ships six filesystem tools (`fs_read`, `fs_write`, `fs_edit`,
`fs_list`, `fs_glob`, `fs_grep`), `shell_exec`, and `web_fetch` —
plus `web_search` when a search key is configured.

Every call is checked before it runs:

    [policy]
    workspace = "~/dev"       # filesystem tools may not leave this tree
    default   = "ask"
    allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"]
    ask       = ["fs_write", "fs_edit", "shell_exec", "mcp__*"]
    deny      = ["shell_exec(matches terraform destroy)"]

    [web]
    brave_api_key = "${BRAVE_API_KEY}"

Rules are `tool` or `tool(predicate)`, where a predicate is
`path outside workspace`, `path matches <globs>`, or `matches <text>`. Tool
globs accept `fs.read` and `fs_read` interchangeably.

**Deny is checked first and is absolute.** A baseline deny list — paths
outside the workspace, `.env`, `.ssh`, private keys, and the usual
destructive shell forms — is always in force and is not opt-out. No approval
answer can override it.

An `ask` decision suspends the turn and prompts:

    allow? [y]es once  [n]o  [s]ession  [p]attern

`s` remembers the answer for the rest of the session; `p` writes a rule into
a marked block at the end of `config.toml`, which you can edit or delete.
An approval nobody answers within `approval_timeout` (default 5m) is denied.

Check a ruleset without running anything:

    spore policy check fs_write '{"path":"/etc/hosts"}'
```

- [ ] **Step 12: Commit**

```bash
git add cmd/spore internal/config README.md
git commit -m "feat(cli): wire tools and policy into the agent with a terminal approver"
```

---

## Verification Checklist

Run before declaring the plan complete.

- [ ] `make vet && make test && make build` all clean.
- [ ] `go list -tags sqlite_fts5 -deps ./internal/agent | grep -E 'internal/(policy|tool|daemon)|net/http'` prints nothing — the core is still transport-free and policy-free.
- [ ] `go list -tags sqlite_fts5 -deps ./internal/policy | grep 'internal/agent'` prints nothing.
- [ ] The adversarial path suite passes on a machine where `/tmp` is a symlink as well as one where it is not.
- [ ] `spore policy check` reports `deny` for: a path outside the workspace, `**/.env`, `~/.ssh/id_rsa`, and `sudo` in a shell command — under both the `local` and `remote` profiles.
- [ ] A turn that gets an `ask` and receives no answer within `approval_timeout` ends with a denial reported to the model, and leaves no row in `pending_calls` with `state = 'pending'`.
- [ ] Nothing in this plan introduced a dependency beyond `golang.org/x/net`.

---

## Deferred to later plans

Named here so they are not mistaken for gaps:

- **`memory` and `recall` tools** — they depend on subsystems that land in Plan 5 and ship with it (spec §6).
- **MCP tools** (`mcp__<server>__<tool>`) — Plan 4. The policy rules and the registry already accommodate the namespace; nothing here needs to change to admit them.
- **The `schedule` tool** — Plan 3, with the scheduler and background jobs.
- **SSE approval events and multi-client answering** — Plan 3. `Guard.Pending` and `Guard.Resolve` are the API that plan consumes; the `Approver` interface is what the daemon implements.
- **The `remote` trust profile in anger** — the engine supports profiles now and is tested for them, but nothing sets `ProfileRemote` until the Telegram bridge in Plan 4.
- **`spore doctor`** — spec §8 lists it under the CLI; it validates providers, MCP servers and sidecars, most of which do not exist until Plans 4 and 5.
