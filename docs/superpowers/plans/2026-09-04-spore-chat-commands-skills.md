# Chat commands and skills — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give spore five chat commands (`/compact`, `/context`, `/usage`, `/clear`, `/skills`) over a new daemon endpoint, and the skills subsystem `/skills` reports on.

**Architecture:** Commands are daemon behaviour: `POST /api/sessions/{id}/commands` dispatches through a table in `internal/daemon/commands.go` and returns rendered text, so the CLI and the web UI stay thin clients over one API. Skills are markdown files under a directory outside `policy.Workspace`, indexed by name and description in every prompt and pulled in as tool results by `skill_load`; `skill_install` is the single, ask-gated, non-learnable write path.

**Tech Stack:** Go, SQLite (FTS5, `-tags sqlite_fts5` on every build and test), Bubble Tea, vanilla JS.

**Spec:** `docs/superpowers/specs/2026-09-04-spore-chat-commands-skills-design.md`

## Global Constraints

- Every build and test needs the FTS5 tag: `make build`, `make test`, `make vet` (which wrap `-tags sqlite_fts5`). A bare `go test ./...` does not compile.
- The core never imports a transport (design spec invariant 1). `internal/agent`, `internal/skill` and `internal/store` must not import `internal/daemon`.
- `internal/skill` is filesystem-only: no database handle, no session knowledge, mirroring `internal/memory`.
- Policy tests build their `config.PolicyConfig` explicitly. `config.Load` adds the baseline deny; a policy test built on a bare `config.Default()` silently loses its security assertions.
- Wire event type strings are append-only API (`internal/daemon/event.go:13-21`).
- Commit after every task, with the message given in the task's last step.

---

### Task 1: `internal/skill` — the file layer

**Files:**
- Create: `internal/skill/skill.go`
- Create: `internal/skill/cache.go`
- Test: `internal/skill/skill_test.go`, `internal/skill/cache_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `skill.Skill{Name, Description, Body, Path string}`; `skill.Load(dir string) ([]Skill, []error)`; `skill.Read(dir, name string) (Skill, error)`; `skill.Write(dir string, s Skill) error`; `skill.ValidName(name string) error`; `skill.Dir(dir, name string) (string, error)`; `skill.Render(s Skill) string`; `skill.ErrReadDir`; `skill.Cache` with `NewCache(dir string) *Cache`, `(*Cache).Reload() []error`, `(*Cache).Skills() []Skill`, `(*Cache).Dir() string`; `skill.Caches` with `NewCaches() *Caches` and `(*Caches).Skills(dir string) []Skill`.

- [ ] **Step 1: Write the failing tests for load and validation**

`internal/skill/skill_test.go`:

```go
package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const good = "---\nname: release-checklist\ndescription: How to cut a release\n---\n\nTag from master only.\n"

func TestLoadSortsAndParses(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	write(t, dir, "aaa-first", strings.Replace(good, "release-checklist", "aaa-first", 1))

	skills, errs := Load(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(skills) != 2 || skills[0].Name != "aaa-first" || skills[1].Name != "release-checklist" {
		t.Fatalf("want two skills sorted by name, got %+v", skills)
	}
	if skills[1].Description != "How to cut a release" || skills[1].Body != "Tag from master only." {
		t.Fatalf("bad parse: %+v", skills[1])
	}
}

func TestLoadMissingDirIsEmpty(t *testing.T) {
	skills, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if len(skills) != 0 || len(errs) != 0 {
		t.Fatalf("want no skills and no error, got %v %v", skills, errs)
	}
}

func TestLoadNameMustMatchDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "other-name", good)
	skills, errs := Load(dir)
	if len(skills) != 0 || len(errs) != 1 {
		t.Fatalf("want one error and no skill, got %v %v", skills, errs)
	}
	if !strings.Contains(errs[0].Error(), "does not match") {
		t.Fatalf("error should name the mismatch: %v", errs[0])
	}
}

func TestLoadOneBrokenSkillCostsOneSkill(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	write(t, dir, "broken", "no frontmatter here")
	skills, errs := Load(dir)
	if len(skills) != 1 || len(errs) != 1 {
		t.Fatalf("want one skill and one error, got %v %v", skills, errs)
	}
}

func TestValidNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../escape", "a/b", "Upper", "", ".", "trailing-", "dot.name"} {
		if err := ValidName(bad); err == nil {
			t.Fatalf("ValidName(%q) should fail", bad)
		}
	}
	if err := ValidName("release-checklist"); err != nil {
		t.Fatalf("ValidName rejected a good name: %v", err)
	}
}

func TestReadRoundTripsWrite(t *testing.T) {
	dir := t.TempDir()
	in := Skill{Name: "release-checklist", Description: "How to cut a release", Body: "Tag from master only."}
	if err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(dir, "release-checklist")
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Description != in.Description || out.Body != in.Body {
		t.Fatalf("round trip changed the skill: %+v", out)
	}
}

func TestWriteRejectsBadName(t *testing.T) {
	if err := Write(t.TempDir(), Skill{Name: "../evil", Description: "d", Body: "b"}); err == nil {
		t.Fatal("Write must reject a name that is not kebab-case")
	}
}

func TestReadRejectsBadName(t *testing.T) {
	if _, err := Read(t.TempDir(), "../../etc"); err == nil {
		t.Fatal("Read must reject a name that is not kebab-case")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/skill/...`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `internal/skill/skill.go`**

Mirror `internal/memory/memory.go` closely: the same hand-written frontmatter parser (two keys here, not three), the same atomic write, the same per-file error rule.

```go
// Package skill owns spore's skill files: the human-editable markdown under
// the skills directory that the model may pull into a turn. The file is the
// source of truth, so this package is filesystem-only: it holds no database
// handle and knows nothing about sessions.
//
// The directory sits outside policy.Workspace, so the filesystem tools cannot
// reach it and this package does its own confinement instead.
package skill

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Skill is one directory holding a SKILL.md.
type Skill struct {
	Name        string
	Description string
	Body        string
	Path        string
}

// ErrReadDir marks a directory-level read failure, which Cache treats
// differently from a per-file one.
var ErrReadDir = errors.New("read skills dir")

// nameRE repeats memory's rule rather than importing it: three lines is the
// better trade against a dependency between two independent filesystem
// packages, and the rule is security-relevant here because skill_load turns a
// model-supplied string into a path.
var nameRE = regexp.MustCompile(`\A[a-z0-9]+(-[a-z0-9]+)*\z`)

func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("skill name %q must be lowercase kebab-case (letters, digits and single hyphens)", name)
	}
	return nil
}

// Dir is the only place a name becomes a directory path.
func Dir(dir, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func (s Skill) Validate() error {
	if err := ValidName(s.Name); err != nil {
		return err
	}
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("skill description is required: it is all the model sees until the skill is loaded")
	}
	if strings.ContainsAny(s.Description, "\r\n") {
		return errors.New("skill description must be a single line")
	}
	if strings.TrimSpace(s.Body) == "" {
		return errors.New("skill body is required")
	}
	return nil
}

// Load reads every skill in dir, sorted by name. Errors are per-file: a human
// edits these by hand, so one broken skill costs exactly one skill and never a
// whole turn. A missing directory is zero skills and no error.
func Load(dir string) ([]Skill, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("%w %s: %v", ErrReadDir, dir, err)}
	}
	var skills []Skill
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue // a loose file is not a skill; dotdirs hold temporary writes
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // a directory with no SKILL.md is not a skill
			}
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		s, err := parse(string(data))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		if s.Name != e.Name() {
			errs = append(errs, fmt.Errorf("%s: frontmatter name %q does not match the directory name", e.Name(), s.Name))
			continue
		}
		s.Path = path
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, errs
}

// Read loads one skill by name.
func Read(dir, name string) (Skill, error) {
	d, err := Dir(dir, name)
	if err != nil {
		return Skill{}, err
	}
	data, err := os.ReadFile(filepath.Join(d, "SKILL.md"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Skill{}, fmt.Errorf("no skill named %q", name)
		}
		return Skill{}, err
	}
	s, err := parse(string(data))
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", name, err)
	}
	if s.Name != name {
		return Skill{}, fmt.Errorf("%s: frontmatter name %q does not match the directory name", name, s.Name)
	}
	s.Path = filepath.Join(d, "SKILL.md")
	return s, nil
}

// parse reads the fixed two-key frontmatter. This is not YAML and does not
// pretend to be: two known keys do not justify a dependency, and a hand parser
// gives error messages that name the actual problem.
func parse(text string) (Skill, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Skill{}, errors.New("missing opening --- frontmatter delimiter")
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Skill{}, errors.New("missing closing --- frontmatter delimiter")
	}
	var s Skill
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Skill{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			s.Name = value
		case "description":
			s.Description = value
		default:
			return Skill{}, fmt.Errorf("unknown frontmatter key %q", strings.TrimSpace(key))
		}
	}
	s.Body = strings.TrimSpace(body)
	if err := s.Validate(); err != nil {
		return Skill{}, err
	}
	return s, nil
}

