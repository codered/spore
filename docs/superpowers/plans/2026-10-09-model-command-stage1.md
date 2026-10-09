# /model Stage 1 (core + plain chat) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-operation model choice: per-session `chat`/`subagent` models, daemon-wide overrides for the background sites, a live model catalog, the API, a `model` argument on the sub-agent tools, and a numbered `/model` in plain chat.

**Architecture:** The router gains runtime overrides for the four background sites (persisted in a spore-managed block of `config.toml`). Sessions gain `chat_model`/`subagent_model` columns that the agent reads for `chat` and `subagent` turns. A new `internal/models` package owns the catalog (live `/v1/models` per provider) and the selection service the daemon, supervisor and clients use. `internal/modelcmd` renders and parses the text form.

**Tech Stack:** Go 1.26, SQLite (modernc / mattn with `sqlite_fts5` tag), BurntSushi/toml, net/http.

**Spec:** `docs/superpowers/specs/2026-10-09-model-command-design.md`

## Global Constraints

- Work in `/home/code/development/spore-model-command` (branch `feat/model-command`). Never `git add -A`; add the files you changed by name.
- Run tests with the build tag: `go test -tags sqlite_fts5 ./...`. CI also runs `go vet -tags sqlite_fts5 ./...`, `make fmtcheck`, `make lint`, `make vulncheck`, `make tidycheck`, and `go test -tags sqlite_fts5 -race ./...`.
- Every Go file gofmt-clean. Comments explain *why*, in the style of the surrounding code.
- Call sites, in the order every surface shows them: `chat, compaction, title, classify, refinement, subagent`.
- Per-session sites: `chat`, `subagent`. Daemon-wide sites: `compaction`, `title`, `classify`, `refinement`.
- No path accepts a free-text model ref: a ref is accepted only if it is in the catalog, or equals the op's default (which clears the override).
- Managed routing markers, verbatim:
  `# >>> spore-managed routing — written by /model; edit or delete freely` and `# <<< spore-managed routing`.
- Commit message trailer on every commit:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016aHKRsXHkr3iTMvo9YixbC
  ```

## Review Focus

1. A model id that itself contains `/` (e.g. `unsloth/Qwen3.8-27B-GGUF`) must round-trip as `studio/unsloth/Qwen3.8-27B-GGUF` everywhere (catalog, store, managed block, Resolve). Test in Task 5.
2. Removing the last override must remove the managed routing block and leave the policy managed block and the rest of the file byte-identical. Test in Task 2.
3. A provider that hangs must not hold `/model` past the 3s timeout, and its error must not hide other providers. Test in Task 5.
4. Choosing the default entry must clear the override (subagent: clears to "inherit chat"), not pin the current default. Test in Task 6.
5. An invalid `model` on `agent_run` must create no child session and no run row. Test in Task 8.

---

### Task 1: Router overrides

**Files:**
- Modify: `internal/router/router.go`
- Test: `internal/router/router_test.go`

**Interfaces:**
- Produces: `router.Sites []string`, `router.GlobalSites []string`, `router.IsGlobalSite(string) bool`, `(*Router).Default(site string) string`, `(*Router).SetOverride(site, ref string) error`, `(*Router).RuleMatches(site string) bool`. `(*Router).Model` now consults overrides first.

- [ ] **Step 1: Write the failing tests** — append to `internal/router/router_test.go`:

```go
func TestOverrideWinsAndDefaultIgnoresIt(t *testing.T) {
	r, err := New([]config.Route{{When: "title", Model: "a/routed"}}, "a/default")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetOverride(SiteTitle, "b/picked"); err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteTitle); got != "b/picked" {
		t.Fatalf("Model(title) = %q, want the override", got)
	}
	if got := r.Default(SiteTitle); got != "a/routed" {
		t.Fatalf("Default(title) = %q, want the hand-written route", got)
	}
	if err := r.SetOverride(SiteTitle, ""); err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteTitle); got != "a/routed" {
		t.Fatalf("after clearing, Model(title) = %q, want the route", got)
	}
}

func TestOverrideRefusesPerSessionSites(t *testing.T) {
	r, _ := New(nil, "a/default")
	for _, site := range []string{SiteChat, SiteSubagent, "nope"} {
		if err := r.SetOverride(site, "b/x"); err == nil {
			t.Errorf("SetOverride(%q) succeeded, want an error", site)
		}
	}
}

func TestRuleMatches(t *testing.T) {
	r, _ := New([]config.Route{{When: "compaction|subagent", Model: "a/x"}}, "a/default")
	if !r.RuleMatches(SiteSubagent) {
		t.Error("RuleMatches(subagent) = false, want true")
	}
	if r.RuleMatches(SiteChat) {
		t.Error("RuleMatches(chat) = true, want false")
	}
}

func TestOverridesAreSafeConcurrently(t *testing.T) {
	r, _ := New(nil, "a/default")
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = r.SetOverride(SiteTitle, "b/x")
			_ = r.SetOverride(SiteTitle, "")
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = r.Model(SiteTitle)
	}
	<-done
}

func TestSitesOrder(t *testing.T) {
	want := []string{SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteSubagent}
	if !slices.Equal(Sites, want) {
		t.Fatalf("Sites = %v, want %v", Sites, want)
	}
	for _, s := range GlobalSites {
		if !IsGlobalSite(s) {
			t.Errorf("IsGlobalSite(%q) = false", s)
		}
	}
	if IsGlobalSite(SiteChat) || IsGlobalSite(SiteSubagent) {
		t.Error("chat and subagent are per-session, not global")
	}
}
```

Add `"slices"` to the test file's imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/router/`
Expected: FAIL to compile (`r.SetOverride undefined`, `Sites undefined`).

- [ ] **Step 3: Implement** — in `internal/router/router.go`:

Add imports `"slices"` and `"sync"`. After the `ValidSite` function add:

```go
// Sites is every call site, in the order /model shows them.
var Sites = []string{SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteSubagent}

// GlobalSites are the call sites /model sets for the whole daemon. chat and
// subagent are chosen per session instead, so they are never overridden here.
var GlobalSites = []string{SiteCompaction, SiteTitle, SiteClassify, SiteRefinement}

// IsGlobalSite reports whether /model sets site daemon-wide.
func IsGlobalSite(site string) bool { return slices.Contains(GlobalSites, site) }
```

Replace the `Router` struct, `Model`, and add the new methods:

```go
// Router is shared by every caller that picks a model (agent, compaction,
// title, refinement), so an override set by /model reaches all of them
// without anything being rebuilt. The mutex is why it is safe to change
// while turns are running.
type Router struct {
	mu           sync.RWMutex
	rules        []rule
	defaultModel string
	overrides    map[string]string
}
```

In `New`, initialise `overrides: map[string]string{}` in the struct literal.

```go
// Model returns the model ref for a call site: a /model override, else the
// first matching rule, else the configured default.
func (r *Router) Model(callSite string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if ref, ok := r.overrides[callSite]; ok {
		return ref
	}
	return r.ruleModel(callSite)
}

// Default is Model without overrides: what config.toml alone says. /model
// marks it with "*", and choosing it clears the override.
func (r *Router) Default(callSite string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ruleModel(callSite)
}

func (r *Router) ruleModel(callSite string) string {
	for _, rule := range r.rules {
		if rule.re.MatchString(callSite) {
			return rule.model
		}
	}
	return r.defaultModel
}

// SetOverride makes ref the model for a daemon-wide site; "" removes the
// override. Per-session sites are refused: their choice lives on the session.
func (r *Router) SetOverride(site, ref string) error {
	if !IsGlobalSite(site) {
		return fmt.Errorf("%q is not a call site /model sets daemon-wide", site)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ref == "" {
		delete(r.overrides, site)
		return nil
	}
	r.overrides[site] = ref
	return nil
}

// RuleMatches reports whether any hand-written rule matches site. Startup
// uses it to warn that a subagent rule no longer selects anything.
func (r *Router) RuleMatches(site string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rule := range r.rules {
		if rule.re.MatchString(site) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 -race ./internal/router/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/router/router.go internal/router/router_test.go
git commit -m "router: daemon-wide overrides for the background call sites"   # plus the trailer
```

---

### Task 2: Config — `[routing.override]` and its managed block

**Files:**
- Modify: `internal/config/config.go` (Config struct, `Validate`, new `ConfiguredRefs`)
- Modify: `internal/config/write.go` (extract `writeConfigFile`, generalise `splitManaged`)
- Create: `internal/config/routing_write.go`
- Test: `internal/config/routing_write_test.go`

**Interfaces:**
- Produces: `config.RoutingConfig{Override map[string]string}`, field `Config.Routing`, `config.RoutingOverrideSites []string`, `config.RoutingBegin`, `config.RoutingEnd`, `config.SetRoutingOverride(path, site, ref string) error`, `(*Config).ConfiguredRefs() []string`.

- [ ] **Step 1: Write the failing tests** — create `internal/config/routing_write_test.go`:

```go
package config

import (
	"os"
	"strings"
	"testing"
)

const routingBase = `default_model = "studio/big"

[[route]]
when  = "title"
model = "studio/small"