// Render is the on-disk form. Write and the tests both go through it so the
// format has exactly one definition.
func Render(s Skill) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(s.Name)
	b.WriteString("\ndescription: ")
	b.WriteString(s.Description)
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimSpace(s.Body))
	b.WriteString("\n")
	return b.String()
}

// Write validates, then replaces SKILL.md atomically so a reader never sees a
// half-written skill.
func Write(dir string, s Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	d, err := Dir(dir, s.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, ".SKILL-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(Render(s)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(d, "SKILL.md"))
}

// Exists reports whether a skill of this name is already on disk, so an
// install can tell a human it is about to update rather than create.
func Exists(dir, name string) bool {
	d, err := Dir(dir, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(d, "SKILL.md"))
	return err == nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/skill/...`
Expected: PASS.

- [ ] **Step 5: Write the failing cache tests**

`internal/skill/cache_test.go`:

```go
package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheReloadPicksUpNewSkills(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(c.Skills()) != 0 {
		t.Fatal("a fresh directory has no skills")
	}
	write(t, dir, "release-checklist", good)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(c.Skills()) != 1 {
		t.Fatalf("want one skill after reload, got %d", len(c.Skills()))
	}
}

func TestCacheKeepsSkillsOnDirectoryFailure(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	c := NewCache(dir)
	c.Reload()
	if len(c.Skills()) != 1 {
		t.Fatal("setup failed")
	}
	// Make the directory unreadable; a transient failure must not blank the
	// cached set.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip("cannot chmod in this environment")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	errs := c.Reload()
	if len(errs) == 0 {
		t.Fatal("an unreadable directory must be reported")
	}
	if len(c.Skills()) != 1 {
		t.Fatal("the cache must keep its skills when the directory read fails")
	}
}

func TestCachesAreKeyedByDirectory(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "release-checklist", good)
	write(t, b, "aaa-first", strings.Replace(good, "release-checklist", "aaa-first", 1))
	cs := NewCaches()
	if got := cs.Skills(a); len(got) != 1 || got[0].Name != "release-checklist" {
		t.Fatalf("wrong skills for dir a: %+v", got)
	}
	if got := cs.Skills(b); len(got) != 1 || got[0].Name != "aaa-first" {
		t.Fatalf("wrong skills for dir b: %+v", got)
	}
	if got := cs.Skills(""); got != nil {
		t.Fatalf("an empty directory means no skills, got %+v", got)
	}
	_ = filepath.Join
}
```

- [ ] **Step 6: Run the cache tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/skill/ -run Cache`
Expected: FAIL — `NewCache` undefined.

- [ ] **Step 7: Write `internal/skill/cache.go`**

`Cache` mirrors `memory.Cache`. `Caches` mirrors `workspace.Describers`: one cache per directory, because under `scope = "workspace"` each session reads a directory of its own.

```go
package skill

import (
	"errors"
	"sync"
	"time"
)

// Cache holds the loaded skills for one directory. Assembly runs on every
// turn and a directory scan per turn buys nothing, so the set is reloaded on
// a TTL and after a write.
type Cache struct {
	dir     string
	mu      sync.RWMutex
	skills  []Skill
	errs    []error
	loaded  time.Time
}

func NewCache(dir string) *Cache { return &Cache{dir: dir} }

func (c *Cache) Dir() string { return c.dir }

// Reload rereads the directory and returns the per-file errors so the caller
// can warn about them. On a directory-level failure the cache preserves its
// existing skills: a transient permission problem or an unmounted volume must
// not silently blank the set.
func (c *Cache) Reload() []error {
	skills, errs := Load(c.dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	var dirErr bool
	for _, e := range errs {
		if errors.Is(e, ErrReadDir) {
			dirErr = true
			break
		}
	}
	if !dirErr {
		c.skills = skills
	}
	c.errs = errs
	c.loaded = time.Now()
	return errs
}

// Skills returns a copy: a caller that trims for a token budget must not be
// able to edit the shared set.
func (c *Cache) Skills() []Skill {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Skill, len(c.skills))
	copy(out, c.skills)
	return out
}

// Errors returns the per-file errors from the last reload, which /skills
// reports so a skill with broken frontmatter is visible rather than missing.
func (c *Cache) Errors() []error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]error, len(c.errs))
	copy(out, c.errs)
	return out
}

// ttl bounds how stale the index may be. A skill written by hand appears
// within this window without a restart.
const ttl = 30 * time.Second

func (c *Cache) fresh() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return !c.loaded.IsZero() && time.Since(c.loaded) < ttl
}

// Caches holds one Cache per directory. Under scope = "workspace" every
// session root has its own skills directory, so one cache per process would
// have them fighting over a single set.
//
// Entries are never evicted. Under the default global scope there is exactly
// one, and the workspace scope bounds this by session history rather than by
// live sessions -- the same shape as internal/workspace's describer cache,
// and tracked in docs/backlog.md alongside it.
type Caches struct {
	mu sync.Mutex
	m  map[string]*Cache
}

func NewCaches() *Caches { return &Caches{m: map[string]*Cache{}} }

func (cs *Caches) cache(dir string) *Cache {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c, ok := cs.m[dir]
	if !ok {
		c = NewCache(dir)
		cs.m[dir] = c
	}
	return c
}

// Skills returns the skills in dir, reloading when the cached set is stale.
// An empty dir means skills are switched off for this session (a workspace
// scope with no session root), and is not an error.
func (cs *Caches) Skills(dir string) []Skill {
	if dir == "" {
		return nil
	}
	c := cs.cache(dir)
	if !c.fresh() {
		c.Reload()
	}
	return c.Skills()
}

// Errors returns the per-file errors for dir's last load.
func (cs *Caches) Errors(dir string) []error {
	if dir == "" {
		return nil
	}
	c := cs.cache(dir)
	if !c.fresh() {
		c.Reload()
	}
	return c.Errors()
}

// Invalidate drops dir's cached set so the next read sees the disk. The
// install tool calls it, which is the only way the set changes from inside
// spore.
func (cs *Caches) Invalidate(dir string) {
	if dir == "" {
		return
	}
	cs.cache(dir).Reload()
}
```

- [ ] **Step 8: Run the whole package**

Run: `go test -tags sqlite_fts5 ./internal/skill/...`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/skill
git commit -m "feat(skill): the skill file layer, mirroring internal/memory"
```

---

### Task 2: configuration — `[skills]` and `skill_budget`

**Files:**
- Modify: `internal/config/config.go` (the `Config` struct, `ContextConfig`, `Default()`, `Validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.SkillsConfig{Scope, Dir string}`; `Config.Skills SkillsConfig`; `ContextConfig.SkillBudget int`; `func (c *Config) SkillsDir(workspace string) string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestSkillsDirGlobalIgnoresWorkspace(t *testing.T) {
	c := Default()
	c.DataDir = "/data"
	if got := c.SkillsDir("/some/project"); got != "/data/skills" {
		t.Fatalf("global scope must ignore the session workspace, got %q", got)
	}
	if got := c.SkillsDir(""); got != "/data/skills" {
		t.Fatalf("global scope needs no workspace, got %q", got)
	}
}

func TestSkillsDirGlobalHonoursExplicitDir(t *testing.T) {
	c := Default()
	c.DataDir = "/data"
	c.Skills.Dir = "/elsewhere/skills"
	if got := c.SkillsDir("/some/project"); got != "/elsewhere/skills" {
		t.Fatalf("an explicit dir must win, got %q", got)
	}
}

func TestSkillsDirWorkspaceScope(t *testing.T) {
	c := Default()
	c.DataDir = "/data"
	c.Skills.Scope = "workspace"
	if got := c.SkillsDir("/some/project"); got != "/some/project/.spore/skills" {
		t.Fatalf("workspace scope must root at the session, got %q", got)
	}
	if got := c.SkillsDir(""); got != "" {
		t.Fatalf("a session with no root has no skills under workspace scope, got %q", got)
	}
}

func TestValidateRejectsUnknownSkillsScope(t *testing.T) {
	c := Default()
	c.Skills.Scope = "everywhere"
	if err := c.Validate(); err == nil {
		t.Fatal("an unknown skills scope must be rejected")
	}
}

func TestDefaultSkillBudget(t *testing.T) {
	if Default().Context.SkillBudget != 500 {
		t.Fatalf("want a default skill budget of 500, got %d", Default().Context.SkillBudget)
	}
}
```

Note: if the existing test file calls `Validate` by another name, match it — read `internal/config/config.go` for the validation entry point (`Validate` is called from `Load`) and use that name.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run Skill`
Expected: FAIL — `Skills` undefined.

- [ ] **Step 3: Add the config**

In `internal/config/config.go`, add to `ContextConfig` next to `FactBudget`:

```go
	// SkillBudget caps the estimated tokens of the skills index. Skills past
	// it are dropped from the index with the count stated, so a long list
	// never crowds out the conversation.
	SkillBudget int `toml:"skill_budget"`
```

Add the block and its field on `Config`:

```go
// SkillsConfig chooses where skills are read from and installed to.
type SkillsConfig struct {
	// Scope is "global" (one directory for every session, the default) or
	// "workspace" (a directory under each session's own root).
	//
	// Workspace scope is opt-in for a reason: a session rooted at a cloned
	// repository would load text the operator did not write into its prompt
	// index, and spore's policy model assumes the prompt is yours.
	Scope string `toml:"scope"`
	// Dir overrides the global directory. Ignored under workspace scope.
	Dir string `toml:"dir"`
}
```

```go
	Skills    SkillsConfig              `toml:"skills"`
```

`Default()` gains `Skills: SkillsConfig{Scope: "global"}` and `SkillBudget: 500` inside the existing `ContextConfig` literal.

`SkillsDir`:

```go
// SkillsDir is the skills directory for a session rooted at workspace. Under
// the default global scope every session shares one directory and workspace
// is ignored. Under workspace scope a session with no root of its own has no
// skills, which is reported as an empty path rather than an error.
func (c *Config) SkillsDir(workspace string) string {
	if c.Skills.Scope == "workspace" {
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
```

In validation, next to the `FactBudget` check:

```go
	switch c.Skills.Scope {
	case "", "global", "workspace":
	default:
		return fmt.Errorf("skills.scope must be global or workspace, got %q", c.Skills.Scope)
	}
	if c.Context.SkillBudget < 0 {
		return fmt.Errorf("context.skill_budget must not be negative")
	}
```

Also expand `~` in `Skills.Dir` wherever `DataDir` is expanded in `Load`, so `dir = "~/skills"` works.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/config/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config
git commit -m "feat(config): the [skills] block and context.skill_budget"
```

---

### Task 3: context assembly — the skills index and the breakdown

**Files:**
- Modify: `internal/agent/context.go`
- Modify: `internal/agent/agent.go` (`Snapshot`, the `Agent` struct)
- Test: `internal/agent/context_test.go`

**Interfaces:**
- Consumes: `skill.Skill` (Task 1); `config.ContextConfig.SkillBudget` (Task 2).
- Produces: `Snapshot.Skills []skill.Skill`; `agent.Breakdown{System, Environment, Facts, Skills, Summary, Messages int}` with `(Breakdown).Total() int`; `agent.SnapshotBreakdown(snap Snapshot, cfg config.ContextConfig) Breakdown`; `Agent.Skills func(root string) []skill.Skill`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/context_test.go`:

```go
func TestSkillsSectionListsNamesAndDescriptions(t *testing.T) {
	snap := Snapshot{Skills: []skill.Skill{
		{Name: "release-checklist", Description: "How to cut a release", Body: "long body here"},
	}}
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, SkillBudget: 500})
	if !strings.Contains(req.System, "release-checklist") ||
		!strings.Contains(req.System, "How to cut a release") {
		t.Fatalf("the skills index must carry name and description:\n%s", req.System)
	}
	if strings.Contains(req.System, "long body here") {
		t.Fatal("a skill body must never be assembled; it arrives as a skill_load result")
	}
	if !strings.Contains(req.System, "skill_load") {
		t.Fatal("the index must tell the model how to load a skill")
	}
}

func TestSkillsSectionOverflowStatesTheCount(t *testing.T) {
	var many []skill.Skill
	for i := 0; i < 50; i++ {
		many = append(many, skill.Skill{
			Name:        fmt.Sprintf("skill-%02d", i),
			Description: strings.Repeat("x", 100),
		})
	}
	req := Assemble(Snapshot{Skills: many}, config.ContextConfig{MaxTokens: 1000, SkillBudget: 200})
	if !strings.Contains(req.System, "more skills did not fit") {
		t.Fatalf("overflow must be stated:\n%s", req.System)
	}
	if strings.Contains(req.System, "skill-49") {
		t.Fatal("the budget must actually drop skills")
	}
}

func TestSkillsSectionEmptyWhenNoSkills(t *testing.T) {
	req := Assemble(Snapshot{}, config.ContextConfig{MaxTokens: 1000, SkillBudget: 500})
	if strings.Contains(req.System, "Skills") {
		t.Fatalf("no skills means no section:\n%s", req.System)
	}
}

func TestBreakdownSumsToSnapshotTokens(t *testing.T) {
	cfg := config.ContextConfig{MaxTokens: 10000, FactBudget: 200, SkillBudget: 200}
	snap := Snapshot{
		System:      "you are spore",
		Environment: "cwd: /tmp",
		Summary:     "we talked about go",
		Facts:       []memory.Fact{{Name: "prefers-tabs", Description: "d", Type: "user", Body: "tabs"}},
		Skills:      []skill.Skill{{Name: "release-checklist", Description: "How to cut a release", Body: "b"}},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hello"}}},
		},
	}
	b := SnapshotBreakdown(snap, cfg)
	if b.Total() != SnapshotTokens(snap, cfg) {
		t.Fatalf("the breakdown must sum to the total: %d vs %d", b.Total(), SnapshotTokens(snap, cfg))
	}
	if b.System == 0 || b.Environment == 0 || b.Facts == 0 || b.Skills == 0 || b.Summary == 0 || b.Messages == 0 {
		t.Fatalf("every part with content must be counted: %+v", b)
	}
}
```

Add `"github.com/codered/spore/internal/skill"` and `"fmt"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -run 'Skills|Breakdown'`
Expected: FAIL — `Snapshot.Skills` undefined.

- [ ] **Step 3: Implement**

In `internal/agent/context.go`:

```go
// Skills is the index of loadable skills: names and descriptions only.
// Bodies enter the prompt as skill_load tool results, never here.
Skills []skill.Skill
```

added to `Snapshot`, and:

```go
// skillsSection renders the index. Only names and descriptions: a body is
// pulled in with skill_load, which keeps the cost of an unused skill at one
// line and makes loading visible in the transcript.
func skillsSection(skills []skill.Skill, budget int) string {
	if len(skills) == 0 {
		return ""
	}
	var section strings.Builder
	section.WriteString("\n\n## Skills you can load\n")
	section.WriteString("\nCall skill_load with a name to read one in full before you act on it.\n\n")
	used, dropped := 0, 0
	for _, s := range skills {
		line := "- " + s.Name + ": " + s.Description + "\n"
		cost := EstimateTokens(line)
		if used+cost > budget {
			dropped++
			continue
		}
		used += cost
		section.WriteString(line)
	}
	if dropped > 0 {
		fmt.Fprintf(&section, "\n(%d more skills did not fit this budget.)\n", dropped)
	}
	return section.String()
}
```

`Assemble` writes `skillsSection(snap.Skills, cfg.SkillBudget)` immediately after the facts section, keeping the spec's fixed order.

Replace `SnapshotTokens` with a breakdown and a sum:

```go
// Breakdown is the assembled size, part by part. /context reports it, and
// SnapshotTokens is defined as its total, so the number that drives
// compaction and the number the user is shown can never drift apart.
type Breakdown struct {
	System      int
	Environment int
	Facts       int
	Skills      int
	Summary     int
	Messages    int
}

func (b Breakdown) Total() int {
	return b.System + b.Environment + b.Facts + b.Skills + b.Summary + b.Messages
}

func SnapshotBreakdown(snap Snapshot, cfg config.ContextConfig) Breakdown {
	b := Breakdown{
		System:      EstimateTokens(snap.System),
		Environment: EstimateTokens(snap.Environment),
		Summary:     EstimateTokens(snap.Summary),
		Facts:       EstimateTokens(factsSection(snap.Facts, cfg.FactBudget)),
		Skills:      EstimateTokens(skillsSection(snap.Skills, cfg.SkillBudget)),
	}
	for _, m := range snap.Messages {
		b.Messages += messageTokens(m)
	}
	return b
}

// SnapshotTokens estimates the assembled size of a snapshot.
func SnapshotTokens(snap Snapshot, cfg config.ContextConfig) int {
	return SnapshotBreakdown(snap, cfg).Total()
}
```

In `internal/agent/agent.go`, add the field beside `Env`:

```go
	// Skills returns the skills loadable from a session root, the same way
	// Env describes one: one agent serves every session, and under workspace
	// scope each has a skills directory of its own.
	Skills func(root string) []skill.Skill
```

and in `Snapshot`, immediately after the `Env` block:

```go
	if a.Skills != nil {
		snap.Skills = a.Skills(policy.WorkspaceFrom(ctx))
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/agent/...`
Expected: PASS, including the existing assembly tests — the skills section is empty when there are no skills, so no golden output changes.

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat(agent): assemble a skills index and report the context breakdown"
```

---

### Task 4: split `Compact` from `MaybeCompact`

**Files:**
- Modify: `internal/agent/compact.go:22`
- Test: `internal/agent/compact_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: `func (a *Agent) Compact(ctx context.Context, sessionID string) (folded int, before, after int, err error)`; `MaybeCompact` keeps its signature `func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/compact_test.go`, following the fixtures the existing tests in that file use:

```go
func TestCompactFoldsBelowTheThreshold(t *testing.T) {
	// A session far below compact_at: MaybeCompact must do nothing, and
	// Compact must fold anyway. That difference is the whole command.
	a, id := compactFixture(t, 30) // 30 messages, tiny bodies
	if err := a.MaybeCompact(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, through, _ := a.Store.Summary(context.Background(), id); through != 0 {
		t.Fatal("MaybeCompact must not fold a session below the threshold")
	}
	folded, before, after, err := a.Compact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 30-a.Cfg.Context.KeepRecent {
		t.Fatalf("want %d folded, got %d", 30-a.Cfg.Context.KeepRecent, folded)
	}
	if after >= before {
		t.Fatalf("compaction must shrink the estimate: %d -> %d", before, after)
	}
	if _, through, _ := a.Store.Summary(context.Background(), id); through == 0 {
		t.Fatal("the summary boundary must have moved")
	}
}

func TestCompactWithNothingOutsideTheProtectedWindow(t *testing.T) {
	a, id := compactFixture(t, 3) // fewer than KeepRecent
	folded, before, after, err := a.Compact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 0 {
		t.Fatalf("nothing should fold, got %d", folded)
	}
	if before != after {
		t.Fatal("a no-op compaction must not change the estimate")
	}
}
```

If `compactFixture` does not exist, write it in the test file: open a store with `storetest`-style helpers already used in `compact_test.go`, create a session, append N user/assistant message pairs, and build an `Agent` with a stub provider that returns a fixed summary string — copy the existing fixture in that file rather than inventing a new shape.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -run Compact`
Expected: FAIL — `a.Compact` undefined.

- [ ] **Step 3: Split the function**

`MaybeCompact` keeps the threshold test and nothing else:

```go
// MaybeCompact summarises the older part of a session once its assembled size
// passes context.compact_at of context.max_tokens. It is the turn path's
// entry point; /compact calls Compact directly, which is the only difference
// between the two.
func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error {
	snap, err := a.Snapshot(ctx, sessionID)
	if err != nil {
		return err
	}
	budget := int(float64(a.Cfg.Context.MaxTokens) * a.Cfg.Context.CompactAt)
	if SnapshotTokens(snap, a.Cfg.Context) <= budget {
		return nil
	}
	_, _, _, err = a.Compact(ctx, sessionID)
	return err
}

// Compact folds everything outside the protected recent window into the
// summary, whatever the session's size. Original messages are never deleted:
// only the summary boundary moves, and Snapshot skips rows at or below it.
// It reports how many messages were folded and the estimated size before and
// after, which is what /compact shows.
func (a *Agent) Compact(ctx context.Context, sessionID string) (folded, before, after int, err error) {
	// ... the body that MaybeCompact has today, from a.Snapshot down to the
	// SetSummary call, with these changes:
	//   * before = SnapshotTokens(snap, a.Cfg.Context) taken at the top
	//   * the "nothing outside the protected window" branch returns
	//     (0, before, before, nil)
	//   * after the SetSummary succeeds, re-snapshot and return
	//     (foldCount, before, SnapshotTokens(newSnap, a.Cfg.Context), nil)
}
```

Move the existing body verbatim; the only new code is the three counters.

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/agent/...`
Expected: PASS, including the existing compaction tests, which exercise `MaybeCompact` unchanged.

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "refactor(agent): split Compact from its threshold test"
```

---

### Task 5: the store — usage totals, the summary boundary, the empty-summary index

**Files:**
- Modify: `internal/store/store.go` (`SetSummary` at :303)
- Create: `internal/store/usage.go`
- Test: `internal/store/usage_test.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `store.UsageRow{Model string; Turns int; TokensIn, TokensOut int; CostUSD float64}`; `func (s *Store) SessionUsage(ctx context.Context, sessionID string) ([]UsageRow, error)`; `func (s *Store) TotalUsage(ctx context.Context) ([]UsageRow, error)`; `func (s *Store) LastSeq(ctx context.Context, sessionID string) (int, error)`.

- [ ] **Step 1: Write the failing tests**

`internal/store/usage_test.go`:

```go
package store

import (
	"context"
	"testing"
)

func TestSessionUsageGroupsByModel(t *testing.T) {
	st := openTestStore(t) // the helper the existing store tests use
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		model  string
		in, out int
		cost   float64
	}{
		{"anthropic/claude-opus-5", 100, 20, 0.10},
		{"anthropic/claude-opus-5", 300, 40, 0.30},
		{"ollama/qwen3:8b", 50, 10, 0},
	} {
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`),
			Model: m.model, TokensIn: m.in, TokensOut: m.out, CostUSD: m.cost,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.SessionUsage(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want one row per model, got %+v", rows)
	}
	var opus UsageRow
	for _, r := range rows {
		if r.Model == "anthropic/claude-opus-5" {
			opus = r
		}
	}
	if opus.Turns != 2 || opus.TokensIn != 400 || opus.TokensOut != 60 {
		t.Fatalf("wrong sums: %+v", opus)
	}
	if opus.CostUSD < 0.399 || opus.CostUSD > 0.401 {
		t.Fatalf("wrong cost: %+v", opus)
	}
}

func TestTotalUsageSpansSessions(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		id, err := st.CreateSession(ctx, "t", "/ws")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`),
			Model: "m", TokensIn: 10, TokensOut: 5, CostUSD: 0.01,
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.TotalUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Turns != 2 || rows[0].TokensIn != 20 {
		t.Fatalf("totals must span sessions: %+v", rows)
	}
}

func TestUsageOfAnEmptySessionIsEmpty(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, err := st.CreateSession(ctx, "t", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := st.SessionUsage(ctx, id)
	if err != nil || len(rows) != 0 {
		t.Fatalf("want no rows and no error, got %+v %v", rows, err)
	}
}

func TestLastSeqReportsTheNewestMessage(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "t", "/ws")
	if seq, err := st.LastSeq(ctx, id); err != nil || seq != 0 {
		t.Fatalf("an empty session has no last seq: %d %v", seq, err)
	}
	for i := 0; i < 3; i++ {
		if _, err := st.AppendMessage(ctx, Message{
			SessionID: id, Role: "user", BlocksJSON: []byte(`[]`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	seq, err := st.LastSeq(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if seq != 3 {
		t.Fatalf("want seq 3, got %d", seq)
	}
}

func TestSetSummaryWithEmptyTextIndexesNothing(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "t", "/ws")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: id, Role: "user", BlocksJSON: []byte(`[]`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, id, "a real summary", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, id, "", 1); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM recall_fts WHERE kind = 'summary' AND ref = ?`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("clearing a summary must leave no indexed document, got %d", n)
	}
}
```

Match `openTestStore`, the `Message` field names and the `recall_fts` column names to what `internal/store/store_test.go` and `internal/store/schema.go` actually use before running.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'Usage|LastSeq|EmptyText'`
Expected: FAIL — `SessionUsage` undefined.