[policy]
workspace = "/ws"
`

func TestSetRoutingOverrideCreatesTheBlockAndLoadApplies(t *testing.T) {
	p := write(t, routingBase)
	if err := SetRoutingOverride(p, "title", "jetson/unsloth/gemma-4"); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	text := string(body)
	if !strings.Contains(text, RoutingBegin) || !strings.Contains(text, RoutingEnd) {
		t.Fatalf("no managed routing block:\n%s", text)
	}
	if !strings.HasPrefix(text, routingBase) {
		t.Fatalf("text before the block changed:\n%s", text)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Routing.Override["title"]; got != "jetson/unsloth/gemma-4" {
		t.Fatalf("Routing.Override[title] = %q", got)
	}
}

func TestSetRoutingOverrideReplacesAndClears(t *testing.T) {
	p := write(t, routingBase)
	if err := LearnRule(p, "allow", "fs_read"); err != nil {
		t.Fatal(err)
	}
	withPolicy, _ := os.ReadFile(p)

	if err := SetRoutingOverride(p, "title", "a/one"); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "compaction", "a/two"); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "title", "a/three"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Routing.Override["title"] != "a/three" || cfg.Routing.Override["compaction"] != "a/two" {
		t.Fatalf("overrides = %v", cfg.Routing.Override)
	}
	if len(cfg.Policy.Learned.Allow) != 1 {
		t.Fatalf("policy block lost: %+v", cfg.Policy.Learned)
	}

	if err := SetRoutingOverride(p, "title", ""); err != nil {
		t.Fatal(err)
	}
	if err := SetRoutingOverride(p, "compaction", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p)
	if strings.Contains(string(after), RoutingBegin) {
		t.Fatalf("empty block was left behind:\n%s", after)
	}
	if strings.TrimRight(string(after), "\n") != strings.TrimRight(string(withPolicy), "\n") {
		t.Fatalf("file not restored after clearing every override.\nbefore:\n%s\nafter:\n%s", withPolicy, after)
	}
}

func TestSetRoutingOverrideClearingNothingWritesNothing(t *testing.T) {
	p := write(t, routingBase)
	if err := SetRoutingOverride(p, "title", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(p)
	if string(body) != routingBase {
		t.Fatalf("file changed:\n%s", body)
	}
}

func TestSetRoutingOverrideRefusesBadInput(t *testing.T) {
	p := write(t, routingBase)
	for _, tc := range []struct{ site, ref string }{
		{"chat", "a/b"},      // per-session site
		{"subagent", "a/b"},  // per-session site
		{"nope", "a/b"},      // not a site
		{"title", "noslash"}, // not provider/model
		{"title", `a/"x`},    // cannot be written as a basic string
	} {
		if err := SetRoutingOverride(p, tc.site, tc.ref); err == nil {
			t.Errorf("SetRoutingOverride(%q, %q) succeeded", tc.site, tc.ref)
		}
	}
}

func TestLoadRejectsAnOverrideForAPerSessionSite(t *testing.T) {
	p := write(t, routingBase+"\n[routing.override]\nchat = \"a/b\"\n")
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "routing.override.chat") {
		t.Fatalf("Load error = %v, want one naming routing.override.chat", err)
	}
}

func TestConfiguredRefs(t *testing.T) {
	p := write(t, routingBase+"\n[routing.override]\ncompaction = \"c/z\"\n")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cfg.ConfiguredRefs(), ",")
	if got != "studio/big,studio/small,c/z" {
		t.Fatalf("ConfiguredRefs = %s", got)
	}
}
```

(`write(t, body)` is the existing helper in `write_test.go` that writes a temp config and returns its path. If `Load` needs `policy.workspace` to exist, the existing tests already pass `/ws`; keep it.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run 'Routing|ConfiguredRefs'`
Expected: FAIL to compile.

- [ ] **Step 3a: Config struct, validation, ConfiguredRefs** — in `internal/config/config.go`:

Add to `Config` after `Routes`:

```go
	// Routing holds what /model set for the daemon-wide call sites. It is
	// written only inside the spore-managed routing block.
	Routing RoutingConfig `toml:"routing"`
```

After the `Route` type add:

```go
// RoutingConfig is the [routing] table. Override maps a daemon-wide call
// site to the model /model chose for it; it wins over every [[route]].
type RoutingConfig struct {
	Override map[string]string `toml:"override"`
}

// RoutingOverrideSites are the call sites [routing.override] may name. It
// mirrors router.GlobalSites, which config cannot import.
var RoutingOverrideSites = []string{"compaction", "title", "classify", "refinement"}

// ConfiguredRefs is every model ref the file names: default_model, each
// route's model and each override, first appearance first. A provider that
// cannot list its models offers these.
func (c *Config) ConfiguredRefs() []string {
	var out []string
	add := func(ref string) {
		if ref != "" && !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	add(c.DefaultModel)
	for _, r := range c.Routes {
		add(r.Model)
	}
	for _, site := range RoutingOverrideSites {
		add(c.Routing.Override[site])
	}
	return out
}
```

(Add `"slices"` to imports if not present.)

In `Validate`, right after the `for i, r := range c.Routes { ... }` loop, add:

```go
	for site, ref := range c.Routing.Override {
		if !slices.Contains(RoutingOverrideSites, site) {
			return fmt.Errorf("routing.override.%s: /model sets only compaction, title, classify and refinement daemon-wide", site)
		}
		if err := ValidateModelRef(ref); err != nil {
			return fmt.Errorf("routing.override.%s: %w", site, err)
		}
	}
```

- [ ] **Step 3b: Refactor write.go** — in `internal/config/write.go`:

1. Replace `splitManaged` with a general function and keep the old name as a wrapper:

```go
// splitManaged returns the text before the managed policy block, the
// block's inner body, the text after it, and whether a block was found.
func splitManaged(body string) (before, inner, after string, found bool) {
	return splitBlock(body, ManagedBegin, ManagedEnd)
}

// splitBlock is splitManaged for any pair of markers.
func splitBlock(body, begin, end string) (before, inner, after string, found bool) {
	i := strings.Index(body, begin)
	if i < 0 {
		return body, "", "", false
	}
	rest := body[i+len(begin):]
	j := strings.Index(rest, end)
	if j < 0 {
		return body, "", "", false
	}
	return body[:i], rest[:j], rest[j+len(end):], true
}
```

2. Move the tail of `rewriteLearned` — from the comment `// Never replace the user's config with something that will not load.` through `return os.Rename(tmp.Name(), path)` — into a new function, and call it:

```go
// writeConfigFile replaces path with out, refusing anything that does not
// parse. Callers hold learnMu.
func writeConfigFile(path, out string) error {
	// (the moved block, unchanged: probe decode, temp file, chmod 0600, rename)
}
```

`rewriteLearned` then ends with `return writeConfigFile(path, out)`.

- [ ] **Step 3c: Create `internal/config/routing_write.go`**:

```go
package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The spore-managed routing block. /model writes the daemon-wide choices
// here, apart from the policy block, so neither rewrite can disturb the
// other and the hand-written [[route]] rules stay the operator's.
const (
	RoutingBegin = "# >>> spore-managed routing — written by /model; edit or delete freely"
	RoutingEnd   = "# <<< spore-managed routing"
)

// SetRoutingOverride records ref as the model for a daemon-wide call site;
// "" removes it. The block is created on first use and removed when its
// last entry goes, so a file whose overrides were all cleared reads as it
// did before /model touched it.
func SetRoutingOverride(path, site, ref string) error {
	if !slices.Contains(RoutingOverrideSites, site) {
		return fmt.Errorf("%q is not a call site /model sets daemon-wide", site)
	}
	if ref != "" {
		if err := ValidateModelRef(ref); err != nil {
			return err
		}
		// Rendered as a basic TOML string, so refuse what would need escaping.
		if strings.ContainsAny(ref, "\"\\\n\r") {
			return fmt.Errorf("model ref %q contains characters that cannot be written to config", ref)
		}
	}

	learnMu.Lock()
	defer learnMu.Unlock()

	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	body := string(raw)
	if n := strings.Count(body, RoutingBegin); n > 1 {
		return fmt.Errorf("%s contains %d spore-managed routing markers; remove all but one block by hand", path, n)
	}

	before, inner, after, found := splitBlock(body, RoutingBegin, RoutingEnd)
	over := map[string]string{}
	if found {
		if over, err = parseRouting(inner); err != nil {
			return err
		}
	}
	if ref == "" {
		if _, ok := over[site]; !ok {
			return nil // nothing to clear; leave the file alone
		}
		delete(over, site)
	} else {
		over[site] = ref
	}

	block := renderRouting(over)
	var out string
	switch {
	case found && block == "":
		out = strings.TrimRight(before, "\n") + "\n"
		if rest := strings.TrimLeft(after, "\n"); rest != "" {
			out += "\n" + rest
		}
	case found:
		out = before + block + strings.TrimPrefix(after, "\n")
	default:
		out = strings.TrimRight(body, "\n") + "\n\n" + block
	}
	return writeConfigFile(path, out)
}

func parseRouting(inner string) (map[string]string, error) {
	var doc struct {
		Routing RoutingConfig `toml:"routing"`
	}
	if _, err := toml.Decode(inner, &doc); err != nil {
		return nil, fmt.Errorf("the spore-managed routing block is not valid TOML: %w", err)
	}
	if doc.Routing.Override == nil {
		return map[string]string{}, nil
	}
	return doc.Routing.Override, nil
}

// renderRouting returns the whole block, markers included, or "" for no
// overrides. Keys are sorted so a rewrite never reorders lines needlessly.
func renderRouting(over map[string]string) string {
	if len(over) == 0 {
		return ""
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(RoutingBegin)
	b.WriteString("\n[routing.override]\n")
	for _, k := range keys {
		b.WriteString(k + " = \"" + over[k] + "\"\n")
	}
	b.WriteString(RoutingEnd)
	b.WriteString("\n")
	return b.String()
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/config/`
Expected: PASS (new and all existing tests). If `TestSetRoutingOverrideReplacesAndClears`'s byte comparison fails only on blank lines, fix `SetRoutingOverride`'s joining, not the test.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/write.go internal/config/routing_write.go internal/config/routing_write_test.go
git commit -m "config: [routing.override] in a spore-managed block"   # plus the trailer
```

---

### Task 3: Store — per-session model columns

**Files:**
- Modify: `internal/store/schema.go`, `internal/store/store.go`
- Test: `internal/store/session_model_test.go`

**Interfaces:**
- Produces: `store.Session.ChatModel string`, `store.Session.SubagentModel string`, `(*Store).SetSessionModel(ctx, id, site, ref string) error`.

- [ ] **Step 1: Write the failing test** — create `internal/store/session_model_test.go`:

```go
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestSetSessionModelRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.Session(ctx, id)

	if err := s.SetSessionModel(ctx, id, "chat", "studio/unsloth/Qwen3.8-27B-GGUF"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionModel(ctx, id, "subagent", "jetson/gemma"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Session(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Session: %v %v", ok, err)
	}
	if got.ChatModel != "studio/unsloth/Qwen3.8-27B-GGUF" || got.SubagentModel != "jetson/gemma" {
		t.Fatalf("models = %q, %q", got.ChatModel, got.SubagentModel)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatal("choosing a model moved updated_at")
	}
	if err := s.SetSessionModel(ctx, id, "chat", ""); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Session(ctx, id)
	if got.ChatModel != "" {
		t.Fatalf("ChatModel = %q after clearing", got.ChatModel)
	}
}

func TestSetSessionModelRefusesOtherSitesAndMissingSessions(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	id, _ := s.CreateSession(ctx, "t", "/ws")
	if err := s.SetSessionModel(ctx, id, "title", "a/b"); err == nil {
		t.Error("title accepted as a per-session site")
	}
	if err := s.SetSessionModel(ctx, "missing", "chat", "a/b"); err == nil {
		t.Error("missing session accepted")
	}
}

func TestOldDatabaseGainsModelColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at) VALUES ('old', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok, err := s.Session(context.Background(), "old")
	if err != nil || !ok || got.ChatModel != "" {
		t.Fatalf("old row: %+v %v %v", got, ok, err)
	}
}
```

Before writing `TestOldDatabaseGainsModelColumns`, check how other migration tests in `internal/store` open a raw database (search `sql.Open(` in `internal/store/*_test.go`) and use the same driver name expression; replace `driverName` with it.

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'SessionModel|ModelColumns'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`internal/store/schema.go`, in `CREATE TABLE IF NOT EXISTS sessions`, after the `source` line add:

```sql
  chat_model     TEXT NOT NULL DEFAULT '',
  subagent_model TEXT NOT NULL DEFAULT '',
```

`internal/store/store.go`:

1. `Session` struct, after `Source`:

```go
	// ChatModel is the model /model chose for this session's own turns, or
	// "" for the chat route. A sub-agent's session is created with the model
	// it was launched on, so its turns read it from here too.
	ChatModel string
	// SubagentModel is what this session's sub-agents run on, or "" to
	// inherit the session's own chat model.
	SubagentModel string
```

2. Migration: after the `refine_attempted_at` block add:

```go
	for _, col := range []string{"chat_model", "subagent_model"} {
		if !have[col] {
			if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ` + col + ` TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("add sessions.%s: %w", col, err)
			}
		}
	}
```

3. `sessionCols` becomes:

```go
const sessionCols = `id, title, workspace, parent_id, source, chat_model, subagent_model, created_at, updated_at, job_id, seen_seq,
	(SELECT coalesce(max(seq), 0) FROM messages WHERE session_id = sessions.id)`
```

and `scanSession`'s Scan list gains `&sess.ChatModel, &sess.SubagentModel` right after `&sess.Source`.

4. After `CreateChildSession` add:

```go
// SetSessionModel stores the model chosen for one of a session's own call
// sites, "chat" or "subagent"; "" clears it. updated_at is left alone:
// choosing a model is not activity in the conversation.
func (s *Store) SetSessionModel(ctx context.Context, id, site, ref string) error {
	var col string
	switch site {
	case "chat":
		col = "chat_model"
	case "subagent":
		col = "subagent_model"
	default:
		return fmt.Errorf("set session model: %q is not a per-session call site", site)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET `+col+` = ? WHERE id = ?`, ref, id)
	if err != nil {
		return fmt.Errorf("set session model: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set session model: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("set session model: no session %s", id)
	}
	return nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/store/`
Expected: PASS (all).

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.go internal/store/store.go internal/store/session_model_test.go
git commit -m "store: sessions remember their chat and sub-agent models"   # plus the trailer
```

---

### Task 4: Providers that can list their models

**Files:**
- Modify: `internal/provider/types.go`, `internal/provider/registry.go`, `internal/provider/openaicompat/openaicompat.go`
- Test: `internal/provider/openaicompat/models_test.go`, `internal/provider/registry_test.go` (create if absent; else append)

**Interfaces:**
- Produces: `provider.Lister` interface `ListModels(ctx) ([]string, error)`; `(*provider.Registry).Names() []string` (sorted); `(*provider.Registry).Provider(name string) (provider.Provider, bool)`; `(*openaicompat.Client).ListModels`.

- [ ] **Step 1: Write the failing tests**

`internal/provider/openaicompat/models_test.go`:

```go
package openaicompat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestListModelsReadsTheOpenAIShape(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet {
			t.Errorf("%s %s, want GET /v1/models", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"unsloth/b"},{"id":"a"},{"id":""}]}`))
	}))
	defer ts.Close()
	ids, err := New(ts.URL+"/v1", "k", nil).ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []string{"a", "unsloth/b"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestListModelsReportsHTTPErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	if _, err := New(ts.URL, "", nil).ListModels(context.Background()); err == nil {
		t.Fatal("want an error for 401")
	}
}
```

Registry test (append to `internal/provider/registry_test.go`, or create it with `package provider` and imports `slices`, `testing`):

```go
func TestRegistryNamesAndProvider(t *testing.T) {
	r := NewRegistry()
	r.Register("zeta", NewScript(), ProviderPrice{})
	r.Register("alpha", NewScript(), ProviderPrice{})
	if got := r.Names(); !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("Names = %v", got)
	}
	if _, ok := r.Provider("alpha"); !ok {
		t.Fatal("Provider(alpha) missing")
	}
	if _, ok := r.Provider("nope"); ok {
		t.Fatal("Provider(nope) found")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/provider/...`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`internal/provider/types.go`, after the `Provider` interface:

```go
// Lister is a provider that can say which models it serves. /model offers
// only what a provider lists; a provider without it offers the refs the
// config names for it.
type Lister interface {
	ListModels(ctx context.Context) ([]string, error)
}
```

`internal/provider/registry.go` (add `"sort"` import):

```go
// Names returns the registered provider names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.entries))
	for n := range r.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Provider returns one registered provider.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	return e.p, ok
}
```

`internal/provider/openaicompat/openaicompat.go`, after `Name()`:

```go
// ListModels asks the endpoint which models it serves: GET {base_url}/models
// in the OpenAI shape, data[].id. llama-server, LiteLLM and Unsloth Studio
// all answer it. The ids come back bare and sorted; the caller adds the
// provider prefix. The caller's context bounds the call: the client's own
// timeout is sized for a long completion, not a listing.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list models: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/provider/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/types.go internal/provider/registry.go internal/provider/registry_test.go internal/provider/openaicompat/openaicompat.go internal/provider/openaicompat/models_test.go
git commit -m "provider: list the models an OpenAI-compatible endpoint serves"   # plus the trailer
```

---

### Task 5: `internal/models` — the catalog

**Files:**
- Create: `internal/models/catalog.go`
- Test: `internal/models/catalog_test.go`

**Interfaces:**
- Consumes: `provider.Lister`, `(*provider.Registry).Names/Provider` (Task 4).
- Produces: `models.Group{Provider, Refs []string, Error string}` (JSON `provider`, `refs`, `error,omitempty`); `models.NewCatalog(reg *provider.Registry, configured []string) *Catalog`; `(*Catalog).List(ctx, fresh bool) []Group`; `(*Catalog).Has(ctx, ref string) bool`; `(*Catalog).Refs(ctx) []string` (every ref in the cached listing, in display order).

- [ ] **Step 1: Write the failing tests** — `internal/models/catalog_test.go`:

```go
package models

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
)

// lister is a provider that lists models; the embedded Script makes it a
// provider.Provider.
type lister struct {
	*provider.Script
	ids   []string
	err   error
	delay time.Duration
	calls atomic.Int32
}

func (l *lister) ListModels(ctx context.Context) ([]string, error) {
	l.calls.Add(1)
	if l.delay > 0 {
		select {
		case <-time.After(l.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.ids, l.err
}

func newLister(ids ...string) *lister { return &lister{Script: provider.NewScript(), ids: ids} }

func TestListGroupsByProviderWithPrefixedRefs(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("studio", newLister("unsloth/Qwen3.8-27B-GGUF", "a"), provider.ProviderPrice{})
	reg.Register("anthropic", provider.NewScript(), provider.ProviderPrice{})
	c := NewCatalog(reg, []string{"anthropic/claude-opus-5", "studio/x", "anthropic/claude-opus-5"})

	got := c.List(context.Background(), false)
	if len(got) != 2 || got[0].Provider != "anthropic" || got[1].Provider != "studio" {
		t.Fatalf("groups = %+v", got)
	}
	if !slices.Equal(got[0].Refs, []string{"anthropic/claude-opus-5"}) {
		t.Fatalf("non-listing provider refs = %v, want the configured ones", got[0].Refs)
	}
	if !slices.Equal(got[1].Refs, []string{"studio/a", "studio/unsloth/Qwen3.8-27B-GGUF"}) {
		t.Fatalf("listing provider refs = %v", got[1].Refs)
	}
	if !c.Has(context.Background(), "studio/unsloth/Qwen3.8-27B-GGUF") {
		t.Fatal("Has(id with a slash) = false")
	}
	if c.Has(context.Background(), "studio/x") {
		t.Fatal("a configured ref a listing provider does not serve must not be available")
	}
}

func TestASlowOrFailingProviderDoesNotHideTheOthers(t *testing.T) {
	reg := provider.NewRegistry()
	slow := newLister("never")
	slow.delay = time.Minute
	reg.Register("slow", slow, provider.ProviderPrice{})
	bad := newLister()
	bad.err = errors.New("connection refused")
	reg.Register("bad", bad, provider.ProviderPrice{})
	reg.Register("good", newLister("m"), provider.ProviderPrice{})
	c := NewCatalog(reg, nil)
	c.timeout = 50 * time.Millisecond

	start := time.Now()
	got := c.List(context.Background(), false)
	if time.Since(start) > 2*time.Second {
		t.Fatal("List waited for the slow provider")
	}
	byName := map[string]Group{}
	for _, g := range got {
		byName[g.Provider] = g
	}
	if byName["slow"].Error == "" || byName["bad"].Error == "" {
		t.Fatalf("errors not reported: %+v", got)
	}
	if !slices.Equal(byName["good"].Refs, []string{"good/m"}) {
		t.Fatalf("good refs = %v", byName["good"].Refs)
	}
}

func TestListCachesUntilFreshOrExpiry(t *testing.T) {
	reg := provider.NewRegistry()
	l := newLister("m")
	reg.Register("p", l, provider.ProviderPrice{})
	c := NewCatalog(reg, nil)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	c.List(context.Background(), false)
	c.List(context.Background(), false)
	if n := l.calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", n)
	}
	c.List(context.Background(), true)
	if n := l.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (fresh)", n)
	}
	now = now.Add(61 * time.Second)
	c.List(context.Background(), false)
	if n := l.calls.Load(); n != 3 {
		t.Fatalf("calls = %d, want 3 (expired)", n)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/models/`
Expected: FAIL (package does not exist).

- [ ] **Step 3: Implement** — `internal/models/catalog.go`:

```go
// Package models is what /model reads and writes: the catalog of models the
// configured providers serve, and the per-operation choice made from it.
package models

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/provider"
)

const (
	// listTimeout bounds one provider's listing. A provider that is down
	// must cost /model a short wait, not a hang.
	listTimeout = 3 * time.Second
	// cacheTTL keeps opening /model cheap; a choice re-checks fresh.
	cacheTTL = 60 * time.Second
)

// Group is one provider's models, as provider/model refs.
type Group struct {
	Provider string   `json:"provider"`
	Refs     []string `json:"refs"`
	Error    string   `json:"error,omitempty"`
}

// Catalog lists what each provider serves.
type Catalog struct {
	reg        *provider.Registry
	configured []string
	timeout    time.Duration
	ttl        time.Duration
	now        func() time.Time

	mu     sync.Mutex
	cached []Group
	at     time.Time
}

// NewCatalog lists the providers in reg. configured is every ref the config
// names; a provider that cannot list offers the ones with its prefix.
func NewCatalog(reg *provider.Registry, configured []string) *Catalog {
	return &Catalog{reg: reg, configured: configured, timeout: listTimeout, ttl: cacheTTL, now: time.Now}
}

// List returns one group per provider, sorted by name. fresh skips the cache.
func (c *Catalog) List(ctx context.Context, fresh bool) []Group {
	c.mu.Lock()
	if !fresh && c.cached != nil && c.now().Sub(c.at) < c.ttl {
		g := c.cached
		c.mu.Unlock()
		return g
	}
	c.mu.Unlock()

	groups := c.fetch(ctx)
	c.mu.Lock()
	c.cached, c.at = groups, c.now()
	c.mu.Unlock()
	return groups
}

func (c *Catalog) fetch(ctx context.Context) []Group {
	names := c.reg.Names()
	groups := make([]Group, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			groups[i] = c.group(ctx, name)
		}()
	}
	wg.Wait()
	return groups
}

func (c *Catalog) group(ctx context.Context, name string) Group {
	g := Group{Provider: name, Refs: []string{}}
	p, _ := c.reg.Provider(name)
	l, ok := p.(provider.Lister)
	if !ok {
		for _, ref := range c.configured {
			if strings.HasPrefix(ref, name+"/") && !slices.Contains(g.Refs, ref) {
				g.Refs = append(g.Refs, ref)
			}
		}
		sort.Strings(g.Refs)
		return g
	}
	lctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ids, err := l.ListModels(lctx)
	if err != nil {
		g.Error = err.Error()
		return g
	}
	for _, id := range ids {
		g.Refs = append(g.Refs, name+"/"+id)
	}
	sort.Strings(g.Refs)
	return g
}

// Has reports whether ref is served right now. It always lists fresh: a
// choice is checked against what the provider says at that moment, and a
// provider that is down makes its models unavailable.
func (c *Catalog) Has(ctx context.Context, ref string) bool {
	for _, g := range c.List(ctx, true) {
		if slices.Contains(g.Refs, ref) {
			return true
		}
	}
	return false
}

// Refs is every ref in the current listing, in display order.
func (c *Catalog) Refs(ctx context.Context) []string {
	var out []string
	for _, g := range c.List(ctx, false) {
		out = append(out, g.Refs...)
	}
	return out
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 -race ./internal/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/models/catalog.go internal/models/catalog_test.go
git commit -m "models: a catalog of what each provider serves"   # plus the trailer
```

---

### Task 6: `internal/models` — the selection service

**Files:**
- Create: `internal/models/service.go`
- Test: `internal/models/service_test.go`

**Interfaces:**
- Consumes: Task 1 router API, Task 2 `config.SetRoutingOverride`, Task 3 `store.Session.ChatModel/SubagentModel`, `(*Store).SetSessionModel`, Task 5 catalog.
- Produces:
  - `models.Op{Op, Scope, Selected, Default string}` (JSON `op`, `scope`, `selected`, `default`); `models.View{Ops []Op, Groups []Group}` (JSON `ops`, `groups`); constants `ScopeSession = "session"`, `ScopeGlobal = "global"`; `var ErrInvalid`.
  - `models.Service{Store *store.Store; Router *router.Router; Catalog *Catalog; ConfigPath string}` with
    `View(ctx, sessionID string, fresh bool) (View, error)`,
    `Set(ctx, sessionID, op, ref string) (View, error)`,
    `ChildModel(ctx, parentID, requested string) (string, error)`.

- [ ] **Step 1: Write the failing tests** — `internal/models/service_test.go`:

```go
package models

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

type fixture struct {
	svc *Service
	st  *store.Store
	sid string
	cfg string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("default_model = \"p/big\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := provider.NewRegistry()
	reg.Register("p", newLister("big", "small", "tiny"), provider.ProviderPrice{})
	rt, err := router.New([]config.Route{{When: "title", Model: "p/small"}}, "p/big")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := st.CreateSession(context.Background(), "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	return fixture{
		svc: &Service{Store: st, Router: rt, Catalog: NewCatalog(reg, nil), ConfigPath: cfgPath},
		st:  st, sid: sid, cfg: cfgPath,
	}
}

func opOf(t *testing.T, v View, name string) Op {
	t.Helper()
	for _, o := range v.Ops {
		if o.Op == name {
			return o
		}
	}
	t.Fatalf("no op %q in %+v", name, v.Ops)
	return Op{}
}

func TestViewDefaultsAndScopes(t *testing.T) {
	f := newFixture(t)
	v, err := f.svc.View(context.Background(), f.sid, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Ops) != 6 || v.Ops[0].Op != "chat" || v.Ops[5].Op != "subagent" {
		t.Fatalf("ops = %+v", v.Ops)
	}
	chat := opOf(t, v, "chat")
	if chat.Scope != ScopeSession || chat.Selected != "p/big" || chat.Default != "p/big" {
		t.Fatalf("chat = %+v", chat)
	}
	title := opOf(t, v, "title")
	if title.Scope != ScopeGlobal || title.Selected != "p/small" || title.Default != "p/small" {
		t.Fatalf("title = %+v", title)
	}
}

func TestSetChatThenSubagentInheritsIt(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	v, err := f.svc.Set(ctx, f.sid, "chat", "p/tiny")
	if err != nil {
		t.Fatal(err)
	}
	if o := opOf(t, v, "chat"); o.Selected != "p/tiny" || o.Default != "p/big" {
		t.Fatalf("chat = %+v", o)
	}
	if o := opOf(t, v, "subagent"); o.Selected != "p/tiny" || o.Default != "p/tiny" {
		t.Fatalf("subagent should inherit the chat choice: %+v", o)
	}
	got, err := f.svc.ChildModel(ctx, f.sid, "")
	if err != nil || got != "p/tiny" {
		t.Fatalf("ChildModel = %q, %v", got, err)
	}
}

func TestChoosingTheDefaultClears(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/small"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/big"); err != nil { // p/big is the default: the chat model
		t.Fatal(err)
	}
	sess, _, _ := f.st.Session(ctx, f.sid)
	if sess.SubagentModel != "" {
		t.Fatalf("SubagentModel = %q, want cleared so it keeps following chat", sess.SubagentModel)
	}
	if _, err := f.svc.Set(ctx, f.sid, "chat", "p/tiny"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/tiny" {
		t.Fatalf("ChildModel = %q, want it to follow the new chat model", got)
	}
}

func TestSetGlobalWritesConfigAndRouter(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.svc.Set(ctx, "", "title", "p/tiny"); err != nil {
		t.Fatal(err)
	}
	if got := f.svc.Router.Model("title"); got != "p/tiny" {
		t.Fatalf("router title = %q", got)
	}
	body, _ := os.ReadFile(f.cfg)
	if !strings.Contains(string(body), `title = "p/tiny"`) {
		t.Fatalf("config not written:\n%s", body)
	}
	if _, err := f.svc.Set(ctx, "", "title", "p/small"); err != nil { // the default
		t.Fatal(err)
	}
	if got := f.svc.Router.Model("title"); got != "p/small" {
		t.Fatalf("router title = %q after choosing the default", got)
	}
	body, _ = os.ReadFile(f.cfg)
	if strings.Contains(string(body), config.RoutingBegin) {
		t.Fatalf("override block left after choosing the default:\n%s", body)
	}
}

func TestSetRefusesWhatIsNotAvailable(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, tc := range []struct{ op, ref string }{
		{"chat", "p/unknown"},
		{"chat", "q/big"},
		{"nope", "p/big"},
		{"chat", "free text"},
	} {
		if _, err := f.svc.Set(ctx, f.sid, tc.op, tc.ref); !errors.Is(err, ErrInvalid) {
			t.Errorf("Set(%q, %q) err = %v, want ErrInvalid", tc.op, tc.ref, err)
		}
	}
	if _, err := f.svc.Set(ctx, "", "chat", "p/tiny"); !errors.Is(err, ErrInvalid) {
		t.Errorf("chat without a session: err = %v, want ErrInvalid", err)
	}
}

func TestChildModelPrecedence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/big" {
		t.Fatalf("no choices: %q, want the chat default", got)
	}
	if _, err := f.svc.Set(ctx, f.sid, "subagent", "p/small"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, ""); got != "p/small" {
		t.Fatalf("subagent choice: %q", got)
	}
	if got, _ := f.svc.ChildModel(ctx, f.sid, "p/tiny"); got != "p/tiny" {
		t.Fatalf("explicit: %q", got)
	}
	_, err := f.svc.ChildModel(ctx, f.sid, "p/unknown")
	if err == nil || !strings.Contains(err.Error(), "p/tiny") {
		t.Fatalf("unknown explicit model: err = %v, want one listing the available refs", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/models/ -run 'View|Set|Child'`
Expected: FAIL to compile.

- [ ] **Step 3: Implement** — `internal/models/service.go`:

```go
package models

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

const (
	// ScopeSession ops are chosen per session and stored on it.
	ScopeSession = "session"
	// ScopeGlobal ops are chosen for the whole daemon and stored in the
	// managed routing block.
	ScopeGlobal = "global"
)

// ErrInvalid marks a choice /model refuses: an unknown operation, a model
// that is not available, or a per-session op with no session.
var ErrInvalid = errors.New("invalid model choice")

// Op is one operation's state as every surface shows it: Selected gets
// "->", Default gets "*".
type Op struct {
	Op       string `json:"op"`
	Scope    string `json:"scope"`
	Selected string `json:"selected"`
	Default  string `json:"default"`
}

// View is GET /api/models: every op, and what can be chosen.
type View struct {
	Ops    []Op    `json:"ops"`
	Groups []Group `json:"groups"`
}

// Service reads and changes the per-operation choice.
type Service struct {
	Store      *store.Store
	Router     *router.Router
	Catalog    *Catalog
	ConfigPath string
}

// View reports every op for a session ("" for none: chat and subagent then
// show their defaults).
func (s *Service) View(ctx context.Context, sessionID string, fresh bool) (View, error) {
	var sess store.Session
	if sessionID != "" {
		got, ok, err := s.Store.Session(ctx, sessionID)
		if err != nil {
			return View{}, err
		}
		if !ok {
			return View{}, fmt.Errorf("%w: no session %s", ErrInvalid, sessionID)
		}
		sess = got
	}
	v := View{Groups: s.Catalog.List(ctx, fresh)}
	chatDefault := s.Router.Model(router.SiteChat)
	chat := chatDefault
	if sess.ChatModel != "" {
		chat = sess.ChatModel
	}
	for _, site := range router.Sites {
		op := Op{Op: site}
		switch site {
		case router.SiteChat:
			op.Scope, op.Default, op.Selected = ScopeSession, chatDefault, chat
		case router.SiteSubagent:
			// A sub-agent inherits the session's own model unless told
			// otherwise, so that is its default.
			op.Scope, op.Default, op.Selected = ScopeSession, chat, chat
			if sess.SubagentModel != "" {
				op.Selected = sess.SubagentModel
			}
		default:
			op.Scope, op.Default, op.Selected = ScopeGlobal, s.Router.Default(site), s.Router.Model(site)
		}
		v.Ops = append(v.Ops, op)
	}
	return v, nil
}

// Set makes ref the model for op. Choosing the op's default clears the
// choice, so it follows the config (or, for subagent, the chat model) again.
// Any other ref must be in a fresh catalog listing.
func (s *Service) Set(ctx context.Context, sessionID, op, ref string) (View, error) {
	if !router.ValidSite(op) {
		return View{}, fmt.Errorf("%w: unknown operation %q", ErrInvalid, op)
	}
	global := router.IsGlobalSite(op)
	if !global && sessionID == "" {
		return View{}, fmt.Errorf("%w: %s is chosen per session", ErrInvalid, op)
	}
	cur, err := s.View(ctx, sessionID, false)
	if err != nil {
		return View{}, err
	}
	for _, o := range cur.Ops {
		if o.Op == op && o.Default == ref {
			ref = ""
		}
	}
	if ref != "" && !s.Catalog.Has(ctx, ref) {
		return View{}, fmt.Errorf("%w: %s is not available for %s", ErrInvalid, ref, op)
	}
	if global {
		if err := config.SetRoutingOverride(s.ConfigPath, op, ref); err != nil {
			return View{}, err
		}
		if err := s.Router.SetOverride(op, ref); err != nil {
			return View{}, err
		}
	} else if err := s.Store.SetSessionModel(ctx, sessionID, op, ref); err != nil {
		return View{}, err
	}
	return s.View(ctx, sessionID, false)
}

// ChildModel picks the model for a sub-agent launched from parentID: the
// requested one if the launching agent named one, else the parent's
// sub-agent choice, else the parent's own model.
func (s *Service) ChildModel(ctx context.Context, parentID, requested string) (string, error) {
	if requested != "" {
		if !s.Catalog.Has(ctx, requested) {
			avail := s.Catalog.Refs(ctx)
			if len(avail) == 0 {
				return "", fmt.Errorf("model %q is not available, and no provider is listing any models right now", requested)
			}
			return "", fmt.Errorf("model %q is not available; choose one of: %s", requested, strings.Join(avail, ", "))
		}
		return requested, nil
	}
	sess, ok, err := s.Store.Session(ctx, parentID)
	if err != nil {
		return "", err
	}
	if ok && sess.SubagentModel != "" {
		return sess.SubagentModel, nil
	}
	if ok && sess.ChatModel != "" {
		return sess.ChatModel, nil
	}
	return s.Router.Model(router.SiteChat), nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 -race ./internal/models/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/models/service.go internal/models/service_test.go
git commit -m "models: choose a model per operation, per session or daemon-wide"   # plus the trailer
```

---

### Task 7: Agent uses the session's model

**Files:**
- Modify: `internal/agent/agent.go` (the `ref := a.Router.Model(site)` line in `loop`, plus a new method)
- Test: `internal/agent/session_model_test.go`

**Interfaces:**
- Consumes: `store.Session.ChatModel` (Task 3).

- [ ] **Step 1: Write the failing test** — `internal/agent/session_model_test.go`:

```go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
)

func TestChatAndSubagentTurnsUseTheSessionModel(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: "a"}, provider.ScriptTurn{Text: "b"}, provider.ScriptTurn{Text: "c"})
	a, st := harness(t, script, nil)
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "/ws")

	collect(t, mustRun(t, a, sid, router.SiteChat))
	if err := st.SetSessionModel(ctx, sid, "chat", "test/model-b"); err != nil {
		t.Fatal(err)
	}
	collect(t, mustRun(t, a, sid, router.SiteChat))
	collect(t, mustRun(t, a, sid, router.SiteSubagent))

	reqs := script.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].Model != "model-a" || reqs[1].Model != "model-b" || reqs[2].Model != "model-b" {
		t.Fatalf("models = %q, %q, %q; want model-a, then the session's model-b for chat and subagent",
			reqs[0].Model, reqs[1].Model, reqs[2].Model)
	}
}

func TestARemovedProviderFailsTheTurnLoudly(t *testing.T) {
	a, st := harness(t, provider.NewScript(provider.ScriptTurn{Text: "never"}), nil)
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "/ws")
	if err := st.SetSessionModel(ctx, sid, "chat", "gone/model"); err != nil {
		t.Fatal(err)
	}
	var failed error
	for _, ev := range collect(t, mustRun(t, a, sid, router.SiteChat)) {
		if ev.Err != nil {
			failed = ev.Err
		}
	}
	if failed == nil || !strings.Contains(failed.Error(), `session model "gone/model"`) || !strings.Contains(failed.Error(), "/model") {
		t.Fatalf("err = %v, want it to name the session model and /model", failed)
	}
}

func mustRun(t *testing.T, a *Agent, sid, site string) <-chan Event {
	t.Helper()
	ch, err := a.RunSite(context.Background(), sid, "hi", site)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}
```

(`harness` registers the scripted provider as `test` with default model `test/model-a`. If `collect` returns a different type than `[]Event`, adapt the loop to it; read `collect` in `agent_test.go` first.)

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -run 'SessionModel|RemovedProvider'`
Expected: FAIL (model-b never used; error text missing).

- [ ] **Step 3: Implement** — in `internal/agent/agent.go`:

Add the method (near `RunSite`):

```go
// modelFor picks one round's model. A conversation's turns and a
// sub-agent's turns run on the session's chosen model when /model or the
// launching agent set one, otherwise on the chat route: the subagent route
// no longer selects anything, so a child with no stored model runs on what
// its parent would. Every other site is the router's alone. chosen reports
// that the ref came from the session, so a failure can say so.
func (a *Agent) modelFor(ctx context.Context, sessionID, site string) (ref string, chosen bool, err error) {
	if site != router.SiteChat && site != router.SiteSubagent {
		return a.Router.Model(site), false, nil
	}
	sess, ok, err := a.Store.Session(ctx, sessionID)
	if err != nil {
		return "", false, err
	}
	if ok && sess.ChatModel != "" {
		return sess.ChatModel, true, nil
	}
	return a.Router.Model(router.SiteChat), false, nil
}
```

Replace in `loop`:

```go
		ref := a.Router.Model(site)
		p, model, price, err := a.Registry.Resolve(ref)
		if err != nil {
			return err
		}
```

with:

```go
		ref, chosen, err := a.modelFor(ctx, sessionID, site)
		if err != nil {
			return err
		}
		p, model, price, err := a.Registry.Resolve(ref)
		if err != nil {
			if chosen {
				return fmt.Errorf("session model %q: %w — pick another with /model", ref, err)
			}
			return err
		}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/agent/`
Expected: PASS (all).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/session_model_test.go
git commit -m "agent: chat and sub-agent turns run on the session's model"   # plus the trailer
```

---

### Task 8: Sub-agents choose their model

**Files:**
- Modify: `internal/subagent/supervisor.go`, `internal/tool/subagent/tools.go`
- Test: `internal/subagent/model_test.go`, `internal/tool/subagent/model_test.go` (create)

**Interfaces:**
- Consumes: `(*store.Store).SetSessionModel` (Task 3). `models.Service` satisfies the new interface but is NOT imported here.
- Produces: `subagent.ModelChooser` interface `ChildModel(ctx, parentID, requested string) (string, error)`; `(*Supervisor).SetModels(ModelChooser)`; `(*Supervisor).RunModel(ctx, parentID, prompt, model string) (Status, error)`; `(*Supervisor).SpawnModel(ctx, parentID, prompt, model string) (string, error)`. `Run`/`Spawn` keep their signatures and pass `""`.

- [ ] **Step 1: Write the failing tests**

`internal/subagent/model_test.go`:

```go
package subagent

import (
	"context"
	"errors"
	"testing"

	"github.com/codered/spore/internal/config"
)

type fakeChooser struct {
	ref string
	err error
	got []string
}

func (f *fakeChooser) ChildModel(_ context.Context, parentID, requested string) (string, error) {
	f.got = append(f.got, parentID+"|"+requested)
	return f.ref, f.err
}

func cfgOK() config.SubagentConfig {
	return config.SubagentConfig{MaxDepth: 3, MaxCostUSD: 10, MaxConcurrent: 4}
}

func TestRunModelStoresTheChosenModelOnTheChild(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	ch := &fakeChooser{ref: "p/small"}
	sup.SetModels(ch)
	parent := parentSession(t, st)

	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "p/small"); err != nil {
		t.Fatal(err)
	}
	if len(ch.got) != 1 || ch.got[0] != parent+"|p/small" {
		t.Fatalf("chooser calls = %v", ch.got)
	}
	kid, ok, err := st.Session(context.Background(), r.ran[0])
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if kid.ChatModel != "p/small" {
		t.Fatalf("child ChatModel = %q", kid.ChatModel)
	}
}

func TestARefusedModelLaunchesNothing(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	sup.SetModels(&fakeChooser{err: errors.New(`model "x/y" is not available; choose one of: p/a`)})
	parent := parentSession(t, st)

	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "x/y"); err == nil {
		t.Fatal("want the chooser's error")
	}
	if len(r.ran) != 0 {
		t.Fatal("a child ran")
	}
	sessions, err := st.ListSessions(context.Background(), 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want only the parent", len(sessions))
	}
	runs, _ := st.SubagentRunsByParent(context.Background(), parent)
	if len(runs) != 0 {
		t.Fatalf("run rows = %d, want 0", len(runs))
	}
}

func TestWithoutAChooserAModelIsRefusedAndNoneIsFine(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	parent := parentSession(t, st)
	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "p/x"); err == nil {
		t.Fatal("a model was accepted with nothing to check it against")
	}
	if _, err := sup.Run(ctxFor(parent), parent, "task"); err != nil {
		t.Fatal(err)
	}
}
```

(Check `config.SubagentConfig` field names in `internal/config/config.go` and the existing tests' config literal; use the same.)

`internal/tool/subagent/model_test.go`:

```go
package subagent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBothLaunchToolsOfferAModelArgument(t *testing.T) {
	for _, tl := range New(nil)[:2] {
		var schema struct {
			Properties map[string]struct {
				Type        string `json:"type"`
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
			t.Fatal(err)
		}
		m, ok := schema.Properties["model"]
		if !ok || m.Type != "string" || !strings.Contains(m.Description, "/model") {
			t.Fatalf("%s: model property = %+v", tl.Name(), m)
		}
		for _, r := range schema.Required {
			if r == "model" {
				t.Fatalf("%s: model must be optional", tl.Name())
			}
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/subagent/ ./internal/tool/subagent/`
Expected: FAIL to compile / schema test fails.

- [ ] **Step 3: Implement**

`internal/subagent/supervisor.go`:

1. After the `Observer` interface:

```go
// ModelChooser picks a child's model: the one the launching agent named, or
// the parent session's choice. models.Service implements it; the interface
// keeps this package free of the catalog.
type ModelChooser interface {
	ChildModel(ctx context.Context, parentID, requested string) (string, error)
}
```

2. Field in `Supervisor` (after `observer`): `models ModelChooser`.

3. Methods:

```go
// SetModels installs the chooser. Without one, a child runs on the chat
// route and a requested model is refused.
func (s *Supervisor) SetModels(m ModelChooser) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.models = m
}

// childModel resolves a child's model before anything is created, so a
// refused model leaves no session and no run row behind.
func (s *Supervisor) childModel(ctx context.Context, parentID, requested string) (string, error) {
	s.mu.Lock()
	m := s.models
	s.mu.Unlock()
	if m == nil {
		if requested != "" {
			return "", fmt.Errorf("choosing a sub-agent's model is not available here; omit model")
		}
		return "", nil
	}
	return m.ChildModel(ctx, parentID, requested)
}
```

4. `start` gains a `model string` parameter (last). Right after `CreateChildSession` succeeds:

```go
	if model != "" {
		if err := s.store.SetSessionModel(ctx, childID, "chat", model); err != nil {
			return "", nil, err
		}
	}
```

5. Rename `Run` to `RunModel(ctx context.Context, parentID, prompt, model string) (Status, error)`; after the `admit` call add:

```go
	ref, err := s.childModel(ctx, parentID, model)
	if err != nil {
		return Status{}, err
	}
```

and pass `ref` to `start`. Then add:

```go
// Run launches a child on its parent's sub-agent model and blocks until it
// settles.
func (s *Supervisor) Run(ctx context.Context, parentID, prompt string) (Status, error) {
	return s.RunModel(ctx, parentID, prompt, "")
}
```

6. Same for `Spawn` → `SpawnModel(ctx, parentID, prompt, model string) (string, error)`, resolving `ref` after `admit` (returning `"", err`), passing it to `start`, plus a `Spawn` wrapper passing `""`.

`internal/tool/subagent/tools.go`:

1. Both `runTool.Schema` and `spawnTool.Schema` gain, inside `properties` after `prompt`:

```json
	    "model": {
	      "type": "string",
	      "description": "Optional provider/model ref to run the sub-agent on, from the models /model lists. Omit to use this session's sub-agent model, which is your own model unless the user chose another."
	    }
```

2. Both `Call` input structs gain `Model string \`json:"model"\``; `runTool.Call` calls `t.sup.RunModel(ctx, sess.ID, in.Prompt, in.Model)` and `spawnTool.Call` calls `t.sup.SpawnModel(ctx, sess.ID, in.Prompt, in.Model)`.

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 -race ./internal/subagent/ ./internal/tool/subagent/`
Expected: PASS (all, including existing).

- [ ] **Step 5: Commit**

```bash
git add internal/subagent/supervisor.go internal/subagent/model_test.go internal/tool/subagent/tools.go internal/tool/subagent/model_test.go
git commit -m "subagent: agent_run and agent_spawn take a model"   # plus the trailer
```

---

### Task 9: Daemon API and client

**Files:**
- Create: `internal/daemon/models.go`, `internal/daemon/models_test.go`
- Modify: `internal/daemon/server.go` (field, attach method, routes)
- Modify: `cmd/spore/client.go` (two methods)

**Interfaces:**
- Consumes: `models.Service`, `models.View`, `models.ErrInvalid` (Task 6); `router.IsGlobalSite`, `router.SiteChat`, `router.SiteSubagent` (Task 1).
- Produces: `daemon.ModelsJSON = models.View`; `(*Server).AttachModels(*models.Service)`; routes `GET /api/models`, `PUT /api/sessions/{id}/model`, `PUT /api/routing`; client methods `(*client).models(ctx, sessionID string, fresh bool) (daemon.ModelsJSON, error)` and `(*client).setModel(ctx, sessionID, op, ref string) (daemon.ModelsJSON, error)`.

- [ ] **Step 1: Write the failing tests** — `internal/daemon/models_test.go`:

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
)

// modelsServer is newTestServer plus a models service over two providers: one
// serving /v1/models and one that is unreachable.
func modelsServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv, ts := newTestServer(t)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gemma"},{"id":"unsloth/qwen"}]}`))
	}))
	t.Cleanup(up.Close)
	reg := provider.NewRegistry()
	reg.Register("jetson", openaicompat.New(up.URL+"/v1", "", nil), provider.ProviderPrice{})
	reg.Register("down", openaicompat.New("http://127.0.0.1:1/v1", "", nil), provider.ProviderPrice{})
	rt, err := router.New(nil, "jetson/gemma")
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("default_model = \"jetson/gemma\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv.AttachModels(&models.Service{Store: srv.store, Router: rt, Catalog: models.NewCatalog(reg, nil), ConfigPath: cfgPath})
	return ts, createTestSession(t, ts.URL)
}

func decodeModels(t *testing.T, res *http.Response) ModelsJSON {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", res.Status)
	}
	var v ModelsJSON
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func put(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestGetModelsListsOpsAndGroups(t *testing.T) {
	ts, id := modelsServer(t)
	res, err := http.Get(ts.URL + "/api/models?session=" + id)
	if err != nil {
		t.Fatal(err)
	}
	v := decodeModels(t, res)
	if len(v.Ops) != 6 || v.Ops[0].Op != "chat" {
		t.Fatalf("ops = %+v", v.Ops)
	}
	var sawDown, sawQwen bool
	for _, g := range v.Groups {
		if g.Provider == "down" && g.Error != "" {
			sawDown = true
		}
		for _, r := range g.Refs {
			if r == "jetson/unsloth/qwen" {
				sawQwen = true
			}
		}
	}
	if !sawDown || !sawQwen {
		t.Fatalf("groups = %+v", v.Groups)
	}
}

func TestPutSessionModelAndRouting(t *testing.T) {
	ts, id := modelsServer(t)
	v := decodeModels(t, put(t, ts.URL+"/api/sessions/"+id+"/model", map[string]string{"op": "chat", "ref": "jetson/unsloth/qwen"}))
	if v.Ops[0].Selected != "jetson/unsloth/qwen" {
		t.Fatalf("chat = %+v", v.Ops[0])
	}
	v = decodeModels(t, put(t, ts.URL+"/api/routing?session="+id, map[string]string{"op": "title", "ref": "jetson/unsloth/qwen"}))
	for _, o := range v.Ops {
		if o.Op == "title" && o.Selected != "jetson/unsloth/qwen" {
			t.Fatalf("title = %+v", o)
		}
	}
}

func TestPutRefusesWrongScopeAndUnavailableModels(t *testing.T) {
	ts, id := modelsServer(t)
	for _, tc := range []struct {
		url  string
		body map[string]string
	}{
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "title", "ref": "jetson/gemma"}},
		{ts.URL + "/api/routing", map[string]string{"op": "chat", "ref": "jetson/gemma"}},
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "chat", "ref": "jetson/nope"}},
		{ts.URL + "/api/sessions/" + id + "/model", map[string]string{"op": "chat", "ref": "down/anything"}},
	} {
		res := put(t, tc.url, tc.body)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("PUT %s %v: status = %d, want 400", tc.url, tc.body, res.StatusCode)
		}
	}
	res := put(t, ts.URL+"/api/sessions/nope/model", map[string]string{"op": "chat", "ref": "jetson/gemma"})
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want 404", res.StatusCode)
	}
}

func TestModelsWithoutAServiceIsUnavailable(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/models")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run Models`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

`internal/daemon/models.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// ModelsJSON is GET /api/models and the answer to both PUTs, so a client
// redraws from whichever it got last.
type ModelsJSON = models.View

// AttachModels installs /model's service. Without it the routes answer 503.
func (s *Server) AttachModels(m *models.Service) { s.models = m }

type modelChoice struct {
	Op  string `json:"op"`
	Ref string `json:"ref"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	id := r.URL.Query().Get("session")
	if id != "" {
		if _, ok := s.findSession(w, r, id); !ok {
			return
		}
	}
	v, err := s.models.View(r.Context(), id, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetSessionModel(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var in modelChoice
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	if in.Op != router.SiteChat && in.Op != router.SiteSubagent {
		writeError(w, http.StatusBadRequest, "%s is set for every session: PUT /api/routing", in.Op)
		return
	}
	s.applyModel(w, r, id, in)
}

func (s *Server) handleSetRouting(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	// The session is optional: it only makes the answer include that
	// session's chat and subagent rows.
	id := r.URL.Query().Get("session")
	if id != "" {
		if _, ok := s.findSession(w, r, id); !ok {
			return
		}
	}
	var in modelChoice
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	if !router.IsGlobalSite(in.Op) {
		writeError(w, http.StatusBadRequest, "%s is set per session: PUT /api/sessions/{id}/model", in.Op)
		return
	}
	s.applyModel(w, r, id, in)
}

func (s *Server) applyModel(w http.ResponseWriter, r *http.Request, id string, in modelChoice) {
	v, err := s.models.Set(r.Context(), id, in.Op, in.Ref)
	if errors.Is(err, models.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
```

`internal/daemon/server.go`:
- Import `"github.com/codered/spore/internal/models"`.
- In the `Server` struct after the `titler` field:

```go
	// models serves /model. Nil means the routes answer 503.
	models *models.Service
```

- In `buildMux`, after `api("GET /api/usage", s.handleUsage)`:

```go
	api("GET /api/models", s.handleModels)
	api("PUT /api/sessions/{id}/model", s.handleSetSessionModel)
	api("PUT /api/routing", s.handleSetRouting)
```

`cmd/spore/client.go` (after `usage`; add `"net/url"` and `"github.com/codered/spore/internal/router"` imports if missing):

```go
// models fetches what /model shows for a session.
func (c *client) models(ctx context.Context, sessionID string, fresh bool) (daemon.ModelsJSON, error) {
	q := url.Values{}
	if sessionID != "" {
		q.Set("session", sessionID)
	}
	if fresh {
		q.Set("fresh", "1")
	}
	path := "/api/models"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out daemon.ModelsJSON
	if err := c.do(ctx, "GET", path, nil, &out); err != nil {
		return daemon.ModelsJSON{}, err
	}
	return out, nil
}

// setModel chooses ref for op. chat and subagent belong to the session; the
// other ops are daemon-wide and go to /api/routing.
func (c *client) setModel(ctx context.Context, sessionID, op, ref string) (daemon.ModelsJSON, error) {
	path := "/api/sessions/" + sessionID + "/model"
	if router.IsGlobalSite(op) {
		path = "/api/routing"
		if sessionID != "" {
			path += "?session=" + url.QueryEscape(sessionID)
		}
	}
	var out daemon.ModelsJSON
	if err := c.do(ctx, "PUT", path, map[string]string{"op": op, "ref": ref}, &out); err != nil {
		return daemon.ModelsJSON{}, err
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ ./cmd/spore/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/daemon/models.go internal/daemon/models_test.go internal/daemon/server.go cmd/spore/client.go
git commit -m "daemon: /api/models and the routes that choose one"   # plus the trailer
```

---

### Task 10: `modelcmd`, plain-chat `/model`, wiring, docs

**Files:**
- Create: `internal/modelcmd/modelcmd.go`, `internal/modelcmd/modelcmd_test.go`, `cmd/spore/model_cmd.go`, `cmd/spore/model_cmd_test.go`
- Modify: `cmd/spore/chat.go` (`runPlainSlash`), `cmd/spore/wire.go` (`agentParts`, `buildAgent`, `buildServer`), `README.md`

**Interfaces:**
- Consumes: Tasks 1, 2, 5, 6, 8, 9.
- Produces (used by stages 2 and 3): `modelcmd.Option{Ref string; Selected, Default, Available bool}`, `modelcmd.Options(v models.View, op string) []Option`, `(Option).Choosable() bool`, `modelcmd.Overview(v models.View) string`, `modelcmd.Render(v models.View) string`, `modelcmd.Parse(args []string) (op string, n int, err error)`, `modelcmd.Pick(v models.View, op string, n int) (string, error)`, `modelcmd.Confirm(v models.View, op string) string`.

- [ ] **Step 1: Write the failing tests**

`internal/modelcmd/modelcmd_test.go`:

```go
package modelcmd

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
)

func view() models.View {
	return models.View{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "b/two", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "gone/x", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "b/two", Default: "b/two"},
		},
		Groups: []models.Group{
			{Provider: "a", Refs: []string{"a/one", "a/three"}},
			{Provider: "b", Refs: []string{"b/two"}},
			{Provider: "down", Refs: []string{}, Error: "connection refused"},
		},
	}
}

func TestOptionsOrderSelectedDefaultThenTheRest(t *testing.T) {
	opts := Options(view(), "chat")
	var refs []string
	for _, o := range opts {
		refs = append(refs, o.Ref)
	}
	if strings.Join(refs, ",") != "b/two,a/one,a/three" {
		t.Fatalf("order = %v", refs)
	}
	if !opts[0].Selected || !opts[1].Default || opts[2].Selected || opts[2].Default {
		t.Fatalf("marks = %+v", opts)
	}
}

func TestAnUnavailableSelectionIsShownButNotChoosable(t *testing.T) {
	opts := Options(view(), "title")
	if opts[0].Ref != "gone/x" || opts[0].Available || opts[0].Choosable() {
		t.Fatalf("first = %+v, want gone/x shown, unavailable, not choosable", opts[0])
	}
	if !opts[1].Default || !opts[1].Choosable() {
		t.Fatalf("default = %+v, want choosable", opts[1])
	}
}

func TestRenderMarksAndNumbers(t *testing.T) {
	out := Render(view())
	for _, want := range []string{
		"chat", "-> b/two", "* a/one", "(this session",
		"   1 -> b/two",
		"   2    a/one *",
		"gone/x (unavailable)",
		"! down: connection refused",
		"/model <operation> <number>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render lacks %q:\n%s", want, out)
		}
	}
}

func TestParse(t *testing.T) {
	if op, n, err := Parse(nil); op != "" || n != 0 || err != nil {
		t.Fatalf("Parse(nil) = %q %d %v", op, n, err)
	}
	if op, n, err := Parse([]string{"title", "2"}); op != "title" || n != 2 || err != nil {
		t.Fatalf("Parse(title 2) = %q %d %v", op, n, err)
	}
	for _, bad := range [][]string{{"chat"}, {"nope", "1"}, {"chat", "x"}, {"chat", "0"}, {"chat", "1", "2"}, {"chat", "a/one"}} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%v) succeeded", bad)
		}
	}
}

func TestPick(t *testing.T) {
	v := view()
	if ref, err := Pick(v, "chat", 3); ref != "a/three" || err != nil {
		t.Fatalf("Pick(chat 3) = %q %v", ref, err)
	}
	if _, err := Pick(v, "chat", 4); err == nil {
		t.Fatal("out of range accepted")
	}
	if _, err := Pick(v, "title", 1); err == nil {
		t.Fatal("unavailable option accepted")
	}
}

func TestConfirmSaysWhereItApplies(t *testing.T) {
	if got := Confirm(view(), "chat"); !strings.Contains(got, "this session") {
		t.Fatalf("chat confirm = %q", got)
	}
	if got := Confirm(view(), "compaction"); !strings.Contains(got, "every session") || !strings.Contains(got, "default") {
		t.Fatalf("compaction confirm = %q", got)
	}
}
```

`cmd/spore/model_cmd_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
)

func TestPlainModelListsThenChoosesByNumber(t *testing.T) {
	v := models.View{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "a/one", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "a/one", Default: "a/one"},
		},
		Groups: []models.Group{{Provider: "a", Refs: []string{"a/one", "a/two"}}},
	}
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		if r.Method == http.MethodPut {
			v.Ops[2].Selected = "a/two"
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/model", false, &out)
	if err != nil || !handled {
		t.Fatalf("/model: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out.String(), "-> a/one") {
		t.Fatalf("listing = %q", out.String())
	}

	out.Reset()
	handled, err = runPlainSlash(context.Background(), c, "s1", "/model title 2", false, &out)
	if err != nil || !handled {
		t.Fatalf("/model title 2: handled=%v err=%v", handled, err)
	}
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "PUT /api/routing") || !strings.Contains(last, `"ref":"a/two"`) {
		t.Fatalf("last call = %q", last)
	}
	if !strings.Contains(out.String(), "title -> a/two") || !strings.Contains(out.String(), "every session") {
		t.Fatalf("confirm = %q", out.String())
	}
}

func TestPlainModelRejectsAFreeTextRef(t *testing.T) {
	c := &client{}
	handled, err := runPlainSlash(context.Background(), c, "s1", "/model chat a/two", false, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want a usage error", handled, err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/modelcmd/ ./cmd/spore/ -run 'Model|Options|Render|Parse|Pick|Confirm'`
Expected: FAIL to compile.

- [ ] **Step 3a: Implement `internal/modelcmd/modelcmd.go`**:

```go
// Package modelcmd is /model's text form and the option order every
// surface uses, so the TUI, the web UI, Discord and plain chat number and
// mark the same list the same way.
package modelcmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// Option is one choice for an op.
type Option struct {
	Ref       string
	Selected  bool
	Default   bool
	Available bool
}

// Choosable reports whether picking it can succeed: the default always can,
// because choosing it clears the override rather than naming a model.
func (o Option) Choosable() bool { return o.Available || o.Default }

func find(v models.View, op string) (models.Op, bool) {
	for _, o := range v.Ops {
		if o.Op == op {
			return o, true
		}
	}
	return models.Op{}, false
}

// Options lists an op's choices: the selected model, the default when it
// differs, then every other available model by provider.
func Options(v models.View, op string) []Option {
	o, ok := find(v, op)
	if !ok {
		return nil
	}
	avail := map[string]bool{}
	for _, g := range v.Groups {
		for _, r := range g.Refs {
			avail[r] = true
		}
	}
	var out []Option
	seen := map[string]bool{}
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, Option{Ref: ref, Selected: ref == o.Selected, Default: ref == o.Default, Available: avail[ref]})
	}
	add(o.Selected)
	add(o.Default)
	for _, g := range v.Groups {
		for _, r := range g.Refs {
			add(r)
		}
	}
	return out
}

func where(scope string) string {
	if scope == models.ScopeGlobal {
		return "every session"
	}
	return "this session"
}

// Overview is one line per op: what it runs on, and the default when that
// differs.
func Overview(v models.View) string {
	var b strings.Builder
	for _, o := range v.Ops {
		if o.Selected == o.Default {
			fmt.Fprintf(&b, "  %-10s -> %s *  (%s)\n", o.Op, o.Selected, where(o.Scope))
		} else {
			fmt.Fprintf(&b, "  %-10s -> %s  (%s; * %s)\n", o.Op, o.Selected, where(o.Scope), o.Default)
		}
	}
	return b.String()
}

// Render is the whole text form: the overview, each op's numbered list,
// and any provider that could not be listed.
func Render(v models.View) string {
	var b strings.Builder
	b.WriteString("Models per operation (-> selected, * default):\n")
	b.WriteString(Overview(v))
	for _, o := range v.Ops {
		fmt.Fprintf(&b, "\n%s (%s):\n", o.Op, where(o.Scope))
		for i, opt := range Options(v, o.Op) {
			mark := "  "
			if opt.Selected {
				mark = "->"
			}
			line := fmt.Sprintf("  %2d %s %s", i+1, mark, opt.Ref)
			if opt.Default {
				line += " *"
			}
			if !opt.Available {
				line += " (unavailable)"
			}
			b.WriteString(line + "\n")
		}
	}
	for _, g := range v.Groups {
		if g.Error != "" {
			fmt.Fprintf(&b, "\n  ! %s: %s\n", g.Provider, g.Error)
		}
	}
	b.WriteString("\nChoose with /model <operation> <number>.\n")
	return b.String()
}

const usage = "usage: /model, or /model <operation> <number> (operations: chat, compaction, title, classify, refinement, subagent)"

// Parse reads the words after /model: none shows the list; an operation and
// a number choose. A model ref is refused on purpose: choices come from the
// list.
func Parse(args []string) (op string, n int, err error) {
	if len(args) == 0 {
		return "", 0, nil
	}
	if len(args) != 2 || !router.ValidSite(args[0]) {
		return "", 0, fmt.Errorf("%s", usage)
	}
	n, err = strconv.Atoi(args[1])
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("%s", usage)
	}
	return args[0], n, nil
}

// Pick returns the ref of option n (1-based) for op.
func Pick(v models.View, op string, n int) (string, error) {
	opts := Options(v, op)
	if n < 1 || n > len(opts) {
		return "", fmt.Errorf("%s has options 1-%d", op, len(opts))
	}
	o := opts[n-1]
	if !o.Choosable() {
		return "", fmt.Errorf("%s is not available right now", o.Ref)
	}
	return o.Ref, nil
}

// Confirm says what op now runs on and where that applies.
func Confirm(v models.View, op string) string {
	o, _ := find(v, op)
	s := fmt.Sprintf("%s -> %s (%s)", op, o.Selected, where(o.Scope))
	if o.Selected == o.Default {
		s += ", the default"
	}
	return s + "\n"
}
```

- [ ] **Step 3b: `cmd/spore/model_cmd.go`**:

```go
package main

import (
	"context"
	"fmt"
	"io"

	"github.com/codered/spore/internal/modelcmd"
)

// runModelCommand is /model in the line-oriented loop: no arguments prints
// the numbered lists, "<op> <n>" chooses from them.
func runModelCommand(ctx context.Context, c *client, sessionID string, args []string, out io.Writer) error {
	op, n, err := modelcmd.Parse(args)
	if err != nil {
		return err
	}
	v, err := c.models(ctx, sessionID, false)
	if err != nil {
		return err
	}
	if op == "" {
		_, err = fmt.Fprint(out, modelcmd.Render(v))
		return err
	}
	ref, err := modelcmd.Pick(v, op, n)
	if err != nil {
		return err
	}
	if v, err = c.setModel(ctx, sessionID, op, ref); err != nil {
		return err
	}
	_, err = fmt.Fprint(out, modelcmd.Confirm(v, op))
	return err
}
```

In `cmd/spore/chat.go` `runPlainSlash`, before `switch text {` add (import `"strings"` if needed):

```go
	if text == "/model" || strings.HasPrefix(text, "/model ") {
		return true, runModelCommand(ctx, c, sessionID, strings.Fields(text)[1:], out)
	}
```

- [ ] **Step 3c: Wiring in `cmd/spore/wire.go`**:

1. Imports: `"github.com/codered/spore/internal/models"`.
2. `agentParts` gains `models *models.Service`.
3. In `buildAgent`, right after `rt, err := router.New(...)` and its error check:

```go
	for site, ref := range cfg.Routing.Override {
		if err := rt.SetOverride(site, ref); err != nil {
			return nil, agentParts{}, fmt.Errorf("routing.override.%s: %w", site, err)
		}
	}
	if rt.RuleMatches(router.SiteSubagent) {
		slog.Default().Warn("a [[route]] matches \"subagent\" but no longer picks the sub-agent model: sub-agents run on their parent's model, or the one /model or agent_run chose")
	}
```

4. Right after `sup := subagent.New(st, cfg.Subagents)`:

```go
	// /model's service: the supervisor asks it for each child's model, and
	// the daemon serves it.
	modelSvc := &models.Service{Store: st, Router: rt, Catalog: models.NewCatalog(reg, cfg.ConfiguredRefs()), ConfigPath: cfg.Path}
	sup.SetModels(modelSvc)
```

5. The final return becomes `agentParts{host: host, mirror: mir, sup: sup, recall: recallBackend, models: modelSvc}`.
6. In `buildServer`, after `srv.AttachSubagents(parts.sup)`: `srv.AttachModels(parts.models)`.

- [ ] **Step 3d: README** — in `README.md`, find the routing/config section (search for `[[route]]` near "Call sites: chat, compaction, title, classify, refinement, subagent") and add after that config example:

```markdown
### Choosing models while spore runs: `/model`

`/model` shows which model each operation runs on (`->`) and its default
from `config.toml` (`*`), then the models your providers serve right now
(each OpenAI-compatible provider is asked for `/v1/models`; Anthropic
providers offer the refs your config names). Choices come only from that
list.

- `chat` and `subagent` are chosen **per session**. A sub-agent runs on its
  parent's model unless the session chose a sub-agent model, or the parent
  passed `model` to `agent_run` / `agent_spawn`. A `[[route]]` for
  `subagent` no longer selects anything.
- `compaction`, `title`, `classify` and `refinement` are chosen **for every
  session**, take effect at once, and are written to a spore-managed
  `[routing.override]` block in `config.toml`. Choosing the `*` default
  removes the override.

In plain chat, `/model` prints numbered lists and `/model <operation> <number>`
chooses.
```

- [ ] **Step 4: Run the full gate**

Run each; all must pass:
```bash
go vet -tags sqlite_fts5 ./...
make fmtcheck
make lint
make tidycheck
go test -tags sqlite_fts5 -race ./...
```

- [ ] **Step 5: Commit**

```bash
git add internal/modelcmd/modelcmd.go internal/modelcmd/modelcmd_test.go cmd/spore/model_cmd.go cmd/spore/model_cmd_test.go cmd/spore/chat.go cmd/spore/wire.go README.md
git commit -m "chat: /model lists and chooses models per operation"   # plus the trailer
```