- [ ] **Step 3: Implement**

`internal/store/usage.go`:

```go
package store

import "context"

// UsageRow is one model's consumption. /usage reports these per session and
// across every session; the numbers come from the columns AppendMessage has
// always written.
type UsageRow struct {
	Model     string  `json:"model"`
	Turns     int     `json:"turns"`
	TokensIn  int     `json:"tokens_in"`
	TokensOut int     `json:"tokens_out"`
	CostUSD   float64 `json:"cost_usd"`
}

const usageSelect = `SELECT model, count(*), sum(tokens_in), sum(tokens_out), sum(cost_usd)
  FROM messages WHERE tokens_in > 0 OR tokens_out > 0 OR cost_usd > 0`

func (s *Store) SessionUsage(ctx context.Context, sessionID string) ([]UsageRow, error) {
	return s.usage(ctx, usageSelect+` AND session_id = ? GROUP BY model ORDER BY model`, sessionID)
}

func (s *Store) TotalUsage(ctx context.Context) ([]UsageRow, error) {
	return s.usage(ctx, usageSelect+` GROUP BY model ORDER BY model`)
}

func (s *Store) usage(ctx context.Context, query string, args ...any) ([]UsageRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Model, &r.Turns, &r.TokensIn, &r.TokensOut, &r.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastSeq is the newest message's sequence number, or 0 for a session with no
// messages. /clear moves the summary boundary here.
func (s *Store) LastSeq(ctx context.Context, sessionID string) (int, error) {
	var seq int
	err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(max(seq), 0) FROM messages WHERE session_id = ?`, sessionID).Scan(&seq)
	return seq, err
}
```

In `SetSummary`, guard the index insert (the delete stays unconditional, so clearing removes the old document):

```go
	if strings.TrimSpace(summary) != "" {
		if err := insertIndex(ctx, tx, kindSummary, sessionID, sessionID, now, summary); err != nil {
			return err
		}
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/store/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store
git commit -m "feat(store): usage totals, LastSeq, and no index row for an empty summary"
```

---

### Task 6: the skill tools and their policy

**Files:**
- Create: `internal/tool/skill/skill.go`
- Test: `internal/tool/skill/skill_test.go`
- Modify: `internal/policy/guard.go:286` (`PatternFor`)
- Test: `internal/policy/guard_test.go`
- Modify: `internal/config/config.go:360-372` (default allow/ask lists and the remote profile)
- Modify: `cmd/spore/wire.go` (`buildTools`, `buildAgent`)

**Interfaces:**
- Consumes: `skill.Read`, `skill.Write`, `skill.Exists`, `skill.Caches` (Task 1); `config.SkillsDir` (Task 2); `Agent.Skills` (Task 3).
- Produces: `toolskill.New(cfg *config.Config, caches *skill.Caches) []tool.Tool` returning the `skill_load` and `skill_install` tools, imported as `toolskill "github.com/codered/spore/internal/tool/skill"`.

- [ ] **Step 1: Write the failing tool tests**

`internal/tool/skill/skill_test.go`:

```go
package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	skillfiles "github.com/codered/spore/internal/skill"
)

func fixture(t *testing.T) (*config.Config, *skillfiles.Caches, string) {
	t.Helper()
	data := t.TempDir()
	cfg := config.Default()
	cfg.DataDir = data
	dir := filepath.Join(data, "skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return cfg, skillfiles.NewCaches(), dir
}

func ctxFor(ws string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{ID: "s1", Workspace: ws})
}

func find(t *testing.T, tools []tool.Tool, name string) tool.Tool {
	t.Helper()
	for _, x := range tools {
		if x.Name() == name {
			return x
		}
	}
	t.Fatalf("no tool named %s", name)
	return nil
}

func TestSkillLoadReturnsTheBody(t *testing.T) {
	cfg, caches, dir := fixture(t)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: "release-checklist", Description: "How to cut a release", Body: "Tag from master only.",
	}); err != nil {
		t.Fatal(err)
	}
	load := find(t, New(cfg, caches), "skill_load")
	out, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"release-checklist"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tag from master only.") {
		t.Fatalf("want the body, got %q", out)
	}
	if !load.ReadOnly() {
		t.Fatal("skill_load must be read-only")
	}
}

func TestSkillLoadUnknownNameIsAToolError(t *testing.T) {
	cfg, caches, _ := fixture(t)
	load := find(t, New(cfg, caches), "skill_load")
	if _, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"nope"}`)); err == nil {
		t.Fatal("an unknown skill must be an error the model can read")
	}
}

func TestSkillLoadRejectsTraversal(t *testing.T) {
	cfg, caches, _ := fixture(t)
	load := find(t, New(cfg, caches), "skill_load")
	if _, err := load.Call(ctxFor("/ws"), json.RawMessage(`{"name":"../../etc/passwd"}`)); err == nil {
		t.Fatal("a traversal name must be rejected")
	}
}

func TestSkillInstallWritesAndInvalidates(t *testing.T) {
	cfg, caches, dir := fixture(t)
	tools := New(cfg, caches)
	install := find(t, tools, "skill_install")
	if install.ReadOnly() {
		t.Fatal("skill_install writes files; it must not be read-only")
	}
	_, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"release-checklist","description":"How to cut a release","body":"Tag from master only."}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := skillfiles.Read(dir, "release-checklist")
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "Tag from master only." {
		t.Fatalf("bad write: %+v", got)
	}
	// The index the next turn assembles must see it without a restart.
	names := caches.Skills(dir)
	if len(names) != 1 || names[0].Name != "release-checklist" {
		t.Fatalf("the cache must be invalidated by an install: %+v", names)
	}
}

func TestSkillInstallRejectsBadName(t *testing.T) {
	cfg, caches, _ := fixture(t)
	install := find(t, New(cfg, caches), "skill_install")
	if _, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"../evil","description":"d","body":"b"}`)); err == nil {
		t.Fatal("a traversal name must be rejected")
	}
}

func TestSkillInstallSaysWhenItUpdates(t *testing.T) {
	cfg, caches, dir := fixture(t)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: "release-checklist", Description: "old", Body: "old body",
	}); err != nil {
		t.Fatal(err)
	}
	install := find(t, New(cfg, caches), "skill_install")
	out, err := install.Call(ctxFor("/ws"), json.RawMessage(
		`{"name":"release-checklist","description":"new","body":"new body"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated") {
		t.Fatalf("an overwrite must say it updated: %q", out)
	}
}

func TestToolsAreOffWithoutASkillsDirectory(t *testing.T) {
	cfg, caches, _ := fixture(t)
	cfg.Skills.Scope = "workspace"
	load := find(t, New(cfg, caches), "skill_load")
	// A session with no root of its own has no skills directory.
	if _, err := load.Call(ctxFor(""), json.RawMessage(`{"name":"anything"}`)); err == nil {
		t.Fatal("a session with no skills directory must say so, not read the global one")
	}
}
```

Import `"github.com/codered/spore/internal/tool"` in the test file. Match `policy.WithSession` / `policy.Session` to the real constructor in `internal/policy/guard.go` — read it first; if the helper has another name, use that.

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/tool/skill/...`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write `internal/tool/skill/skill.go`**

```go
// Package skill exposes the skills directory to the model: skill_load reads
// one in, and skill_install is the single write path into a directory the
// filesystem tools cannot reach.
package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	skillfiles "github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/tool"
)

// New builds both skill tools around one cache set. The directory is resolved
// per call from the calling session's workspace, because under
// skills.scope = "workspace" each session reads a directory of its own.
func New(cfg *config.Config, caches *skillfiles.Caches) []tool.Tool {
	return []tool.Tool{loadTool{cfg: cfg, caches: caches}, installTool{cfg: cfg, caches: caches}}
}

func dirFor(cfg *config.Config, ctx context.Context) (string, error) {
	dir := cfg.SkillsDir(policy.WorkspaceFrom(ctx))
	if dir == "" {
		return "", fmt.Errorf("this session has no skills directory: skills.scope is \"workspace\" and the session has no workspace of its own")
	}
	return dir, nil
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

type loadTool struct {
	cfg    *config.Config
	caches *skillfiles.Caches
}

func (loadTool) Name() string { return "skill_load" }

func (loadTool) Description() string {
	return "Read one skill in full: a markdown document of instructions the user wrote for a " +
		"particular kind of work. The skills index in your context lists what is available by " +
		"name and description; load one before you act on the work it covers."
}

func (loadTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "description": "The skill's name, as listed in the skills index."}
	  },
	  "required": ["name"]
	}`)
}

// ReadOnly is true: this reads a file the user wrote and changes nothing.
func (loadTool) ReadOnly() bool { return true }

func (t loadTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name string `json:"name"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir, err := dirFor(t.cfg, ctx)
	if err != nil {
		return "", err
	}
	s, err := skillfiles.Read(dir, a.Name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("# %s\n\n%s\n", s.Name, s.Body), nil
}

type installTool struct {
	cfg    *config.Config
	caches *skillfiles.Caches
}

func (installTool) Name() string { return "skill_install" }

func (installTool) Description() string {
	return "Install a skill: write a markdown document of instructions into the user's skills " +
		"directory, where it is listed in every future conversation and can be loaded with " +
		"skill_load. Install one only when the user asks for it."
}

func (installTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "name": {"type": "string", "description": "Lowercase kebab-case identifier, e.g. release-checklist."},
	    "description": {"type": "string", "description": "One line saying when to load this skill."},
	    "body": {"type": "string", "description": "The skill itself, in markdown."}
	  },
	  "required": ["name", "description", "body"]
	}`)
}

// ReadOnly is false: this writes files, so the loop must not dispatch it
// alongside other calls.
func (installTool) ReadOnly() bool { return false }

func (t installTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir, err := dirFor(t.cfg, ctx)
	if err != nil {
		return "", err
	}
	updated := skillfiles.Exists(dir, a.Name)
	if err := skillfiles.Write(dir, skillfiles.Skill{
		Name: a.Name, Description: a.Description, Body: a.Body,
	}); err != nil {
		return "", err
	}
	// The index the next turn assembles is built from the cache, so an
	// install that did not invalidate it would be invisible until the TTL.
	t.caches.Invalidate(dir)
	verb := "installed"
	if updated {
		verb = "updated"
	}
	return fmt.Sprintf("%s skill %q", verb, a.Name), nil
}
```

- [ ] **Step 4: Run the tool tests**

Run: `go test -tags sqlite_fts5 ./internal/tool/skill/...`
Expected: PASS.

- [ ] **Step 5: Write the failing policy test**

Append to `internal/policy/guard_test.go`:

```go
func TestSkillInstallIsNeverLearnable(t *testing.T) {
	// "Always allow this pattern" must never turn an install grant into a
	// standing rule: the permission is granted for one write and is gone.
	if _, ok := PatternFor(Call{Tool: "skill_install", Args: []byte(
		`{"name":"x","description":"d","body":"b","path":"/tmp/x"}`)}); ok {
		t.Fatal("skill_install must never offer a pattern scope, even with a path-shaped argument")
	}
}

func TestSkillInstallDeniedUnderRemote(t *testing.T) {
	cfg := config.PolicyConfig{
		Workspace: "/ws",
		Default:   "ask",
		Allow:     []string{"skill_load"},
		Ask:       []string{"skill_install"},
		Profiles: map[string]config.ProfilePolicy{
			"remote": {Deny: []string{"skill_install"}},
		},
	}
	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := eng.Evaluate(Session{ID: "s", Profile: ProfileRemote, Workspace: "/ws"},
		Call{Tool: "skill_install", Args: []byte(`{"name":"x"}`)})
	if res.Decision != DecisionDeny {
		t.Fatalf("a bridge session must not install a skill, got %v", res.Decision)
	}
}
```

Match `Evaluate`, `Result`, `ProfileRemote` and `ProfilePolicy` to the real names in `internal/policy/engine.go` and `internal/config/config.go` before running. Build the config explicitly, as written — not from `config.Default()`.

- [ ] **Step 6: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run Skill`
Expected: FAIL on the first test — `PatternFor` currently returns `ok` for any call with exactly one path-shaped argument.

- [ ] **Step 7: Make the non-learnable set explicit**

In `internal/policy/guard.go`, above `PatternFor`:

```go
// nonLearnable are tools whose approval must never widen into a standing
// rule, whatever their arguments look like. A skill written once shapes every
// later turn in every session, so each install is approved on its own: the
// permission is granted for one write and is gone afterwards.
//
// These would not produce a pattern today either -- their arguments are not
// path-shaped -- but resting a security property on that accident is how it
// gets lost to a later argument rename.
var nonLearnable = map[string]bool{
	"skill_install": true,
	"memory":        true,
}

func PatternFor(c Call) (string, bool) {
	if nonLearnable[c.Tool] {
		return "", false
	}
	// ... unchanged
}
```

- [ ] **Step 8: Run the policy tests**

Run: `go test -tags sqlite_fts5 ./internal/policy/...`
Expected: PASS.

- [ ] **Step 9: Add the policy defaults and wire the tools**

In `internal/config/config.go`'s `Default()`:

- append `"skill_load"` to `Allow`,
- append `"skill_install"` to `Ask`,
- extend the remote profile to `{Deny: []string{"mcp__*", "memory", "skill_install"}}`,
- extend the comment above the profile to say why: a skill written once shapes every later turn in every session, so a single injection through a bridge would otherwise plant permanent context. `skill_load` stays allowed there: a skill body is the operator's own prose and loading it has no persistent effect.

In `cmd/spore/wire.go`:

```go
	tools = append(tools, mem.NewRecallSearch(recallBackend), mem.NewMemory(facts, st))
	tools = append(tools, toolskill.New(cfg, skills)...)
```

`buildTools` takes `skills *skillfiles.Caches` as a new parameter, built in `buildAgent` next to the fact cache and also attached to the agent:

```go
	// One cache set for the process, keyed by directory: under
	// skills.scope = "workspace" each session root has its own.
	skills := skillfiles.NewCaches()
	...
	// Caches are keyed by directory and the agent asks by session root, so
	// the scope rule is applied here, in the one place that knows both.
	a.Skills = func(root string) []skillfiles.Skill {
		return skills.Skills(cfg.SkillsDir(root))
	}
```

Add the imports: `skillfiles "github.com/codered/spore/internal/skill"` and `toolskill "github.com/codered/spore/internal/tool/skill"`.

- [ ] **Step 10: Build and run everything**

Run: `make vet && make test`
Expected: PASS. `make vet` catches the `buildTools` signature change at every call site.

- [ ] **Step 11: Commit**

```bash
git add internal/tool/skill internal/policy internal/config cmd/spore/wire.go
git commit -m "feat(skill): skill_load and a non-learnable skill_install"
```

---

### Task 7: the command endpoint and the five commands

**Files:**
- Create: `internal/daemon/commands.go`
- Test: `internal/daemon/commands_test.go`
- Modify: `internal/daemon/server.go:92` (route table)
- Modify: `internal/daemon/event.go:13-21` (one new wire type)

**Interfaces:**
- Consumes: `agent.Compact`, `agent.SnapshotBreakdown` (Tasks 3-4); `store.SessionUsage`, `store.TotalUsage`, `store.LastSeq` (Task 5); `config.SkillsDir`, `skill.Caches` (Tasks 1-2, reached through `Server.agent.Skills`).
- Produces: `daemon.CommandResult{Name, Text string}`; `POST /api/sessions/{id}/commands`; `daemon.WireCommand = "command"`.

- [ ] **Step 1: Write the failing tests**

`internal/daemon/commands_test.go`, following the harness `api_test.go` uses:

```go
package daemon

import (
	"net/http"
	"strings"
	"testing"
)

func TestCommandUnknownNameListsTheValidOnes(t *testing.T) {
	srv, id := testServerWithSession(t)
	res, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"nope"}`)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", res.StatusCode)
	}
	for _, want := range []string{"clear", "compact", "context", "skills", "usage"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the error must list the valid commands, got %q", body)
		}
	}
}

func TestCommandUnknownSessionIs404(t *testing.T) {
	srv, _ := testServerWithSession(t)
	res, _ := postJSON(t, srv, "/api/sessions/does-not-exist/commands", `{"name":"usage"}`)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404, got %d", res.StatusCode)
	}
}

func TestContextCommandReportsEveryPart(t *testing.T) {
	srv, id := testServerWithSession(t)
	res, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"context"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.StatusCode, body)
	}
	for _, want := range []string{"system", "environment", "facts", "skills", "summary", "messages", "total"} {
		if !strings.Contains(strings.ToLower(body), want) {
			t.Fatalf("/context must report %q, got %s", want, body)
		}
	}
}

func TestUsageCommandReportsSessionAndTotal(t *testing.T) {
	srv, id := testServerWithSession(t)
	_, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"usage"}`)
	if !strings.Contains(strings.ToLower(body), "this session") ||
		!strings.Contains(strings.ToLower(body), "all sessions") {
		t.Fatalf("/usage must report both, got %s", body)
	}
}

func TestClearMovesTheBoundaryAndKeepsMessages(t *testing.T) {
	srv, id := testServerWithSession(t)
	appendUserMessage(t, srv, id, "hello")
	appendUserMessage(t, srv, id, "again")

	res, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"clear"}`)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.StatusCode, body)
	}
	summary, through, err := srv.store.Summary(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if summary != "" || through != 2 {
		t.Fatalf("want an empty summary through seq 2, got %q %d", summary, through)
	}
	msgs, err := srv.store.Messages(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatal("/clear must delete nothing")
	}
}

func TestClearPublishesToAttachedClients(t *testing.T) {
	srv, id := testServerWithSession(t)
	appendUserMessage(t, srv, id, "hello")
	events, cancel := srv.Subscribe(id)
	defer cancel()

	if res, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"clear"}`); res.StatusCode != 200 {
		t.Fatalf("want 200, got %d: %s", res.StatusCode, body)
	}
	select {
	case ev := <-events:
		if ev.Type != WireCommand || ev.Text == "" {
			t.Fatalf("want a command event carrying text, got %+v", ev)
		}
	default:
		t.Fatal("a state-changing command must reach every attached client")
	}
}

func TestStateChangingCommandsConflictWithARunningTurn(t *testing.T) {
	srv, id := testServerWithSession(t)
	if !srv.hub.Begin(id) {
		t.Fatal("could not mark the session running")
	}
	defer srv.hub.End(id)
	for _, name := range []string{"clear", "compact"} {
		res, _ := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"`+name+`"}`)
		if res.StatusCode != http.StatusConflict {
			t.Fatalf("/%s during a turn must be 409, got %d", name, res.StatusCode)
		}
	}
	for _, name := range []string{"context", "usage", "skills"} {
		res, _ := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"`+name+`"}`)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("/%s is a read and must answer during a turn, got %d", name, res.StatusCode)
		}
	}
}

func TestSkillsCommandListsSkills(t *testing.T) {
	srv, id := testServerWithSession(t)
	// The test server's config points DataDir at a temp dir; write a skill
	// into <DataDir>/skills and expect it listed.
	writeTestSkill(t, srv, "release-checklist", "How to cut a release")
	_, body := postJSON(t, srv, "/api/sessions/"+id+"/commands", `{"name":"skills"}`)
	if !strings.Contains(body, "release-checklist") || !strings.Contains(body, "How to cut a release") {
		t.Fatalf("/skills must list the skill, got %s", body)
	}
}
```

Write the helpers `testServerWithSession`, `postJSON`, `appendUserMessage` and `writeTestSkill` in this file only if `api_test.go` does not already provide equivalents — read it first and reuse what is there.

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run Command`
Expected: FAIL — 404 for every request, the route does not exist.

- [ ] **Step 3: Add the wire type**

In `internal/daemon/event.go`, append to the const block (these strings are append-only API):

```go
	WireCommand    = "command"
```

- [ ] **Step 4: Write `internal/daemon/commands.go`**

```go
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/skill"
	"github.com/codered/spore/internal/store"
)

// CommandResult is one command's answer. Text is rendered here rather than in
// each client, so the CLI and the web UI cannot drift: a command is daemon
// behaviour, and every surface gets the same words.
type CommandResult struct {
	Name string `json:"name"`
	Text string `json:"text"`
}

type commandFunc func(ctx context.Context, s *Server, sess store.Session, args string) (string, error)

// commands is the dispatch table. mutates marks the two that rewrite the
// summary boundary a running turn is reading; those answer 409 instead.
type command struct {
	run     commandFunc
	mutates bool
	help    string
}

var commands = map[string]command{
	"clear":   {run: cmdClear, mutates: true, help: "start fresh: move the summary boundary to now, deleting nothing"},
	"compact": {run: cmdCompact, mutates: true, help: "fold the older messages into a summary now"},
	"context": {run: cmdContext, help: "what is in the prompt right now, part by part"},
	"usage":   {run: cmdUsage, help: "tokens and cost, for this session and in total"},
	"skills":  {run: cmdSkills, help: "the skills this session can load"},
}

func commandNames() []string {
	out := make([]string, 0, len(commands))
	for name := range commands {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.findSession(w, r, id)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
		Args string `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: %v", err)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(body.Name), "/")
	c, ok := commands[name]
	if !ok {
		writeError(w, http.StatusBadRequest, "no command %q; try one of: %s",
			name, strings.Join(commandNames(), ", "))
		return
	}
	if c.mutates && s.hub.Running(id) {
		writeError(w, http.StatusConflict, "session %s already has a turn running", id)
		return
	}

	// A command is not a turn, but it reads what a turn would read: the
	// session's workspace decides its environment section and its skills
	// directory, so the context is built the same way startTurn builds it.
	ctx := policy.WithSession(s.base, policy.Session{
		ID: sess.ID, Workspace: sess.Workspace, Profile: policy.Profile(sess.Profile),
	})
	text, err := c.run(ctx, s, sess, strings.TrimSpace(body.Args))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%s: %v", name, err)
		return
	}
	if c.mutates {
		// Every attached client learns the boundary moved, not just the one
		// that asked.
		s.hub.Publish(sess.ID, WireEvent{Type: WireCommand, Tool: name, Text: text})
	}
	writeJSON(w, http.StatusOK, CommandResult{Name: name, Text: text})
}
```

Then the five commands, in the same file:

```go
func cmdClear(ctx context.Context, s *Server, sess store.Session, _ string) (string, error) {
	last, err := s.store.LastSeq(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	_, through, err := s.store.Summary(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	if last == through {
		return "nothing to clear: the prompt already starts here", nil
	}
	// An empty summary through the newest message: the next prompt is the
	// system block, the environment, the facts and the skills index, and
	// nothing else. Nothing is deleted -- every message stays in the
	// transcript, and recall still finds it.
	if err := s.store.SetSummary(ctx, sess.ID, "", last); err != nil {
		return "", err
	}
	return fmt.Sprintf("cleared: %d messages are now outside the prompt (none were deleted)", last-through), nil
}

func cmdCompact(ctx context.Context, s *Server, sess store.Session, _ string) (string, error) {
	if s.agent == nil {
		return "", fmt.Errorf("no agent attached")
	}
	folded, before, after, err := s.agent.Compact(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	if folded == 0 {
		return "nothing outside the protected recent window to fold", nil
	}
	return fmt.Sprintf("folded %d messages into the summary: %s -> %s tokens",
		folded, humanTokens(before), humanTokens(after)), nil
}

func cmdContext(ctx context.Context, s *Server, sess store.Session, _ string) (string, error) {
	if s.agent == nil {
		return "", fmt.Errorf("no agent attached")
	}
	snap, err := s.agent.Snapshot(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	cfg := s.cfg.Context
	b := agent.SnapshotBreakdown(snap, cfg)
	var out strings.Builder
	fmt.Fprintf(&out, "system        %8s\n", humanTokens(b.System))
	fmt.Fprintf(&out, "environment   %8s\n", humanTokens(b.Environment))
	fmt.Fprintf(&out, "facts         %8s  (%d)\n", humanTokens(b.Facts), len(snap.Facts))
	fmt.Fprintf(&out, "skills        %8s  (%d)\n", humanTokens(b.Skills), len(snap.Skills))
	fmt.Fprintf(&out, "summary       %8s\n", humanTokens(b.Summary))
	fmt.Fprintf(&out, "messages      %8s  (%d)\n", humanTokens(b.Messages), len(snap.Messages))
	fmt.Fprintf(&out, "total         %8s  of %s (%.0f%%)\n",
		humanTokens(b.Total()), humanTokens(cfg.MaxTokens),
		100*float64(b.Total())/float64(cfg.MaxTokens))
	fmt.Fprintf(&out, "compacts at   %8s\n", humanTokens(int(float64(cfg.MaxTokens)*cfg.CompactAt)))
	return out.String(), nil
}

func cmdUsage(ctx context.Context, s *Server, sess store.Session, _ string) (string, error) {
	session, err := s.store.SessionUsage(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	total, err := s.store.TotalUsage(ctx)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	out.WriteString("this session\n")
	writeUsage(&out, session)
	out.WriteString("\nall sessions\n")
	writeUsage(&out, total)
	return out.String(), nil
}

// writeUsage renders one set of rows plus its totals. Cost is always shown:
// show_cost governs the per-turn footer, and /usage is an explicit request
// for exactly this number.
func writeUsage(out *strings.Builder, rows []store.UsageRow) {
	if len(rows) == 0 {
		out.WriteString("  nothing yet\n")
		return
	}
	var turns, in, tokensOut int
	var cost float64
	for _, r := range rows {
		fmt.Fprintf(out, "  %-32s %5d turns  %8s in  %8s out  $%.4f\n",
			r.Model, r.Turns, humanTokens(r.TokensIn), humanTokens(r.TokensOut), r.CostUSD)
		turns += r.Turns
		in += r.TokensIn
		tokensOut += r.TokensOut
		cost += r.CostUSD
	}
	fmt.Fprintf(out, "  %-32s %5d turns  %8s in  %8s out  $%.4f\n",
		"total", turns, humanTokens(in), humanTokens(tokensOut), cost)
}

func cmdSkills(ctx context.Context, s *Server, sess store.Session, _ string) (string, error) {
	dir := s.cfg.SkillsDir(sess.Workspace)
	if dir == "" {
		return "this session has no skills directory (skills.scope is \"workspace\" and the session has no workspace of its own)", nil
	}
	skills, errs := skill.Load(dir)
	loaded, err := s.loadedSkills(ctx, sess.ID)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n", dir)
	if len(skills) == 0 {
		out.WriteString("no skills installed\n")
	}
	for _, sk := range skills {
		mark := " "
		if loaded[sk.Name] {
			mark = "*"
		}
		fmt.Fprintf(&out, "%s %-28s %6s  %s\n", mark, sk.Name,
			humanTokens(agent.EstimateTokens(sk.Body)), sk.Description)
	}
	if len(loaded) > 0 {
		out.WriteString("\n* loaded in this session\n")
	}
	for _, e := range errs {
		fmt.Fprintf(&out, "\nskipped: %v\n", e)
	}
	return out.String(), nil
}

// loadedSkills reads the transcript for skill_load calls rather than keeping
// state: a loaded body lives in the transcript like any other tool result, so
// the transcript is where the answer already is.
func (s *Server) loadedSkills(ctx context.Context, sessionID string) (map[string]bool, error) {
	rows, err := s.store.Messages(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	loaded := map[string]bool{}
	for _, r := range rows {
		var blocks []provider.Block
		if err := json.Unmarshal(r.BlocksJSON, &blocks); err != nil {
			continue // a row that will not decode is not worth failing a listing for
		}
		for _, b := range blocks {
			if b.Type != provider.BlockToolUse || b.Name != "skill_load" {
				continue
			}
			var a struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(b.Input, &a) == nil && a.Name != "" {
				loaded[a.Name] = true
			}
		}
	}
	return loaded, nil
}

// humanTokens renders an estimate compactly: 38k reads better than 38104 in a
// column, and the estimate is crude enough that the digits are noise.
func humanTokens(n int) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}
```

Add the imports this needs: `"github.com/codered/spore/internal/agent"` and `"github.com/codered/spore/internal/provider"`. Match `writeError`, `writeJSON`, `findSession` and `policy.WithSession` to the real helpers — `internal/daemon/sessions.go` has all of them; read `startTurn` at `:228` for exactly how it builds the session context and copy that.

- [ ] **Step 5: Register the route**

In `internal/daemon/server.go`, after the messages route:

```go
	mux.HandleFunc("POST /api/sessions/{id}/commands", s.handleCommand)
```

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon
git commit -m "feat(daemon): a command endpoint and the five commands"
```

---

### Task 8: the CLI clients

**Files:**
- Modify: `cmd/spore/client.go` (add `command`)
- Modify: `cmd/spore/chat.go` (`chatPlain`)
- Modify: `cmd/spore/tui.go` (the Bubble Tea model's submit path)
- Test: `cmd/spore/tui_test.go`, `cmd/spore/wire_test.go`

**Interfaces:**
- Consumes: `POST /api/sessions/{id}/commands` and `daemon.CommandResult` (Task 7).
- Produces: `func (c *client) command(ctx context.Context, sessionID, name, args string) (daemon.CommandResult, error)`; `func parseCommand(line string) (name, args string, ok bool)`.

- [ ] **Step 1: Write the failing parser tests**

Append to `cmd/spore/tui_test.go`:

```go
func TestParseCommand(t *testing.T) {
	for _, tc := range []struct {
		line, name, args string
		ok               bool
	}{
		{"/usage", "usage", "", true},
		{"/compact  ", "compact", "", true},
		{"/skills release-checklist", "skills", "release-checklist", true},
		{"  /context", "context", "", true},
		{"/", "", "", false},
		{"//", "", "", false},
		{"hello /usage", "", "", false},
		{"", "", "", false},
		{"tell me about the /usage command", "", "", false},
	} {
		name, args, ok := parseCommand(tc.line)
		if ok != tc.ok || name != tc.name || args != tc.args {
			t.Fatalf("parseCommand(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.line, name, args, ok, tc.name, tc.args, tc.ok)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run ParseCommand`
Expected: FAIL — `parseCommand` undefined.

- [ ] **Step 3: Implement the parser and the client call**

In `cmd/spore/input.go`:

```go
// parseCommand splits a leading-slash line into a command and its arguments.
// The client knows no command names: an unknown one is the daemon's 400,
// which carries the valid list, so a client can never hold a stale one.
//
// A bare "/" and a line that merely contains a slash are ordinary messages.
func parseCommand(line string) (name, args string, ok bool) {
	line = strings.TrimSpace(line)
	rest, ok := strings.CutPrefix(line, "/")
	if !ok || rest == "" {
		return "", "", false
	}
	name, args, _ = strings.Cut(rest, " ")
	if name == "" {
		return "", "", false
	}
	return name, strings.TrimSpace(args), true
}
```

In `cmd/spore/client.go`:

```go
func (c *client) command(ctx context.Context, sessionID, name, args string) (daemon.CommandResult, error) {
	var out daemon.CommandResult
	err := c.do(ctx, "POST", "/api/sessions/"+sessionID+"/commands",
		map[string]string{"name": name, "args": args}, &out)
	return out, err
}
```

- [ ] **Step 4: Wire both loops**

In `chatPlain`, inside the `case line, ok := <-lines:` branch, before `c.send`:

```go
			if name, args, ok := parseCommand(text); ok {
				res, err := c.command(ctx, sessionID, name, args)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
				} else {
					fmt.Print("\n" + strings.TrimRight(res.Text, "\n") + "\n")
				}
				continue
			}
```

In `tui.go`, where the model submits a line, take the same branch: run the command through `ui.command` (a func field set in `chatTUI` beside `ui.send`), and append the result to the transcript as its own block rather than as a user message. Follow whatever the model already does for locally-generated text; do not start a turn, and do not set the spinner.

In `chatTUI`, next to `ui.send`:

```go
	ui.command = func(name, args string) (string, error) {
		res, err := c.command(streamCtx, sessionID, name, args)
		return res.Text, err
	}
```

Handle `WireCommand` in the event switch of both loops: print `ev.Text` — this is how a `/clear` run in the web UI shows up in an attached terminal.

- [ ] **Step 5: Run the CLI tests**

Run: `go test -tags sqlite_fts5 ./cmd/spore/...`
Expected: PASS.

- [ ] **Step 6: Add an end-to-end test**

In `cmd/spore/wire_test.go` (or the daemon's `e2e_test.go`, whichever already drives a real server), add a test that starts the daemon, creates a session, posts `{"name":"usage"}` and asserts a 200 with non-empty text. This is the only test that proves the client and the daemon agree on the wire shape.

- [ ] **Step 7: Run and commit**

Run: `make test`

```bash
git add cmd/spore
git commit -m "feat(cli): slash commands in both chat loops"
```

---

### Task 9: the web UI

**Files:**
- Modify: `web/app.js`
- Modify: `web/style.css`

**Interfaces:**
- Consumes: the commands endpoint and `WireCommand` (Task 7).
- Produces: nothing other code depends on.

- [ ] **Step 1: Send commands instead of messages**

In the composer's submit handler, before the existing POST to `/messages`:

```js
  const cmd = parseCommand(text);
  if (cmd) {
    const res = await fetch(`/api/sessions/${sessionID}/commands`, {
      method: "POST",
      headers: {"Content-Type": "application/json"},
      body: JSON.stringify({name: cmd.name, args: cmd.args}),
    });
    const body = await res.json();
    appendCommandResult(res.ok ? body.text : body.error);
    return;
  }
```

with the parser matching the Go one exactly — a bare `/` and a line merely containing a slash are messages:

```js
function parseCommand(line) {
  const t = line.trim();
  if (!t.startsWith("/") || t === "/") return null;
  const rest = t.slice(1);
  const i = rest.indexOf(" ");
  if (i === -1) return {name: rest, args: ""};
  const name = rest.slice(0, i);
  if (!name) return null;
  return {name, args: rest.slice(i + 1).trim()};
}
```

- [ ] **Step 2: Render the result**

`appendCommandResult` appends a `<pre class="command">` to the transcript — the text is column-aligned server-side, so it needs a monospace block and must not be re-wrapped. Add to `style.css`:

```css
.command {
  white-space: pre;
  overflow-x: auto;
  opacity: 0.85;
  border-left: 2px solid currentColor;
  padding-left: 0.75rem;
  margin: 0.5rem 0;
}
```

- [ ] **Step 3: Handle the event**

In the SSE switch, add a `command` case that calls `appendCommandResult(ev.text)`, so a `/clear` run from the CLI appears in an open browser tab.

- [ ] **Step 4: Verify by hand**

Run: `make build && ./spore serve`, open the UI, run `/context`, `/usage` and `/skills` in a session, and confirm each renders as an aligned block. Run `/clear` in the terminal client with the browser open on the same session and confirm the browser shows it.

- [ ] **Step 5: Commit**

```bash
git add web
git commit -m "feat(web): run chat commands from the browser"
```

---

### Task 10: documentation

**Files:**
- Modify: `docs/superpowers/specs/2026-08-29-spore-design.md` (sections 3, 5, 6, 8, 9, 11)
- Modify: `README.md`
- Modify: `docs/backlog.md`

- [ ] **Step 1: Amend the design spec**

Make exactly the amendments the new spec's section 1 header names:

- §3 context assembly: the fixed order gains "Skills — the index of loadable skills, names and descriptions only" between facts and the summary, with a sentence on `skill_budget` and on bodies arriving as `skill_load` results.
- §5: a "Skills — files, human-editable" subsection after Facts, carrying the layout, the `internal/skill` interface, and why the directory sits outside the workspace.
- §6 builtins: `skill_load` and `skill_install` in the tool list; in the policy subsection, `skill_load` allowed by default, `skill_install` ask-gated, non-learnable and denied under `remote`, with the reason.
- §8 daemon: the commands endpoint joins the enumerated API surface.
- §9: the `[skills]` block and `context.skill_budget`.
- §11: a stage 7 entry naming this plan.
- Update the header's amendment line: `amended 2026-09-04: chat commands and the skills subsystem, sections 3, 5, 6, 8, 9 and 11`.

- [ ] **Step 2: Update the README**

Change the status line to stage 7, and add a short section covering the five commands and skills: where skills live, the frontmatter, that the directory is read-only to the filesystem tools, that installing one needs a human approval each time, and the `[skills]` config block.

- [ ] **Step 3: Update the backlog**

Rewrite the "Chat commands" entry as shipped: all five landed, `/skills` as the SKILL.md subsystem, the daemon-API answer to open question 1, and the `/clear` answer to open question 3 (the boundary moves, so a bound thread survives). Note the one gap this plan knowingly leaves: `skill.Caches` never evicts, which is the same shape as the describer cache entry above it — fold it into that entry rather than writing a second one.

- [ ] **Step 4: Commit**

```bash
git add docs README.md
git commit -m "docs: amend the design spec for commands and skills, and catch up the backlog"
```

---

## Final verification

- [ ] `make vet` — clean.
- [ ] `make test` — every package passes.
- [ ] `make build && ./spore chat` in a scratch directory: run all five commands; install a skill and confirm the approval prompt offers no "always" option; confirm `/skills` lists it and a new session's `/context` counts it.
