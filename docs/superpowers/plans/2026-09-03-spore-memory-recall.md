# Plan 5a — Memory and recall (local) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give spore a memory it can read, write and search entirely offline — human-editable fact files budgeted into every turn, and FTS5 keyword recall over messages, summaries and facts behind a `Recall` interface a Weaviate backend can later implement.

**Architecture:** `internal/memory` owns fact files and nothing else — parse, validate, atomic write, delete, and an in-process `Cache` the daemon reloads after a write. `internal/recall` declares the `Recall` interface and the query tokeniser; `internal/recall/sqlitefts` implements it against one FTS5 virtual table, written from Go inside the same transaction that appends a message. Two builtins in `internal/tool/mem` expose the layers to the model: `recall_search` (read-only, scoped to the calling session under the `remote` profile) and `memory` (`ask` locally, denied remotely).

**Tech Stack:** Go 1.26, `github.com/mattn/go-sqlite3` v1.14.50 with FTS5, OpenTelemetry (existing `internal/trace`). No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md`, section 5 (with sections 2, 3, 6, 9, 10 amended to match)

## Global Constraints

- Go 1.26; module `github.com/codered/spore`. **No new entries in `go.mod`.** In particular do not add a YAML library: fact frontmatter is three fixed keys and is parsed by hand.
- **Every build, test and vet runs with `-tags sqlite_fts5`.** Use `make build`, `make test`, `make vet`. A bare `go test ./...` fails to compile the FTS code and proves nothing.
- Any test that asserts a policy outcome MUST build its config with `config.Load` on a real TOML file. `config.Default()` does not carry the baseline deny rules, so a policy assertion against it proves nothing.
- Run `make fmtcheck` before every commit.
- Nothing in this plan may fail a turn. A malformed fact, a missing directory, a failed search: each degrades to a warning or a tool error the model can read. The one deliberate exception is the index write inside `AppendMessage`, which shares that transaction.
- Facts live under `<data_dir>/memory`, which is **outside** `policy.Workspace`. Never route fact access through `policy.Resolve` or the `fs` tools — `internal/memory` does its own confinement.
- Comment density and naming follow the surrounding code: comments explain *why*, never *what*.

## Verified behaviour (probed, not recalled)

These were confirmed by running SQLite 3 through `github.com/mattn/go-sqlite3` v1.14.50 with `-tags sqlite_fts5` before this plan was written. Do not re-litigate them:

1. `CREATE VIRTUAL TABLE ... USING fts5(text, kind UNINDEXED, ...)` compiles, and `WHERE recall_fts MATCH ? AND session_id = ? AND kind IN (...)` filters on UNINDEXED columns alongside a match.
2. `bm25()` returns **negative** scores where more-negative is a better match, so best-first is `ORDER BY score ASC`. Sorting descending silently returns the worst hits.
3. Unquoted user text is frequently a **syntax error**, not an empty result: `what's the -v flag?` → `fts5: syntax error near "'"`, `AND` → syntax error, `""` (empty) → syntax error. Quoting each token fixes all of them.
4. An input that tokenises to **nothing** (empty string, all punctuation) must be short-circuited by the caller: passing an empty MATCH string is a syntax error.
5. `DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?` works on an ordinary (non-external-content) FTS5 table, and an `AFTER DELETE` trigger on `messages` may run it.
6. `INSERT INTO recall_fts(recall_fts) VALUES('rebuild')` is a **no-op** here, because the table stores its own content. Reindexing means `DELETE` then re-`INSERT` from the source tables — never `'rebuild'`.

---

### Task 1: `internal/memory` — fact files

**Files:**
- Create: `internal/memory/memory.go`
- Create: `internal/memory/cache.go`
- Test: `internal/memory/memory_test.go`

**Interfaces:**
- Consumes: nothing (leaf package; imports only stdlib).
- Produces: `memory.Fact{Name, Description, Type, Body, Path string}`; `memory.Load(dir string) ([]Fact, []error)`; `memory.Write(dir string, f Fact) error`; `memory.Delete(dir, name string) error`; `memory.ValidName(name string) error`; `memory.Cache` with `NewCache(dir string) *Cache`, `(*Cache).Reload() []error`, `(*Cache).Facts() []Fact`, `(*Cache).Dir() string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/memory/memory_test.go`:

```go
package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const good = `---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Gofmt defaults, tabs, no line-length limit.
`

func TestLoadParsesAFact(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prefers-tabs.md", good)

	facts, errs := Load(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	f := facts[0]
	if f.Name != "prefers-tabs" || f.Type != "user" {
		t.Fatalf("bad frontmatter: %+v", f)
	}
	if f.Description != "How the user wants Go code formatted" {
		t.Fatalf("bad description: %q", f.Description)
	}
	if f.Body != "Gofmt defaults, tabs, no line-length limit." {
		t.Fatalf("body not trimmed: %q", f.Body)
	}
	if f.Path != filepath.Join(dir, "prefers-tabs.md") {
		t.Fatalf("bad path: %q", f.Path)
	}
}

// A human edits these files by hand, so one broken file must cost exactly one
// fact -- never the whole set, and never a returned error that could fail a turn.
func TestLoadDegradesPerFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prefers-tabs.md", good)
	write(t, dir, "broken.md", "no frontmatter at all\n")
	write(t, dir, "notes.txt", "ignored, not markdown")

	facts, errs := Load(dir)
	if len(facts) != 1 || facts[0].Name != "prefers-tabs" {
		t.Fatalf("good fact not returned alongside the broken one: %+v", facts)
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "broken.md") {
		t.Fatalf("error does not name the file: %v", errs[0])
	}
}

func TestLoadMissingDirIsNotAnError(t *testing.T) {
	facts, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if len(facts) != 0 || len(errs) != 0 {
		t.Fatalf("missing dir should be empty and quiet, got %v %v", facts, errs)
	}
}

func TestLoadSortsByName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"zebra", "alpha", "middle"} {
		write(t, dir, n+".md", "---\nname: "+n+"\ndescription: d\ntype: user\n---\n\nbody\n")
	}
	facts, _ := Load(dir)
	if len(facts) != 3 || facts[0].Name != "alpha" || facts[2].Name != "zebra" {
		t.Fatalf("not sorted by name: %+v", facts)
	}
}

func TestLoadRejectsNameFilenameMismatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "actual.md", "---\nname: claimed\ndescription: d\ntype: user\n---\n\nbody\n")
	facts, errs := Load(dir)
	if len(facts) != 0 || len(errs) != 1 {
		t.Fatalf("mismatch must be rejected: %v %v", facts, errs)
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x.md", "---\nname: x\ndescription: d\ntype: wibble\n---\n\nbody\n")
	if facts, errs := Load(dir); len(facts) != 0 || len(errs) != 1 {
		t.Fatalf("unknown type must be rejected: %v %v", facts, errs)
	}
}

func TestLoadIgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, sub, "hidden.md", good)
	facts, errs := Load(dir)
	if len(facts) != 0 || len(errs) != 0 {
		t.Fatalf("subdirectory must not become context: %v %v", facts, errs)
	}
}

func TestWriteRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	f := Fact{Name: "likes-go", Description: "a taste", Type: "user", Body: "Go, mostly."}
	if err := Write(dir, f); err != nil {
		t.Fatal(err)
	}
	facts, errs := Load(dir)
	if len(errs) != 0 || len(facts) != 1 || facts[0].Body != "Go, mostly." {
		t.Fatalf("round trip failed: %v %v", facts, errs)
	}
}

// A name is attacker-influenced: the model chooses it. It must never be able
// to place or remove a file outside the memory directory.
func TestNamesCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape", "a/b", "", ".", "..", "Caps", "with space", "under_score", "-lead", "trail-"} {
		if err := Write(dir, Fact{Name: bad, Description: "d", Type: "user", Body: "b"}); err == nil {
			t.Fatalf("Write accepted name %q", bad)
		}
		if err := Delete(dir, bad); err == nil {
			t.Fatalf("Delete accepted name %q", bad)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("rejected writes left files behind: %v", entries)
	}
}

func TestWriteRequiresDescriptionAndType(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Fact{Name: "ok", Type: "user"}); err == nil {
		t.Fatal("empty description accepted")
	}
	if err := Write(dir, Fact{Name: "ok", Description: "d", Type: "nope"}); err == nil {
		t.Fatal("bad type accepted")
	}
	if err := Write(dir, Fact{Name: "ok", Description: "one\ntwo", Type: "user"}); err == nil {
		t.Fatal("multi-line description accepted")
	}
}

func TestDeleteRemovesAndReportsMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "gone.md", "---\nname: gone\ndescription: d\ntype: user\n---\n\nb\n")
	if err := Delete(dir, "gone"); err != nil {
		t.Fatal(err)
	}
	if facts, _ := Load(dir); len(facts) != 0 {
		t.Fatal("fact still present")
	}
	if err := Delete(dir, "gone"); err == nil {
		t.Fatal("deleting a missing fact should report it")
	}
}

func TestCacheReloadsAfterWrite(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(c.Facts()) != 0 {
		t.Fatal("cache should start empty")
	}
	if err := Write(dir, Fact{Name: "new", Description: "d", Type: "user", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(c.Facts()) != 0 {
		t.Fatal("cache must not see a write until it reloads")
	}
	c.Reload()
	if len(c.Facts()) != 1 {
		t.Fatal("reload did not pick up the new fact")
	}
}

func TestCacheFactsIsACopy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nname: a\ndescription: d\ntype: user\n---\n\nb\n")
	c := NewCache(dir)
	c.Reload()
	got := c.Facts()
	got[0].Body = "mutated"
	if c.Facts()[0].Body != "b" {
		t.Fatal("caller mutated the cache's backing array")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/memory/ 2>&1 | head -20`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write `internal/memory/memory.go`**

```go
// Package memory owns spore's fact files: the human-editable markdown under
// <data_dir>/memory that is loaded into every turn. The file is the source of
// truth -- nothing else stores a fact -- so this package is filesystem-only:
// it holds no database handle and knows nothing about sessions.
//
// The directory sits outside policy.Workspace, so the filesystem tools cannot
// reach it and this package does its own confinement instead.
package memory

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

// Fact is one file. A fact carries no token count: sizing belongs to the
// estimator in internal/agent, and a filesystem package must not depend on
// the agent to describe a file.
type Fact struct {
	Name        string
	Description string
	Type        string
	Body        string
	Path        string
}

// Types is the closed set a fact may declare. A closed set keeps the system
// block's headings predictable and catches typos at write time.
var Types = []string{"user", "feedback", "project", "reference"}

// nameRE is deliberately narrower than "a legal filename": lowercase kebab
// only. The model chooses this string, and it becomes a path, so anything
// that could traverse, collide case-insensitively, or need quoting is out.
var nameRE = regexp.MustCompile(`\A[a-z0-9]+(-[a-z0-9]+)*\z`)

// ValidName reports whether a name may be turned into a path.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("fact name %q must be lowercase kebab-case (letters, digits and single hyphens)", name)
	}
	return nil
}

func validType(t string) error {
	for _, k := range Types {
		if t == k {
			return nil
		}
	}
	return fmt.Errorf("fact type %q must be one of %s", t, strings.Join(Types, ", "))
}

// Validate checks a fact is safe to write and complete enough to be useful.
func (f Fact) Validate() error {
	if err := ValidName(f.Name); err != nil {
		return err
	}
	if err := validType(f.Type); err != nil {
		return err
	}
	if strings.TrimSpace(f.Description) == "" {
		return errors.New("fact description is required: it is what the model sees when the body does not fit the budget")
	}
	if strings.ContainsAny(f.Description, "\r\n") {
		return errors.New("fact description must be a single line")
	}
	if strings.TrimSpace(f.Body) == "" {
		return errors.New("fact body is required")
	}
	return nil
}

// Path is the only place a name becomes a filename.
func Path(dir, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".md"), nil
}

// Load reads every fact in dir, sorted by name. Errors are per-file and are
// returned alongside the facts that did parse: a human edits these by hand,
// so one broken file must cost exactly one fact and never a whole turn. A
// missing directory is zero facts and no error.
func Load(dir string) ([]Fact, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("read memory dir %s: %w", dir, err)}
	}
	var facts []Fact
	var errs []error
	for _, e := range entries {
		// No recursion: a scratch subdirectory must never become context.
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		f, err := parse(string(data))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		// The filename is the identity used by Delete and by the recall index,
		// so frontmatter that disagrees with it is a defect, not a preference.
		if want := strings.TrimSuffix(e.Name(), ".md"); f.Name != want {
			errs = append(errs, fmt.Errorf("%s: frontmatter name %q does not match the filename", e.Name(), f.Name))
			continue
		}
		f.Path = path
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Name < facts[j].Name })
	return facts, errs
}

// parse reads the fixed three-key frontmatter. This is not YAML and does not
// pretend to be: three known keys do not justify a dependency, and a hand
// parser gives error messages that name the actual problem.
func parse(text string) (Fact, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Fact{}, errors.New("missing opening --- frontmatter delimiter")
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Fact{}, errors.New("missing closing --- frontmatter delimiter")
	}
	var f Fact
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Fact{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			f.Name = value
		case "description":
			f.Description = value
		case "type":
			f.Type = value
		default:
			return Fact{}, fmt.Errorf("unknown frontmatter key %q", strings.TrimSpace(key))
		}
	}
	f.Body = strings.TrimSpace(body)
	if err := f.Validate(); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// Render is the on-disk form. Write and the tests both go through it so the
// format has exactly one definition.
func Render(f Fact) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(f.Name)
	b.WriteString("\ndescription: ")
	b.WriteString(f.Description)
	b.WriteString("\ntype: ")
	b.WriteString(f.Type)
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n")
	return b.String()
}

// Write validates, then replaces the file atomically so a reader never sees a
// half-written fact.
func Write(dir string, f Fact) error {
	if err := f.Validate(); err != nil {
		return err
	}
	path, err := Path(dir, f.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".fact-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(Render(f)); err != nil {
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

// Delete removes one fact. A name that does not exist is an error the model
// can read and correct, not a silent success.
func Delete(dir, name string) error {
	path, err := Path(dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no fact named %q", name)
		}
		return err
	}
	return nil
}
```

- [ ] **Step 4: Write `internal/memory/cache.go`**

```go
package memory

import "sync"

// Cache holds the loaded facts for the process. Assembly runs on every turn
// and a directory scan per turn buys nothing: the daemon reloads after a
// write instead, which is the only way the set changes while spore runs.
type Cache struct {
	dir   string
	mu    sync.RWMutex
	facts []Fact
}

func NewCache(dir string) *Cache { return &Cache{dir: dir} }

func (c *Cache) Dir() string { return c.dir }

// Reload rereads the directory and returns the per-file errors so the caller
// can warn about them. The cache keeps whatever parsed.
func (c *Cache) Reload() []error {
	facts, errs := Load(c.dir)
	c.mu.Lock()
	c.facts = facts
	c.mu.Unlock()
	return errs
}

// Facts returns a copy: a caller that trims for a token budget must not be
// able to edit the shared set.
func (c *Cache) Facts() []Fact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Fact, len(c.facts))
	copy(out, c.facts)
	return out
}
```

- [ ] **Step 5: Run the tests**

Run: `make test 2>&1 | tail -25` and `go test -tags sqlite_fts5 -run Test ./internal/memory/ -v 2>&1 | tail -40`
Expected: every test in `internal/memory` PASSes, and no other package breaks.

- [ ] **Step 6: Verify the escape test is load-bearing (mutation check)**

Temporarily change `nameRE` to `regexp.MustCompile(`.+`)`. Run `go test -tags sqlite_fts5 ./internal/memory/`. Expected: `TestNamesCannotEscapeTheDirectory` FAILS. **Restore the regexp** and confirm the suite passes again. If the mutation does not fail the test, the test is decorative — fix it before committing.

- [ ] **Step 7: Commit**

```bash
make fmtcheck && make test
git add internal/memory
git commit -m "feat(memory): fact files with validation, atomic writes and a reload cache"
```

---

### Task 2: Context budgeting

**Files:**
- Modify: `internal/agent/context.go`
- Modify: `internal/config/config.go` (`ContextConfig`, `Default`, `Validate`)
- Test: `internal/agent/context_test.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `memory.Fact` (Task 1).
- Produces: `agent.Snapshot.Facts []memory.Fact`; `config.ContextConfig.FactBudget int` (`toml:"fact_budget"`, default 2000).

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/context_test.go`:

```go
func fact(name, desc, body string) memory.Fact {
	return memory.Fact{Name: name, Description: desc, Type: "user", Body: body}
}

func TestAssembleInlinesFactsUnderBudget(t *testing.T) {
	snap := Snapshot{
		System: "sys",
		Facts:  []memory.Fact{fact("alpha", "first", "Alpha body."), fact("beta", "second", "Beta body.")},
	}
	req := Assemble(snap, config.ContextConfig{FactBudget: 1000})
	for _, want := range []string{"### alpha", "Alpha body.", "### beta", "Beta body."} {
		if !strings.Contains(req.System, want) {
			t.Fatalf("system block missing %q:\n%s", want, req.System)
		}
	}
	if strings.Contains(req.System, "recall_search") {
		t.Fatalf("no overflow expected, but the overflow heading is present:\n%s", req.System)
	}
}

func TestAssembleOverflowsToDescriptions(t *testing.T) {
	big := strings.Repeat("x ", 2000) // ~1000 tokens
	snap := Snapshot{
		System: "sys",
		Facts:  []memory.Fact{fact("aaa", "small one", "tiny"), fact("zzz", "the big one", big)},
	}
	req := Assemble(snap, config.ContextConfig{FactBudget: 100})
	if !strings.Contains(req.System, "tiny") {
		t.Fatalf("the fact that fits was not inlined:\n%s", req.System)
	}
	if strings.Contains(req.System, big) {
		t.Fatal("the oversized fact body was inlined despite the budget")
	}
	if !strings.Contains(req.System, "- zzz: the big one") {
		t.Fatalf("overflow fact missing its description line:\n%s", req.System)
	}
	if !strings.Contains(req.System, "recall_search") {
		t.Fatalf("overflow section must tell the model how to retrieve a body:\n%s", req.System)
	}
}

// A fact too large to inline must not evict the smaller facts that follow it.
func TestAssembleKeepsInliningAfterAnOverflow(t *testing.T) {
	big := strings.Repeat("x ", 2000)
	snap := Snapshot{Facts: []memory.Fact{
		fact("aaa", "d", "first small"),
		fact("mmm", "d", big),
		fact("zzz", "d", "last small"),
	}}
	req := Assemble(snap, config.ContextConfig{FactBudget: 100})
	if !strings.Contains(req.System, "last small") {
		t.Fatalf("a later small fact was dropped by an earlier oversized one:\n%s", req.System)
	}
}

func TestAssembleZeroBudgetSendsEverythingToOverflow(t *testing.T) {
	snap := Snapshot{Facts: []memory.Fact{fact("aaa", "described", "body text")}}
	req := Assemble(snap, config.ContextConfig{FactBudget: 0})
	if strings.Contains(req.System, "body text") {
		t.Fatal("a zero budget inlined a body")
	}
	if !strings.Contains(req.System, "- aaa: described") {
		t.Fatal("a zero budget dropped the fact entirely instead of listing it")
	}
}

// The system block is the prompt-cache prefix. Two assemblies of the same
// snapshot must be byte-identical, which is why facts are ordered by name and
// never by recency.
func TestAssembleIsByteStableAcrossCalls(t *testing.T) {
	snap := Snapshot{System: "sys", Facts: []memory.Fact{
		fact("beta", "b", "B"), fact("alpha", "a", "A"), fact("gamma", "g", "G"),
	}}
	cfg := config.ContextConfig{FactBudget: 1000}
	if a, b := Assemble(snap, cfg).System, Assemble(snap, cfg).System; a != b {
		t.Fatalf("system block not stable:\n%q\n%q", a, b)
	}
}

func TestAssembleNoFactsNoSection(t *testing.T) {
	req := Assemble(Snapshot{System: "sys"}, config.ContextConfig{FactBudget: 1000})
	if req.System != "sys" {
		t.Fatalf("empty fact set added a section: %q", req.System)
	}
}
```

Append to `internal/config/config_test.go`:

```go
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
```

Note: `internal/agent/context_test.go` will need `"strings"`, `"github.com/codered/spore/internal/config"` and `"github.com/codered/spore/internal/memory"` in its imports. In `config_test.go`, match the surrounding tests' construction of a valid config — read the neighbouring tests before writing `TestNegativeFactBudgetIsRejected` and copy how they satisfy `Validate`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent/ ./internal/config/ 2>&1 | head -20`
Expected: compile failure — `Snapshot.Facts` is `[]string` and `FactBudget` does not exist.

- [ ] **Step 3: Add the config field**

In `internal/config/config.go`, extend `ContextConfig`:

```go
type ContextConfig struct {
	MaxTokens  int     `toml:"max_tokens"`
	CompactAt  float64 `toml:"compact_at"`
	KeepRecent int     `toml:"keep_recent"`
	// FactBudget caps the estimated tokens of inlined fact bodies. Facts past
	// the budget still appear, as one name-and-description line each, so the
	// model always knows they exist.
	FactBudget int `toml:"fact_budget"`
}
```

In `Default()`, change the Context line to:

```go
		Context:   ContextConfig{MaxTokens: 180_000, CompactAt: 0.75, KeepRecent: 12, FactBudget: 2000},
```

In `Validate`, alongside the existing context checks, add:

```go
	if c.Context.FactBudget < 0 {
		return fmt.Errorf("context.fact_budget must not be negative")
	}
```

- [ ] **Step 4: Rewrite the fact section of `Assemble`**

In `internal/agent/context.go`, change the import block to add `"github.com/codered/spore/internal/memory"`, change the field, and replace the facts branch:

```go
type Snapshot struct {
	System   string
	Facts    []memory.Fact
	Summary  string
	Messages []provider.Message
}
```

`SnapshotTokens` becomes:

```go
func SnapshotTokens(snap Snapshot) int {
	n := EstimateTokens(snap.System) + EstimateTokens(snap.Summary)
	for _, f := range snap.Facts {
		n += factTokens(f)
	}
	for _, m := range snap.Messages {
		n += messageTokens(m)
	}
	return n
}

// factTokens sizes a fact as it will actually be rendered, heading included,
// so the budget measures what the request will cost rather than the file.
func factTokens(f memory.Fact) int {
	return EstimateTokens(f.Name) + EstimateTokens(f.Description) + EstimateTokens(f.Body) + 4
}
```

And in `Assemble`, replace the whole `if len(snap.Facts) > 0 { ... }` block with:

```go
	if len(snap.Facts) > 0 {
		sys.WriteString("\n\n## What you know about the user\n")
		var overflow []memory.Fact
		used := 0
		for _, f := range snap.Facts {
			// An oversized fact overflows on its own account and does not
			// evict the smaller facts after it, so one long file cannot empty
			// the section.
			cost := factTokens(f)
			if used+cost > cfg.FactBudget {
				overflow = append(overflow, f)
				continue
			}
			used += cost
			sys.WriteString("\n### ")
			sys.WriteString(f.Name)
			sys.WriteString("\n")
			sys.WriteString(f.Body)
			sys.WriteString("\n")
		}
		if len(overflow) > 0 {
			sys.WriteString("\nThese facts did not fit. Retrieve one by name with recall_search:\n")
			for _, f := range overflow {
				sys.WriteString("- ")
				sys.WriteString(f.Name)
				sys.WriteString(": ")
				sys.WriteString(f.Description)
				sys.WriteString("\n")
			}
		}
	}
```

- [ ] **Step 5: Fix the one existing caller**

`internal/agent/agent.go` builds `Snapshot{System: ..., Summary: ...}` and never sets `Facts`, so it still compiles. Update its stale comment:

```go
// Snapshot reads the session's persisted state into the value context
// assembly consumes. Facts come from the fact cache when one is attached;
// a nil cache means no facts, which is what `spore once` runs with.
```

Then run `grep -rn "Facts" --include='*.go' .` and fix any other construction site the compiler flags.

- [ ] **Step 6: Run the tests**

Run: `make test 2>&1 | tail -25`
Expected: all packages ok.

- [ ] **Step 7: Verify the stability test is load-bearing**

Temporarily add `sort.Slice(snap.Facts, func(i, j int) bool { return snap.Facts[i].Body > snap.Facts[j].Body })` at the top of the facts branch — a plausible "most detailed first" change. Confirm nothing fails, then instead make the order genuinely unstable by iterating a map, or simply reverse the slice on every other call using a package-level counter. Expected: `TestAssembleIsByteStableAcrossCalls` FAILS. **Revert the mutation.**

- [ ] **Step 8: Commit**

```bash
make fmtcheck && make test
git add internal/agent internal/config
git commit -m "feat(agent): budget fact bodies into the system block, overflow to descriptions"
```

---

### Task 3: The FTS5 index in the store

**Files:**
- Modify: `internal/store/schema.go`
- Modify: `internal/store/store.go` (`Open`, `AppendMessage`, `SetSummary`, new `DB`, fact and reindex helpers)
- Create: `internal/store/recall.go`
- Test: `internal/store/recall_test.go`

**Interfaces:**
- Consumes: `provider.Block`, `provider.BlockText` (existing).
- Produces: `(*store.Store).DB() *sql.DB`; `(*store.Store).IndexFact(ctx context.Context, name, text string) error`; `(*store.Store).UnindexFact(ctx context.Context, name string) error`; `(*store.Store).ReindexAll(ctx context.Context) (int, error)`. Table `recall_fts(text, kind UNINDEXED, ref_id UNINDEXED, session_id UNINDEXED, created_at UNINDEXED)` with kinds `message`, `summary`, `fact`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/recall_test.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func blocks(t *testing.T, bs ...provider.Block) []byte {
	t.Helper()
	b, err := json.Marshal(bs)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func countFTS(t *testing.T, st *Store, where string, args ...any) int {
	t.Helper()
	var n int
	q := `SELECT count(*) FROM recall_fts`
	if where != "" {
		q += " WHERE " + where
	}
	if err := st.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAppendMessageIndexesText(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "exponential backoff with jitter"}),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'message' AND recall_fts MATCH '"backoff"'`); n != 1 {
		t.Fatalf("message text not indexed: n=%d", n)
	}
}

// The trust boundary: tool results carry third-party text -- a fetched web
// page, an MCP server's reply -- and must never become searchable.
func TestToolResultBlocksAreNeverIndexed(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "tool",
		BlocksJSON: blocks(t,
			provider.Block{Type: provider.BlockToolResult, ID: "1", Content: "poisonedmarker from a web page"},
			provider.Block{Type: provider.BlockToolUse, ID: "1", Name: "web_fetch", Input: json.RawMessage(`{"url":"poisonedmarker"}`)},
		),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"poisonedmarker"'`); n != 0 {
		t.Fatalf("tool result reached the index: n=%d", n)
	}
	if n := countFTS(t, st, ""); n != 0 {
		t.Fatalf("a message with no text blocks still wrote a row: n=%d", n)
	}
}

// A mixed message indexes its prose and drops the rest, rather than being
// skipped wholesale.
func TestMixedMessageIndexesOnlyItsText(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "assistant",
		BlocksJSON: blocks(t,
			provider.Block{Type: provider.BlockText, Text: "checking the keepthis file"},
			provider.Block{Type: provider.BlockToolResult, Content: "dropthis"},
		),
	}); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"keepthis"'`); n != 1 {
		t.Fatalf("prose not indexed: n=%d", n)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"dropthis"'`); n != 0 {
		t.Fatalf("tool result indexed alongside prose: n=%d", n)
	}
}

func TestSummaryIndexReplacesRatherThanAccumulates(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if err := st.SetSummary(ctx, sid, "first summary", 1); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "second summary", 2); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'summary'`); n != 1 {
		t.Fatalf("summary rows accumulated: n=%d", n)
	}
	if n := countFTS(t, st, `recall_fts MATCH '"first"'`); n != 0 {
		t.Fatal("the superseded summary is still searchable")
	}
}

func TestDeletingASessionClearsItsIndexRows(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "vanishing text"})}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "vanishing summary", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM sessions WHERE id = ?`, sid); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, ""); n != 0 {
		t.Fatalf("index rows outlived their session: n=%d", n)
	}
}

func TestFactIndexWriteReplaceAndDelete(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	if err := st.IndexFact(ctx, "prefers-tabs", "tabs not spaces"); err != nil {
		t.Fatal(err)
	}
	if err := st.IndexFact(ctx, "prefers-tabs", "tabs always"); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'fact'`); n != 1 {
		t.Fatalf("reindexing a fact accumulated rows: n=%d", n)
	}
	if err := st.UnindexFact(ctx, "prefers-tabs"); err != nil {
		t.Fatal(err)
	}
	if n := countFTS(t, st, `kind = 'fact'`); n != 0 {
		t.Fatalf("fact row survived deletion: n=%d", n)
	}
}

func TestReindexRebuildsAfterCorruption(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "recoverable text"})}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSummary(ctx, sid, "recoverable summary", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	n, err := st.ReindexAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("reindexed %d rows, want 2", n)
	}
	if got := countFTS(t, st, `recall_fts MATCH '"recoverable"'`); got != 2 {
		t.Fatalf("reindex did not restore both rows: n=%d", got)
	}
}

// An existing database predates the index, so opening it must backfill.
func TestOpenBackfillsAnExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spore.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := st.CreateSession(ctx, "t")
	if _, err := st.AppendMessage(ctx, Message{SessionID: sid, Role: "user",
		BlocksJSON: blocks(t, provider.Block{Type: provider.BlockText, Text: "historic text"})}); err != nil {
		t.Fatal(err)
	}
	// Simulate a database written before recall existed.
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if n := countFTS(t, st2, `recall_fts MATCH '"historic"'`); n != 1 {
		t.Fatalf("open did not backfill: n=%d", n)
	}
}
```

Every call to `countFTS` passes the store explicitly, because the last test opens a second one.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ 2>&1 | head -20`
Expected: compile failure — `DB`, `IndexFact`, `UnindexFact`, `ReindexAll` are undefined.

- [ ] **Step 3: Extend the schema**

Append to `schemaSQL` in `internal/store/schema.go`, before the closing backtick:

```sql
-- recall_fts is the keyword index behind spore recall and the recall_search
-- tool. Content lives in this table, so 'rebuild' is a no-op here: repairing
-- the index means deleting and reinserting from the source tables.
--
-- ref_id is TEXT for every kind: a message id cast to text, a session id for a
-- summary, a fact name for a fact.
CREATE VIRTUAL TABLE IF NOT EXISTS recall_fts USING fts5(
  text,
  kind       UNINDEXED,
  ref_id     UNINDEXED,
  session_id UNINDEXED,
  created_at UNINDEXED
);

-- Deletion is the one sync path a trigger can own, because it needs no
-- knowledge of the block format. Insertion happens in Go, where the block
-- types are real types rather than JSON to be re-parsed in SQL.
CREATE TRIGGER IF NOT EXISTS recall_fts_messages_ad AFTER DELETE ON messages BEGIN
  DELETE FROM recall_fts WHERE kind = 'message' AND ref_id = CAST(old.id AS TEXT);
END;

CREATE TRIGGER IF NOT EXISTS recall_fts_summaries_ad AFTER DELETE ON summaries BEGIN
  DELETE FROM recall_fts WHERE kind = 'summary' AND ref_id = old.session_id;
END;
```

- [ ] **Step 4: Write `internal/store/recall.go`**

```go
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/provider"
)

// Recall index kinds. They are the same strings internal/recall exports;
// the store cannot import that package without a cycle, so the values are
// repeated here and pinned by a test in internal/recall.
const (
	kindMessage = "message"
	kindSummary = "summary"
	kindFact    = "fact"
)

// DB exposes the handle so internal/recall/sqlitefts can query the index
// without the store growing search methods of its own. The store owns writes
// to recall_fts; the backend only reads.
func (s *Store) DB() *sql.DB { return s.db }

// indexableText extracts the part of a message that may be searched. Text
// blocks only: tool_result blocks carry third-party content -- a fetched page,
// an MCP server's reply -- and indexing them would make an injected string
// permanently retrievable. tool_use blocks are arguments, not prose.
func indexableText(blocksJSON []byte) string {
	var blocks []provider.Block
	if err := json.Unmarshal(blocksJSON, &blocks); err != nil {
		// A message whose blocks will not parse is already broken for every
		// other reader; it simply contributes nothing to the index.
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == provider.BlockText && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertIndex(ctx context.Context, e execer, kind, refID, sessionID, createdAt, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	_, err := e.ExecContext(ctx,
		`INSERT INTO recall_fts (text, kind, ref_id, session_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		text, kind, refID, sessionID, createdAt)
	if err != nil {
		return fmt.Errorf("index %s %s: %w", kind, refID, err)
	}
	return nil
}

func deleteIndex(ctx context.Context, e execer, kind, refID string) error {
	_, err := e.ExecContext(ctx, `DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?`, kind, refID)
	if err != nil {
		return fmt.Errorf("unindex %s %s: %w", kind, refID, err)
	}
	return nil
}

// IndexFact makes one fact file searchable, replacing whatever was indexed
// under that name. Facts are file-owned, so this is called by whoever loads or
// writes them rather than by a trigger.
func (s *Store) IndexFact(ctx context.Context, name, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := deleteIndex(ctx, tx, kindFact, name); err != nil {
		return err
	}
	if err := insertIndex(ctx, tx, kindFact, name, "", nowString(), text); err != nil {
		return err
	}
	return tx.Commit()
}

// UnindexFact drops a deleted fact from the index.
func (s *Store) UnindexFact(ctx context.Context, name string) error {
	return deleteIndex(ctx, s.db, kindFact, name)
}

// ReindexAll rebuilds the message and summary rows from the source tables and
// reports how many it wrote. Fact rows are left alone: they belong to files
// this package cannot read, and their owner reindexes them. This is the repair
// path for an index that has drifted, and the backfill for a database written
// before recall existed.
func (s *Store) ReindexAll(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM recall_fts WHERE kind IN (?, ?)`, kindMessage, kindSummary); err != nil {
		return 0, fmt.Errorf("clear index: %w", err)
	}

	n := 0
	rows, err := tx.QueryContext(ctx, `SELECT id, session_id, blocks, created_at FROM messages ORDER BY id`)
	if err != nil {
		return 0, err
	}
	// text holds a message's blocks JSON or a summary's text, depending on
	// which query filled it.
	type row struct {
		id                        int64
		session, text, created    string
	}
	var msgs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.session, &r.text, &r.created); err != nil {
			rows.Close()
			return 0, err
		}
		msgs = append(msgs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, r := range msgs {
		text := indexableText([]byte(r.text))
		if strings.TrimSpace(text) == "" {
			continue
		}
		if err := insertIndex(ctx, tx, kindMessage, strconv.FormatInt(r.id, 10), r.session, r.created, text); err != nil {
			return 0, err
		}
		n++
	}

	srows, err := tx.QueryContext(ctx, `SELECT session_id, text, created_at FROM summaries`)
	if err != nil {
		return 0, err
	}
	var sums []row
	for srows.Next() {
		var r row
		if err := srows.Scan(&r.session, &r.text, &r.created); err != nil {
			srows.Close()
			return 0, err
		}
		sums = append(sums, r)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return 0, err
	}
	for _, r := range sums {
		if err := insertIndex(ctx, tx, kindSummary, r.session, r.session, r.created, r.text); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// backfillRecall populates an index that has never been written. The guard is
// deliberately "no rows at all": a database with history and an empty index is
// either an upgrade or a wiped index, and both want the same repair.
func backfillRecall(db *sql.DB) error {
	var indexed, messages int
	if err := db.QueryRow(`SELECT count(*) FROM recall_fts`).Scan(&indexed); err != nil {
		return fmt.Errorf("inspect recall index: %w", err)
	}
	if indexed > 0 {
		return nil
	}
	if err := db.QueryRow(`SELECT count(*) FROM messages`).Scan(&messages); err != nil {
		return err
	}
	if messages == 0 {
		return nil
	}
	s := &Store{db: db}
	_, err := s.ReindexAll(context.Background())
	return err
}
```

- [ ] **Step 5: Hook the write paths**

In `internal/store/store.go`, add the shared timestamp helper next to `timeFormat`:

```go
func nowString() string { return time.Now().UTC().Format(timeFormat) }
```

In `AppendMessage`, after `res, err := tx.ExecContext(...)` succeeds and after `id, err := res.LastInsertId()`, and before `return id, tx.Commit()`, insert:

```go
	// The index write shares this transaction on purpose. An FTS insert into
	// the same database fails only for reasons -- disk full, corruption --
	// that would fail the message write too, so there is no case where losing
	// the archive buys a working index.
	if err := insertIndex(ctx, tx, kindMessage, strconv.FormatInt(id, 10), m.SessionID, now, indexableText(m.BlocksJSON)); err != nil {
		return 0, err
	}
```

(add `"strconv"` to the imports).

Replace `SetSummary`'s body with a transaction, so the upsert and the index replacement cannot diverge:

```go
func (s *Store) SetSummary(ctx context.Context, sessionID, summary string, throughSeq int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := nowString()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO summaries (session_id, text, through_seq, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET text = excluded.text, through_seq = excluded.through_seq, created_at = excluded.created_at`,
		sessionID, summary, throughSeq, now); err != nil {
		return fmt.Errorf("set summary: %w", err)
	}
	// A session has one summary, so the index has one row for it: replace
	// rather than append, or a compacted session accumulates stale text.
	if err := deleteIndex(ctx, tx, kindSummary, sessionID); err != nil {
		return err
	}
	if err := insertIndex(ctx, tx, kindSummary, sessionID, sessionID, now, summary); err != nil {
		return err
	}
	return tx.Commit()
}
```

In `Open`, after the `db.Exec(schemaSQL)` block succeeds, add:

```go
	if err := backfillRecall(db); err != nil {
		db.Close()
		return nil, err
	}
```

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v 2>&1 | tail -40` then `make test 2>&1 | tail -25`
Expected: all PASS. If `TestDeletingASessionClearsItsIndexRows` fails, check `PRAGMA foreign_keys` is on and the triggers exist — the cascade from `sessions` to `messages` is what fires them.

- [ ] **Step 7: Verify the trust-boundary test is load-bearing**

Temporarily change `indexableText` to append `b.Content` for every block type. Run `go test -tags sqlite_fts5 ./internal/store/`. Expected: `TestToolResultBlocksAreNeverIndexed` and `TestMixedMessageIndexesOnlyItsText` both FAIL. **Revert.**

- [ ] **Step 8: Commit**

```bash
make fmtcheck && make test
git add internal/store
git commit -m "feat(store): FTS5 recall index over messages, summaries and facts"
```

---

### Task 4: `internal/recall` and the `sqlitefts` backend

**Files:**
- Create: `internal/recall/recall.go`
- Create: `internal/recall/query.go`
- Create: `internal/recall/sqlitefts/sqlitefts.go`
- Test: `internal/recall/query_test.go`
- Test: `internal/recall/sqlitefts/sqlitefts_test.go`

**Interfaces:**
- Consumes: the `recall_fts` table shape from Task 3.
- Produces: `recall.Chunk`, `recall.Hit`, `recall.Query`, `recall.Status`, the `recall.Recall` interface, `recall.Tokenize(string) string`, kind constants `recall.KindMessage/KindSummary/KindFact`, `recall.DefaultK = 8`, `recall.MaxK = 25`; `sqlitefts.New(q Queryer) *Backend` where `Queryer` is satisfied by `*sql.DB`.

- [ ] **Step 1: Write the failing tests**

Create `internal/recall/query_test.go`:

```go
package recall

import "testing"

// FTS5 reads punctuation and bare keywords as query syntax, so raw user text
// is frequently a syntax error rather than an empty result. Every input must
// come out as a literal token conjunction.
func TestTokenizeQuotesEveryToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{`retry logic`, `"retry" "logic"`},
		{`what's the -v flag?`, `"what's" "the" "v" "flag"`},
		{`retry "logic`, `"retry" "logic"`},
		{`AND`, `"AND"`},
		{`a OR b`, `"a" "OR" "b"`},
		{`foo*`, `"foo"`},
		{`NEAR(x y)`, `"NEAR" "x" "y"`},
		{`naïve café`, `"naïve" "café"`},
		{`snake_case`, `"snake_case"`},
		{`  spaced   out  `, `"spaced" "out"`},
		{``, ``},
		{`   `, ``},
		{`!@#$%^&*()`, ``},
	}
	for _, c := range cases {
		if got := Tokenize(c.in); got != c.want {
			t.Errorf("Tokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The store repeats these strings because it cannot import this package.
func TestKindConstantsMatchTheStore(t *testing.T) {
	if KindMessage != "message" || KindSummary != "summary" || KindFact != "fact" {
		t.Fatal("kind constants drifted from the values internal/store writes")
	}
}
```

Create `internal/recall/sqlitefts/sqlitefts_test.go`:

```go
package sqlitefts

import (
	"context"
	"database/sql"
	"testing"

	"github.com/codered/spore/internal/recall"
	_ "github.com/mattn/go-sqlite3"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE VIRTUAL TABLE recall_fts USING fts5(
		text, kind UNINDEXED, ref_id UNINDEXED, session_id UNINDEXED, created_at UNINDEXED)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func seed(t *testing.T, b *Backend, chunks ...recall.Chunk) {
	t.Helper()
	if err := b.Index(context.Background(), chunks); err != nil {
		t.Fatal(err)
	}
}

func chunk(kind, id, session, text string) recall.Chunk {
	return recall.Chunk{ID: id, Kind: kind, SessionID: session, Text: text}
}

// bm25 scores are negative and more-negative is better, so best-first is
// ORDER BY score ASC. Sorting the other way silently returns the worst hits.
func TestSearchRanksBestMatchFirst(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s1", "the retry logic uses exponential backoff"),
		chunk(recall.KindMessage, "2", "s1", "retry retry retry retry"),
		chunk(recall.KindMessage, "3", "s1", "unrelated gardening notes"),
	)
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("got %d hits, want 2", len(hits))
	}
	if hits[0].ID != "2" {
		t.Fatalf("best match is not first: %+v", hits)
	}
	if hits[0].Score > hits[1].Score {
		t.Fatalf("scores not ascending (bm25 is negative): %v %v", hits[0].Score, hits[1].Score)
	}
}

func TestSearchIsAConjunction(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s1", "retry logic here"),
		chunk(recall.KindMessage, "2", "s1", "retry only"),
	)
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry logic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("want only the chunk containing both terms, got %+v", hits)
	}
}

// Every one of these is a syntax error when passed to MATCH unquoted.
func TestSearchSurvivesRawUserInput(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "the flag is documented"))
	for _, q := range []string{`what's the -v flag?`, `retry "logic`, `AND`, `a OR`, `foo*`, `NEAR`, `naïve café`, ``, `   `, `!!!`} {
		if _, err := b.Search(context.Background(), recall.Query{Text: q}); err != nil {
			t.Errorf("Search(%q) returned an error: %v", q, err)
		}
	}
}

func TestEmptyQueryReturnsNothingNotAnError(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "some text"))
	hits, err := b.Search(context.Background(), recall.Query{Text: "   "})
	if err != nil || len(hits) != 0 {
		t.Fatalf("hits=%v err=%v; want no hits and no error", hits, err)
	}
}

func TestSearchFiltersBySessionAndKind(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "mine", "shared secret word"),
		chunk(recall.KindMessage, "2", "theirs", "shared secret word"),
		chunk(recall.KindFact, "afact", "", "shared secret word"),
	)
	hits, err := b.Search(context.Background(), recall.Query{
		Text: "secret", SessionID: "mine", Kinds: []string{recall.KindMessage, recall.KindSummary},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "1" {
		t.Fatalf("scoping leaked: %+v", hits)
	}
}

func TestSearchClampsK(t *testing.T) {
	b := New(newDB(t))
	var chunks []recall.Chunk
	for i := 0; i < 40; i++ {
		chunks = append(chunks, chunk(recall.KindMessage, string(rune('a'+i%26))+string(rune('a'+i/26)), "s", "common word"))
	}
	seed(t, b, chunks...)
	for _, tc := range []struct{ k, want int }{{0, recall.DefaultK}, {-3, recall.DefaultK}, {5, 5}, {999, recall.MaxK}} {
		hits, err := b.Search(context.Background(), recall.Query{Text: "common", K: tc.k})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != tc.want {
			t.Errorf("K=%d returned %d hits, want %d", tc.k, len(hits), tc.want)
		}
	}
}

func TestHitsCarryAnExcerpt(t *testing.T) {
	b := New(newDB(t))
	seed(t, b, chunk(recall.KindMessage, "1", "s1", "a long preamble and then the retry logic and more after it"))
	hits, err := b.Search(context.Background(), recall.Query{Text: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Excerpt == "" {
		t.Fatalf("no excerpt: %+v", hits)
	}
}

func TestStatusCountsByKind(t *testing.T) {
	b := New(newDB(t))
	seed(t, b,
		chunk(recall.KindMessage, "1", "s", "one"),
		chunk(recall.KindMessage, "2", "s", "two"),
		chunk(recall.KindFact, "f", "", "three"),
	)
	st, err := b.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Backend != "sqlitefts" || st.Degraded {
		t.Fatalf("bad status: %+v", st)
	}
	if st.Counts[recall.KindMessage] != 2 || st.Counts[recall.KindFact] != 1 {
		t.Fatalf("bad counts: %+v", st.Counts)
	}
}

var _ recall.Recall = (*Backend)(nil)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/recall/... 2>&1 | head -20`
Expected: FAIL — the packages do not exist.

- [ ] **Step 3: Write `internal/recall/recall.go`**

```go
// Package recall declares the search interface spore's memory hides behind.
// The default backend is keyword search over SQLite FTS5 and ships in Plan 5a;
// a semantic backend implements the same interface in 5b, which is why every
// caller depends on this package and never on a backend.
package recall

import (
	"context"
	"time"
)

// Kinds of indexed content. Tool results are deliberately absent: they carry
// third-party text, and making it retrievable would let one injected page
// reappear in a later turn.
const (
	KindMessage = "message"
	KindSummary = "summary"
	KindFact    = "fact"
)

const (
	// DefaultK is the hit count when a caller names none.
	DefaultK = 8
	// MaxK bounds one search, so a model cannot spend a turn's whole budget
	// on retrieved text.
	MaxK = 25
)

// Chunk is one indexed unit. ID is the kind's own identifier: a message id, a
// session id for a summary, a fact name for a fact.
type Chunk struct {
	ID        string
	Kind      string
	Text      string
	SessionID string
	CreatedAt time.Time
}

// Hit is a Chunk with its match. Score's meaning is backend-defined; only the
// ordering the backend returns is contractual.
type Hit struct {
	Chunk
	Score   float64
	Excerpt string
}

// Query is a struct rather than positional arguments because scoping is not
// optional: the recall_search tool narrows by session and kind under the
// remote trust profile, and a second backend must not force a signature change
// to gain a filter.
type Query struct {
	Text      string
	K         int
	Kinds     []string
	SessionID string
}

// Status describes the backend for `spore recall status`. Degraded is always
// false for a local backend; it exists for the 5b case where a configured
// vector store is unreachable and search has fallen back.
type Status struct {
	Backend  string
	Counts   map[string]int
	Degraded bool
	Reason   string
}

type Recall interface {
	Index(ctx context.Context, chunks []Chunk) error
	Search(ctx context.Context, q Query) ([]Hit, error)
	Status(ctx context.Context) (Status, error)
}

// ClampK applies DefaultK and MaxK. Backends share it so `k` means the same
// thing whichever one is configured.
func ClampK(k int) int {
	switch {
	case k <= 0:
		return DefaultK
	case k > MaxK:
		return MaxK
	default:
		return k
	}
}
```

- [ ] **Step 4: Write `internal/recall/query.go`**

```go
package recall

import "strings"

// Tokenize turns arbitrary text into an FTS5 MATCH expression that cannot be
// a syntax error. FTS5 reads `"`, `*`, `-`, `AND`, `OR` and `NEAR` as syntax,
// so a natural-language question fails to parse rather than returning nothing:
// splitting on non-word runes and quoting each token makes every query a
// literal conjunction of terms.
//
// An input with no word characters returns "", and the caller MUST treat that
// as "no hits" rather than passing it to MATCH, which is itself an error.
func Tokenize(q string) string {
	var b strings.Builder
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		// A double quote cannot survive inside a quoted FTS5 string, and
		// tokenising already dropped it; nothing here can reintroduce one.
		b.WriteString(string(cur))
		b.WriteByte('"')
		cur = cur[:0]
	}
	for _, r := range q {
		if isWordRune(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return b.String()
}

// isWordRune keeps letters, digits, underscores, apostrophes and everything
// above ASCII, so accented words survive as single tokens.
func isWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '\'':
		return true
	case r > 127:
		return true
	}
	return false
}
```

- [ ] **Step 5: Write `internal/recall/sqlitefts/sqlitefts.go`**

```go
// Package sqlitefts implements recall.Recall over the recall_fts table in
// spore's own database. It is the default backend: no setup, no service, and
// available wherever the binary runs.
package sqlitefts

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/recall"
)

// Queryer is the slice of *sql.DB this backend needs. Taking an interface
// keeps the backend testable against a bare in-memory database, without the
// store's session machinery.
type Queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type Backend struct{ db Queryer }

func New(db Queryer) *Backend { return &Backend{db: db} }

// timeFormat matches the store's, so parsing a created_at written by either
// side yields the same instant.
const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

// Index writes chunks directly. The store already indexes messages and
// summaries inside their own transactions; this path exists for facts and for
// tests, and replaces any row with the same kind and id.
func (b *Backend) Index(ctx context.Context, chunks []recall.Chunk) error {
	for _, c := range chunks {
		if _, err := b.db.ExecContext(ctx,
			`DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?`, c.Kind, c.ID); err != nil {
			return fmt.Errorf("replace %s %s: %w", c.Kind, c.ID, err)
		}
		created := c.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, err := b.db.ExecContext(ctx,
			`INSERT INTO recall_fts (text, kind, ref_id, session_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			c.Text, c.Kind, c.ID, c.SessionID, created.UTC().Format(timeFormat)); err != nil {
			return fmt.Errorf("index %s %s: %w", c.Kind, c.ID, err)
		}
	}
	return nil
}

func (b *Backend) Search(ctx context.Context, q recall.Query) ([]recall.Hit, error) {
	match := recall.Tokenize(q.Text)
	if match == "" {
		// An empty MATCH expression is a syntax error, and a query with no
		// searchable characters has no hits by definition.
		return nil, nil
	}

	// bm25 is negative and more-negative is a better match, so best-first is
	// ascending. Ordering descending here would silently return the worst hits.
	sb := strings.Builder{}
	sb.WriteString(`SELECT text, kind, ref_id, session_id, created_at,
		bm25(recall_fts) AS score, snippet(recall_fts, 0, '', '', '…', 12)
		FROM recall_fts WHERE recall_fts MATCH ?`)
	args := []any{match}
	if len(q.Kinds) > 0 {
		sb.WriteString(" AND kind IN (")
		for i, k := range q.Kinds {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteByte('?')
			args = append(args, k)
		}
		sb.WriteByte(')')
	}
	if q.SessionID != "" {
		sb.WriteString(" AND session_id = ?")
		args = append(args, q.SessionID)
	}
	sb.WriteString(" ORDER BY score LIMIT ?")
	args = append(args, recall.ClampK(q.K))

	rows, err := b.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("recall search: %w", err)
	}
	defer rows.Close()

	var hits []recall.Hit
	for rows.Next() {
		var h recall.Hit
		var created string
		if err := rows.Scan(&h.Text, &h.Kind, &h.ID, &h.SessionID, &created, &h.Score, &h.Excerpt); err != nil {
			return nil, err
		}
		h.CreatedAt, _ = time.Parse(timeFormat, created)
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func (b *Backend) Status(ctx context.Context) (recall.Status, error) {
	st := recall.Status{Backend: "sqlitefts", Counts: map[string]int{}}
	rows, err := b.db.QueryContext(ctx, `SELECT kind, count(*) FROM recall_fts GROUP BY kind`)
	if err != nil {
		return st, fmt.Errorf("recall status: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return st, err
		}
		st.Counts[kind] = n
	}
	return st, rows.Err()
}
```

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/recall/... -v 2>&1 | tail -40` then `make test 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 7: Verify the ranking test is load-bearing**

Change `ORDER BY score` to `ORDER BY score DESC`. Expected: `TestSearchRanksBestMatchFirst` FAILS. **Revert.** Then delete the `if match == ""` short-circuit and confirm `TestEmptyQueryReturnsNothingNotAnError` and `TestSearchSurvivesRawUserInput` FAIL. **Revert.**

- [ ] **Step 8: Commit**

```bash
make fmtcheck && make test
git add internal/recall
git commit -m "feat(recall): Recall interface and the sqlitefts keyword backend"
```

---

### Task 5: The `recall_search` tool

**Files:**
- Create: `internal/tool/mem/recall.go`
- Create: `internal/tool/mem/mem.go`
- Modify: `internal/trace/trace.go`
- Modify: `internal/config/config.go` (`Default`)
- Test: `internal/tool/mem/recall_test.go`

**Interfaces:**
- Consumes: `recall.Recall`, `recall.Query`, `recall.Hit`, `recall.ClampK` (Task 4); `policy.SessionFrom` (existing).
- Produces: `mem.NewRecallSearch(r recall.Recall) tool.Tool`; `trace.StartRetriever(ctx context.Context, backend, query string, k int) (context.Context, trace.Span)` and `trace.EndRetriever(span trace.Span, hits int, ids []string, scores []float64)`.

- [ ] **Step 1: Write the failing test**

Create `internal/tool/mem/recall_test.go`:

```go
package mem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/recall"
)

// fakeRecall records the query it was given, which is the only way to prove
// scoping happened before the backend saw it.
type fakeRecall struct {
	got  recall.Query
	hits []recall.Hit
	err  error
}

func (f *fakeRecall) Index(context.Context, []recall.Chunk) error { return nil }
func (f *fakeRecall) Search(_ context.Context, q recall.Query) ([]recall.Hit, error) {
	f.got = q
	return f.hits, f.err
}
func (f *fakeRecall) Status(context.Context) (recall.Status, error) {
	return recall.Status{Backend: "fake"}, nil
}

func hit(kind, id, session, text, excerpt string) recall.Hit {
	return recall.Hit{
		Chunk:   recall.Chunk{ID: id, Kind: kind, SessionID: session, Text: text},
		Excerpt: excerpt,
	}
}

func localCtx(sid string) context.Context {
	return policy.WithSession(context.Background(), sid, policy.ProfileLocal)
}

func TestRecallSearchRendersHits(t *testing.T) {
	f := &fakeRecall{hits: []recall.Hit{
		hit(recall.KindMessage, "12", "sess", "full message text", "…excerpt…"),
	}}
	out, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"retry"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "message") || !strings.Contains(out, "…excerpt…") {
		t.Fatalf("hit not rendered: %q", out)
	}
}

// A fact is short and is the retrieval path for a fact that did not fit the
// context budget, so it comes back whole rather than as a snippet.
func TestRecallSearchReturnsWholeFactBodies(t *testing.T) {
	f := &fakeRecall{hits: []recall.Hit{
		hit(recall.KindFact, "prefers-tabs", "", "the entire fact body", "…snip…"),
	}}
	out, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"tabs"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the entire fact body") {
		t.Fatalf("fact body not returned in full: %q", out)
	}
}

func TestRecallSearchNoHits(t *testing.T) {
	out, err := NewRecallSearch(&fakeRecall{}).Call(localCtx("sess"), json.RawMessage(`{"query":"nothing"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches") {
		t.Fatalf("empty result should say so plainly: %q", out)
	}
}

func TestRecallSearchLocalProfileIsUnscoped(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(localCtx("sess"), json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.SessionID != "" || len(f.got.Kinds) != 0 {
		t.Fatalf("local search was scoped: %+v", f.got)
	}
}

// The policy engine gates tool names, not result scope, so the tool owns this:
// a remote session must never search another session's history or any fact.
func TestRecallSearchRemoteProfileIsScoped(t *testing.T) {
	f := &fakeRecall{}
	ctx := policy.WithSession(context.Background(), "remote-sess", policy.ProfileRemote)
	if _, err := NewRecallSearch(f).Call(ctx, json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.SessionID != "remote-sess" {
		t.Fatalf("remote search not pinned to its own session: %+v", f.got)
	}
	for _, k := range f.got.Kinds {
		if k == recall.KindFact {
			t.Fatal("remote search may read facts")
		}
	}
	if len(f.got.Kinds) == 0 {
		t.Fatal("remote search did not restrict kinds at all")
	}
}

// SessionFrom reports the least-trusted profile when nothing is attached, so a
// caller that forgot WithSession must get the scoped behaviour, not the open one.
func TestRecallSearchWithNoSessionOnContextIsScoped(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(context.Background(), json.RawMessage(`{"query":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if len(f.got.Kinds) == 0 {
		t.Fatal("a context with no session was treated as trusted")
	}
}

func TestRecallSearchClampsK(t *testing.T) {
	f := &fakeRecall{}
	if _, err := NewRecallSearch(f).Call(localCtx("s"), json.RawMessage(`{"query":"x","k":9999}`)); err != nil {
		t.Fatal(err)
	}
	if f.got.K != recall.MaxK {
		t.Fatalf("K = %d, want %d", f.got.K, recall.MaxK)
	}
}

func TestRecallSearchRejectsAnEmptyQuery(t *testing.T) {
	if _, err := NewRecallSearch(&fakeRecall{}).Call(localCtx("s"), json.RawMessage(`{"query":"  "}`)); err == nil {
		t.Fatal("empty query accepted")
	}
}

func TestRecallSearchIsReadOnly(t *testing.T) {
	if !NewRecallSearch(&fakeRecall{}).ReadOnly() {
		t.Fatal("recall_search must be read-only so it can dispatch concurrently")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/tool/mem/ 2>&1 | head -20`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Add the retriever span helpers**

In `internal/trace/trace.go`, extend the existing attribute-key `const` block by replacing its last line (`attrToolResultLen  = "spore.tool.result_bytes"`) with:

```go
	attrToolResultLen    = "spore.tool.result_bytes"
	attrRetrievalBackend = "spore.recall.backend"
	attrRetrievalK       = "spore.recall.k"
	attrRetrievalHits    = "spore.recall.hits"
```

and add:

```go
// StartRetriever opens the retriever span Phoenix renders natively. The query
// is prompt-shaped text, so it is dropped when redacting; the shape of the
// search is kept either way.
func StartRetriever(ctx context.Context, backend, query string, k int) (context.Context, Span) {
	kv := []attribute.KeyValue{
		attribute.String(attrSpanKind, "RETRIEVER"),
		attribute.String(attrRetrievalBackend, backend),
		attribute.Int(attrRetrievalK, k),
	}
	if !redact.Load() {
		kv = append(kv, attribute.String(attrInput, query))
	}
	return tracer().Start(ctx, "recall.search", oteltrace.WithAttributes(kv...))
}

// EndRetriever records which documents came back. Ids and scores are index
// metadata rather than content, so they survive redaction.
func EndRetriever(span Span, ids []string, scores []float64) {
	span.SetAttributes(
		attribute.Int(attrRetrievalHits, len(ids)),
		attribute.StringSlice("retrieval.documents.ids", ids),
		attribute.Float64Slice("retrieval.documents.scores", scores),
	)
	span.End()
}
```

- [ ] **Step 4: Write `internal/tool/mem/mem.go`**

```go
// Package mem exposes spore's memory layers to the model: recall_search over
// the index, and memory over the fact files. Both live here because they are
// two views of one subsystem, and because the fact directory is the only path
// either of them may touch.
package mem

import (
	"encoding/json"
	"fmt"
)

func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Write `internal/tool/mem/recall.go`**

```go
package mem

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/trace"
)

type recallSearch struct{ r recall.Recall }

// NewRecallSearch builds the read-only search tool over whichever backend is
// configured.
func NewRecallSearch(r recall.Recall) tool.Tool { return recallSearch{r: r} }

func (recallSearch) Name() string { return "recall_search" }

func (recallSearch) Description() string {
	return "Search earlier conversations, compaction summaries and memory facts by keyword. " +
		"Use it to recover something discussed in another session, or to read a memory fact " +
		"whose body did not fit in context."
}

func (recallSearch) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "query": {"type": "string", "description": "Keywords to search for."},
	    "k": {"type": "integer", "description": "Maximum hits to return (default 8, maximum 25)."}
	  },
	  "required": ["query"]
	}`)
}

// ReadOnly is true: search mutates nothing, so the loop may dispatch it
// concurrently with other read-only calls.
func (recallSearch) ReadOnly() bool { return true }

func (t recallSearch) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}

	q := recall.Query{Text: a.Query, K: recall.ClampK(a.K)}

	// The policy engine gates tool names and their arguments, not the scope of
	// a result, so this restriction has to live in the tool. A remote session
	// is an admitted chat user, not the operator: it may search its own
	// conversation and nothing else, and it may not read facts at all.
	sessionID, profile := policy.SessionFrom(ctx)
	if profile == policy.ProfileRemote {
		q.SessionID = sessionID
		q.Kinds = []string{recall.KindMessage, recall.KindSummary}
	}

	status, _ := t.r.Status(ctx)
	backend := status.Backend
	if backend == "" {
		backend = "unknown"
	}
	ctx, span := trace.StartRetriever(ctx, backend, a.Query, q.K)

	hits, err := t.r.Search(ctx, q)
	if err != nil {
		span.End()
		return "", fmt.Errorf("recall search failed: %w", err)
	}
	ids := make([]string, 0, len(hits))
	scores := make([]float64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.Kind+":"+h.ID)
		scores = append(scores, h.Score)
	}
	trace.EndRetriever(span, ids, scores)

	if len(hits) == 0 {
		return "no matches", nil
	}
	var b strings.Builder
	for i, h := range hits {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "[%s", h.Kind)
		if h.SessionID != "" {
			fmt.Fprintf(&b, " · session %s", h.SessionID)
		}
		if !h.CreatedAt.IsZero() {
			fmt.Fprintf(&b, " · %s", h.CreatedAt.Format("2006-01-02"))
		}
		if h.Kind == recall.KindFact {
			fmt.Fprintf(&b, " · %s", h.ID)
		}
		b.WriteString("]\n")
		// A fact is short and is the retrieval path for one that did not fit
		// the context budget, so it comes back whole. Everything else is a
		// snippet: the point is to locate the conversation, not replay it.
		if h.Kind == recall.KindFact {
			b.WriteString(h.Text)
		} else {
			b.WriteString(h.Excerpt)
		}
	}
	return b.String(), nil
}
```

- [ ] **Step 6: Allow the tool in the default policy**

In `internal/config/config.go`, `Default()`, add `"recall_search"` to the end of the `Allow` slice:

```go
			Allow:           []string{"fs_read", "fs_list", "fs_glob", "fs_grep", "web_*", "schedule_list", "recall_search"},
```

- [ ] **Step 7: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/tool/mem/ ./internal/trace/ ./internal/config/ -v 2>&1 | tail -40` then `make test 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 8: Verify the scoping test is load-bearing**

Delete the `if profile == policy.ProfileRemote { ... }` block. Expected: `TestRecallSearchRemoteProfileIsScoped` and `TestRecallSearchWithNoSessionOnContextIsScoped` FAIL. **Restore it.**

- [ ] **Step 9: Commit**

```bash
make fmtcheck && make test
git add internal/tool/mem internal/trace internal/config
git commit -m "feat(tool): recall_search, scoped to its own session under the remote profile"
```

---

### Task 6: The `memory` tool

**Files:**
- Create: `internal/tool/mem/memory.go`
- Modify: `internal/config/config.go` (`Default`)
- Test: `internal/tool/mem/memory_test.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `memory.Cache`, `memory.Fact`, `memory.Write`, `memory.Delete` (Task 1); `store.IndexFact`, `store.UnindexFact` (Task 3).
- Produces: `mem.NewMemory(cache *memory.Cache, idx FactIndexer) tool.Tool`, where `type FactIndexer interface { IndexFact(ctx context.Context, name, text string) error; UnindexFact(ctx context.Context, name string) error }`.

- [ ] **Step 1: Write the failing test**

Create `internal/tool/mem/memory_test.go`:

```go
package mem

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/memory"
)

type fakeIndex struct {
	indexed   map[string]string
	unindexed []string
}

func newFakeIndex() *fakeIndex { return &fakeIndex{indexed: map[string]string{}} }

func (f *fakeIndex) IndexFact(_ context.Context, name, text string) error {
	f.indexed[name] = text
	return nil
}
func (f *fakeIndex) UnindexFact(_ context.Context, name string) error {
	f.unindexed = append(f.unindexed, name)
	return nil
}

func TestMemoryWriteCreatesReloadsAndIndexes(t *testing.T) {
	dir := t.TempDir()
	cache := memory.NewCache(dir)
	cache.Reload()
	idx := newFakeIndex()

	out, err := NewMemory(cache, idx).Call(context.Background(), json.RawMessage(
		`{"op":"write","name":"prefers-tabs","description":"formatting","type":"user","body":"Tabs."}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("result does not name the fact: %q", out)
	}
	facts := cache.Facts()
	if len(facts) != 1 || facts[0].Body != "Tabs." {
		t.Fatalf("cache not reloaded after the write: %+v", facts)
	}
	if got := idx.indexed["prefers-tabs"]; !strings.Contains(got, "Tabs.") {
		t.Fatalf("fact not indexed: %q", got)
	}
	if !strings.Contains(idx.indexed["prefers-tabs"], "formatting") {
		t.Fatal("the description should be searchable alongside the body")
	}
}

func TestMemoryDeleteRemovesReloadsAndUnindexes(t *testing.T) {
	dir := t.TempDir()
	if err := memory.Write(dir, memory.Fact{Name: "old", Description: "d", Type: "user", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	cache := memory.NewCache(dir)
	cache.Reload()
	idx := newFakeIndex()

	if _, err := NewMemory(cache, idx).Call(context.Background(), json.RawMessage(`{"op":"delete","name":"old"}`)); err != nil {
		t.Fatal(err)
	}
	if len(cache.Facts()) != 0 {
		t.Fatal("cache still holds the deleted fact")
	}
	if len(idx.unindexed) != 1 || idx.unindexed[0] != "old" {
		t.Fatalf("fact not removed from the index: %v", idx.unindexed)
	}
}

func TestMemoryRejectsBadInput(t *testing.T) {
	tl := NewMemory(memory.NewCache(t.TempDir()), newFakeIndex())
	for _, args := range []string{
		`{"op":"wibble","name":"x"}`,
		`{"op":"write","name":"../escape","description":"d","type":"user","body":"b"}`,
		`{"op":"write","name":"ok","type":"user","body":"b"}`,
		`{"op":"write","name":"ok","description":"d","type":"nope","body":"b"}`,
		`{"op":"delete"}`,
		`{"op":"delete","name":"missing"}`,
		`{}`,
	} {
		if _, err := tl.Call(context.Background(), json.RawMessage(args)); err == nil {
			t.Errorf("accepted bad args: %s", args)
		}
	}
}

// A failed write must leave nothing behind for the index to disagree with.
func TestMemoryDoesNotIndexAFailedWrite(t *testing.T) {
	idx := newFakeIndex()
	tl := NewMemory(memory.NewCache(t.TempDir()), idx)
	if _, err := tl.Call(context.Background(), json.RawMessage(
		`{"op":"write","name":"bad name","description":"d","type":"user","body":"b"}`)); err == nil {
		t.Fatal("expected a rejection")
	}
	if len(idx.indexed) != 0 {
		t.Fatalf("indexed a fact that was never written: %v", idx.indexed)
	}
}

func TestMemoryIsNotReadOnly(t *testing.T) {
	if NewMemory(memory.NewCache(t.TempDir()), newFakeIndex()).ReadOnly() {
		t.Fatal("memory writes files and must not be dispatched concurrently as read-only")
	}
}
```

Add to `internal/config/config_test.go`:

```go
// A fact shapes every later turn in every session, so an admitted chat user
// must never be able to write one.
func TestDefaultDeniesMemoryToRemoteSessions(t *testing.T) {
	remote, ok := Default().Policy.Profiles["remote"]
	if !ok {
		t.Fatal("no remote profile")
	}
	var found bool
	for _, r := range remote.Deny {
		if r == "memory" {
			found = true
		}
	}
	if !found {
		t.Fatalf("remote profile does not deny memory: %v", remote.Deny)
	}
}

func TestDefaultAsksBeforeWritingAFact(t *testing.T) {
	var found bool
	for _, r := range Default().Policy.Ask {
		if r == "memory" {
			found = true
		}
	}
	if !found {
		t.Fatal("memory is not in the default ask list")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/tool/mem/ ./internal/config/ 2>&1 | head -20`
Expected: FAIL — `NewMemory` undefined; the config assertions fail.

- [ ] **Step 3: Write `internal/tool/mem/memory.go`**

```go
package mem

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/tool"
)

// FactIndexer is the slice of the store this tool needs. Facts are file-owned,
// so writing one is two steps -- the file, then the index -- and the tool is
// where they are sequenced.
type FactIndexer interface {
	IndexFact(ctx context.Context, name, text string) error
	UnindexFact(ctx context.Context, name string) error
}

type memoryTool struct {
	cache *memory.Cache
	idx   FactIndexer
}

// NewMemory builds the fact-writing tool. It is `ask` in the default policy
// and denied under the remote profile: a fact written once shapes every later
// turn in every session, so a human sees each one before it lands.
func NewMemory(cache *memory.Cache, idx FactIndexer) tool.Tool {
	return memoryTool{cache: cache, idx: idx}
}

func (memoryTool) Name() string { return "memory" }

func (memoryTool) Description() string {
	return "Write or delete a memory fact: a short markdown file about the user, the project, " +
		"or how they want you to work, loaded into every future conversation. " +
		"Write a fact when the user tells you something worth remembering beyond this session."
}

func (memoryTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "op": {"type": "string", "enum": ["write", "delete"]},
	    "name": {"type": "string", "description": "Lowercase kebab-case identifier, e.g. prefers-tabs."},
	    "description": {"type": "string", "description": "One line saying what the fact covers. Required for write."},
	    "type": {"type": "string", "enum": ["user", "feedback", "project", "reference"]},
	    "body": {"type": "string", "description": "The fact itself, in markdown. Required for write."}
	  },
	  "required": ["op", "name"]
	}`)
}

// ReadOnly is false: this writes files, so the loop must not dispatch it
// alongside other calls.
func (memoryTool) ReadOnly() bool { return false }

func (t memoryTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Op          string `json:"op"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Type        string `json:"type"`
		Body        string `json:"body"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	dir := t.cache.Dir()

	switch a.Op {
	case "write":
		f := memory.Fact{Name: a.Name, Description: a.Description, Type: a.Type, Body: a.Body}
		if err := memory.Write(dir, f); err != nil {
			return "", err
		}
		// Reload before indexing: the cache is what the next turn assembles
		// from, and a fact the model cannot see is worse than one it cannot
		// search.
		t.reload()
		// The description is indexed with the body so a search for what a fact
		// is about finds it even when the body words differ.
		if err := t.idx.IndexFact(ctx, f.Name, f.Description+"\n"+f.Body); err != nil {
			return "", fmt.Errorf("fact %q was written but could not be indexed: %w", f.Name, err)
		}
		return fmt.Sprintf("wrote fact %q", f.Name), nil

	case "delete":
		if err := memory.Delete(dir, a.Name); err != nil {
			return "", err
		}
		t.reload()
		if err := t.idx.UnindexFact(ctx, a.Name); err != nil {
			return "", fmt.Errorf("fact %q was deleted but could not be unindexed: %w", a.Name, err)
		}
		return fmt.Sprintf("deleted fact %q", a.Name), nil

	default:
		return "", fmt.Errorf("op must be write or delete, got %q", a.Op)
	}
}

// reload refreshes the fact set the next turn will assemble. Parse errors are
// not returned to the model: the write it just made succeeded, and someone
// else's malformed file is not this call's failure.
func (t memoryTool) reload() { t.cache.Reload() }
```

- [ ] **Step 4: Update the default policy**

In `internal/config/config.go`, `Default()`:

```go
			Ask:             []string{"fs_write", "fs_edit", "shell_exec", "schedule_create", "schedule_cancel", "mcp__*", "memory"},
```

and extend the remote profile, keeping the existing comment and adding to it:

```go
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
				"remote": {Deny: []string{"mcp__*", "memory"}},
			},
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/tool/mem/ ./internal/config/ -v 2>&1 | tail -40` then `make test 2>&1 | tail -20`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
make fmtcheck && make test
git add internal/tool/mem internal/config
git commit -m "feat(tool): memory tool for fact files, ask locally and denied remotely"
```

---

### Task 7: Wiring

**Files:**
- Modify: `cmd/spore/wire.go`
- Modify: `internal/agent/agent.go`
- Test: `internal/agent/agent_test.go`
- Test: `cmd/spore/wire_test.go` (create if absent)

**Interfaces:**
- Consumes: everything from Tasks 1–6.
- Produces: `agent.Agent.Facts *memory.Cache` (nil means no facts); `buildTools` and `buildAgent` unchanged in signature, both wiring the two new tools.

- [ ] **Step 1: Write the failing tests**

Add to `internal/agent/agent_test.go`:

```go
func TestSnapshotIncludesFactsFromTheCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := memory.Write(dir, memory.Fact{Name: "a-fact", Description: "d", Type: "user", Body: "remembered"}); err != nil {
		t.Fatal(err)
	}
	cache := memory.NewCache(dir)
	cache.Reload()

	// Build an agent the way the surrounding tests in this file do, then
	// attach the cache.
	a := newTestAgent(t)
	a.Facts = cache

	sid, err := a.Store.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) != 1 || snap.Facts[0].Body != "remembered" {
		t.Fatalf("facts not loaded into the snapshot: %+v", snap.Facts)
	}
}

func TestSnapshotWithNoFactCacheIsEmpty(t *testing.T) {
	ctx := context.Background()
	a := newTestAgent(t)
	sid, _ := a.Store.CreateSession(ctx, "t")
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("a nil fact cache must not be an error: %v", err)
	}
	if len(snap.Facts) != 0 {
		t.Fatal("facts appeared with no cache attached")
	}
}
```

`newTestAgent` may not exist — read `internal/agent/agent_test.go` first and build the agent the way its existing tests do, factoring out a helper only if that is a small change. Do not restructure the file.

Create or extend `cmd/spore/wire_test.go`:

```go
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

// The two memory builtins must actually reach the registry, and the policy
// engine must judge them the way the defaults say. This is built through
// config.Load on a real file because Default() carries no baseline deny.
func TestMemoryToolsAreRegisteredAndGated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	guard, host, err := buildTools(cfg, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != nil {
		defer host.Close()
	}
	specs := guard.Specs()
	var haveRecall, haveMemory bool
	for _, s := range specs {
		switch s.Name {
		case "recall_search":
			haveRecall = true
		case "memory":
			haveMemory = true
		}
	}
	if !haveRecall || !haveMemory {
		t.Fatalf("memory builtins missing from the registry: %+v", specs)
	}

	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	decide := func(p policy.Profile, name string) policy.Decision {
		return engine.Evaluate(p, policy.Call{Tool: name, Args: json.RawMessage(`{}`)}).Decision
	}
	if d := decide(policy.ProfileRemote, "memory"); d != policy.DecisionDeny {
		t.Fatalf("remote memory decision = %v, want deny", d)
	}
	if d := decide(policy.ProfileLocal, "memory"); d != policy.DecisionAsk {
		t.Fatalf("local memory decision = %v, want ask", d)
	}
	if d := decide(policy.ProfileLocal, "recall_search"); d != policy.DecisionAllow {
		t.Fatalf("local recall_search decision = %v, want allow", d)
	}
}
```

These identifiers were checked against the tree while this plan was written: `(*policy.Engine).Evaluate(profile Profile, c Call) Result`, `policy.Call{Tool, Args}`, `Result.Decision`, the `DecisionAllow/Ask/Deny` and `ProfileLocal/Remote` constants, and `(*policy.Guard).Specs() []provider.ToolSpec`. Pass `Args: json.RawMessage(`{}`)` rather than nil — a rule predicate parses the arguments, and a malformed argument object is itself denied.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent/ ./cmd/spore/ 2>&1 | head -20`
Expected: FAIL — `Agent.Facts` undefined, tools not registered.

- [ ] **Step 3: Give the agent a fact cache**

In `internal/agent/agent.go`, add the field and populate the snapshot:

```go
type Agent struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	Tools    ToolRunner
	// Facts is the loaded fact set. Nil means no memory layer, which is what
	// a bare `spore once` runs with.
	Facts *memory.Cache
}
```

In `Snapshot`, where the snapshot value is built:

```go
	snap := Snapshot{System: a.Cfg.SystemPrompt, Summary: summary}
	if a.Facts != nil {
		snap.Facts = a.Facts.Facts()
	}
```

Add `"github.com/codered/spore/internal/memory"` to the imports.

- [ ] **Step 4: Wire the tools**

In `cmd/spore/wire.go`, add imports for `internal/memory`, `internal/recall/sqlitefts`, and `internal/tool/mem`, then inside `buildTools`, after the `schedule.New(st)` line:

```go
	// The fact cache is loaded once here; the memory tool reloads it after
	// each write, which is the only way the set changes while spore runs.
	factsDir := filepath.Join(cfg.DataDir, "memory")
	facts := memory.NewCache(factsDir)
	for _, err := range facts.Reload() {
		// A hand-edited fact that will not parse costs one fact and a warning,
		// never a failed startup.
		slog.Default().Warn("skipping malformed fact", "error", err)
	}
	recallBackend := sqlitefts.New(st.DB())
	tools = append(tools, mem.NewRecallSearch(recallBackend), mem.NewMemory(facts, st))
```

`buildTools` does not currently return the cache, and `buildAgent` needs it to set `Agent.Facts`. Rather than widen the already-three-valued return, build the cache in `buildAgent` and pass it down: change `buildTools` to take it as a parameter —

```go
func buildTools(cfg *config.Config, st *store.Store, facts *memory.Cache, approver policy.Approver) (*policy.Guard, *mcphost.Host, error)
```

— construct the cache in `buildAgent` before calling `buildTools`, set `a.Facts` on the agent it returns, and update every call site the compiler names. Keep the Reload-and-warn loop wherever the cache is constructed.

Also index the facts that were just loaded, so a fresh install can search them before anything is written:

```go
	for _, f := range facts.Facts() {
		if err := st.IndexFact(context.Background(), f.Name, f.Description+"\n"+f.Body); err != nil {
			slog.Default().Warn("indexing fact failed", "fact", f.Name, "error", err)
		}
	}
```

- [ ] **Step 5: Run the tests and check every call site**

Run: `grep -rn "buildTools(" --include='*.go' cmd/` and confirm each call passes the cache.
Run: `make build && make vet && make test 2>&1 | tail -25`
Expected: builds, vets clean, all packages ok.

- [ ] **Step 6: Smoke-test by hand**

```bash
make build
TMP=$(mktemp -d)
mkdir -p "$TMP/memory"
cat > "$TMP/memory/prefers-tabs.md" <<'EOT'
---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Tabs, gofmt defaults.
EOT
cat > "$TMP/config.toml" <<EOT
default_model = "p/m"
data_dir = "$TMP"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "$TMP"
EOT
./spore --config "$TMP/config.toml" policy 2>&1 | head -20
```

Expected: the command runs and lists policy including `recall_search` and `memory`. Confirm `$TMP/spore.db` exists afterwards. (Check `cmd/spore/main.go` for the actual config flag name before running.)

- [ ] **Step 7: Commit**

```bash
make fmtcheck && make build && make vet && make test
git add cmd/spore internal/agent
git commit -m "feat(wire): load facts into every turn and register the memory builtins"
```

---

### Task 8: `spore recall` and documentation

**Files:**
- Create: `cmd/spore/recall.go`
- Modify: `cmd/spore/main.go`
- Modify: `README.md`
- Test: `cmd/spore/recall_test.go`

**Interfaces:**
- Consumes: `sqlitefts.New`, `store.ReindexAll`, `store.IndexFact`, `memory.Load` (Tasks 1, 3, 4).
- Produces: `cmdRecall(ctx context.Context, cfg *config.Config, args []string) error` handling `search`, `status` and `reindex`.

- [ ] **Step 1: Write the failing test**

Create `cmd/spore/recall_test.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func recallFixture(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "prefers-tabs.md"),
		[]byte("---\nname: prefers-tabs\ndescription: formatting\ntype: user\n---\n\nTabs, always.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
default_model = "p/m"
data_dir = "`+dir+`"

[providers.p]
kind = "anthropic"
api_key = "x"

[policy]
workspace = "`+dir+`"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: "exponential backoff and jitter"}})
	if _, err := st.AppendMessage(ctx, store.Message{SessionID: sid, Role: "user", BlocksJSON: blocks}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := fn()
	w.Close()
	os.Stdout = old
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if runErr != nil {
		t.Fatalf("command failed: %v", runErr)
	}
	return sb.String()
}

func TestRecallSearchCommandFindsAMessage(t *testing.T) {
	cfg := recallFixture(t)
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "backoff"})
	})
	if !strings.Contains(out, "backoff") {
		t.Fatalf("search found nothing:\n%s", out)
	}
}

// reindex rebuilds messages from SQLite and facts from disk, which is the
// documented repair for an index that has drifted.
func TestRecallReindexRestoresBothSources(t *testing.T) {
	cfg := recallFixture(t)
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`DELETE FROM recall_fts`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })

	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "Tabs"})
	})
	if !strings.Contains(out, "prefers-tabs") {
		t.Fatalf("facts were not reindexed:\n%s", out)
	}
	out = captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"search", "backoff"})
	})
	if !strings.Contains(out, "backoff") {
		t.Fatalf("messages were not reindexed:\n%s", out)
	}
}

func TestRecallStatusReportsCounts(t *testing.T) {
	cfg := recallFixture(t)
	captureStdout(t, func() error { return cmdRecall(context.Background(), cfg, []string{"reindex"}) })
	out := captureStdout(t, func() error {
		return cmdRecall(context.Background(), cfg, []string{"status"})
	})
	if !strings.Contains(out, "sqlitefts") || !strings.Contains(out, "message") {
		t.Fatalf("status is not informative:\n%s", out)
	}
}

func TestRecallRejectsAnUnknownSubcommand(t *testing.T) {
	cfg := recallFixture(t)
	if err := cmdRecall(context.Background(), cfg, []string{"frobnicate"}); err == nil {
		t.Fatal("unknown subcommand accepted")
	}
	if err := cmdRecall(context.Background(), cfg, nil); err == nil {
		t.Fatal("missing subcommand accepted")
	}
	if err := cmdRecall(context.Background(), cfg, []string{"search"}); err == nil {
		t.Fatal("search with no query accepted")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run TestRecall 2>&1 | head -20`
Expected: FAIL — `cmdRecall` undefined.

- [ ] **Step 3: Write `cmd/spore/recall.go`**

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/recall/sqlitefts"
	"github.com/codered/spore/internal/store"
)

// cmdRecall is the operator's view of the index the model searches through
// recall_search: the same backend, unscoped, because whoever runs the binary
// is the operator.
func cmdRecall(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore recall search <query> | status | reindex")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()
	backend := sqlitefts.New(st.DB())

	switch args[0] {
	case "search":
		return recallSearchCmd(ctx, backend, args[1:])
	case "status":
		return recallStatusCmd(ctx, backend)
	case "reindex":
		return recallReindexCmd(ctx, cfg, st)
	default:
		return fmt.Errorf("unknown recall command %q: want search, status or reindex", args[0])
	}
}

func recallSearchCmd(ctx context.Context, backend recall.Recall, args []string) error {
	fs := flag.NewFlagSet("recall search", flag.ContinueOnError)
	kind := fs.String("kind", "", "restrict to one kind: message, summary or fact")
	session := fs.String("session", "", "restrict to one session id")
	k := fs.Int("k", recall.DefaultK, "maximum hits")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.Join(fs.Args(), " ")
	if strings.TrimSpace(query) == "" {
		return fmt.Errorf("usage: spore recall search [--kind K] [--session ID] [-k N] <query>")
	}
	q := recall.Query{Text: query, K: *k, SessionID: *session}
	if *kind != "" {
		q.Kinds = []string{*kind}
	}
	hits, err := backend.Search(ctx, q)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		fmt.Println("no matches")
		return nil
	}
	for _, h := range hits {
		fmt.Printf("%s\t%s\t%s\n", h.Kind, h.ID, h.CreatedAt.Format("2006-01-02"))
		body := h.Excerpt
		if h.Kind == recall.KindFact {
			body = h.Text
		}
		for _, line := range strings.Split(body, "\n") {
			fmt.Printf("    %s\n", line)
		}
		fmt.Println()
	}
	return nil
}

func recallStatusCmd(ctx context.Context, backend recall.Recall) error {
	st, err := backend.Status(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("backend: %s\n", st.Backend)
	if st.Degraded {
		fmt.Printf("degraded: %s\n", st.Reason)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KIND\tINDEXED")
	kinds := make([]string, 0, len(st.Counts))
	for k := range st.Counts {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		fmt.Fprintf(w, "%s\t%d\n", k, st.Counts[k])
	}
	return w.Flush()
}

// recallReindexCmd rebuilds both halves: messages and summaries from SQLite,
// facts from the files that own them.
func recallReindexCmd(ctx context.Context, cfg *config.Config, st *store.Store) error {
	n, err := st.ReindexAll(ctx)
	if err != nil {
		return err
	}
	dir := filepath.Join(cfg.DataDir, "memory")
	facts, errs := memory.Load(dir)
	for _, e := range errs {
		fmt.Fprintf(os.Stderr, "skipping malformed fact: %v\n", e)
	}
	for _, f := range facts {
		if err := st.IndexFact(ctx, f.Name, f.Description+"\n"+f.Body); err != nil {
			return err
		}
	}
	fmt.Printf("reindexed %d messages and summaries, %d facts\n", n, len(facts))
	return nil
}
```

- [ ] **Step 4: Register the verb**

In `cmd/spore/main.go`, add a `case "recall":` alongside the existing `mcp` and `session` cases, dispatching to `cmdRecall(ctx, cfg, args)` with whatever argument slice the neighbouring cases use. Add `recall` to the usage string. Read the surrounding cases and match their shape exactly.

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -v -run TestRecall 2>&1 | tail -30` then `make build && make vet && make test 2>&1 | tail -25`
Expected: all PASS.

- [ ] **Step 6: Smoke-test the CLI**

Using the `$TMP` fixture from Task 7 step 6:

```bash
./spore --config "$TMP/config.toml" recall reindex
./spore --config "$TMP/config.toml" recall status
./spore --config "$TMP/config.toml" recall search tabs
./spore --config "$TMP/config.toml" recall search 'what -v flag?'   # must not error
```

Expected: reindex reports one fact, status shows a `fact` count, the first search prints the fact body in full, and the punctuation-heavy query prints `no matches` rather than a syntax error.

- [ ] **Step 7: Document it**

Add a `## Memory and recall` section to `README.md`, after the existing tool/MCP material, in the README's established voice. It must cover:

- where facts live (`<data_dir>/memory/*.md`), the three required frontmatter keys, the four types, and that the file is the source of truth and may be edited or version-controlled by hand;
- that facts are inlined into every turn up to `[context] fact_budget` (default 2000 tokens) and appear as one-line descriptions past it;
- the `memory` tool being `ask` by default and denied to `remote` sessions, and why;
- `recall_search` being allowed by default, and that a `remote` session's search is confined to its own history with facts excluded;
- the three CLI verbs with a worked example of each;
- that recall is keyword-only in this release and `spore recall setup` for semantic search arrives with the Weaviate backend.

- [ ] **Step 8: Commit**

```bash
make fmtcheck && make build && make vet && make test
git add cmd/spore README.md
git commit -m "feat(cli): spore recall search, status and reindex"
```

---

## Definition of done

- [ ] `make build`, `make vet`, `make fmtcheck` and `make test` all clean at HEAD in a worktree freshly checked out from the branch tip — not in the working tree.
- [ ] A fact file placed in `<data_dir>/memory` by hand appears in the next turn's system block and is searchable via `spore recall search`.
- [ ] A malformed fact file produces a warning and costs exactly one fact.
- [ ] `spore recall search` never returns an FTS5 syntax error for any input.
- [ ] The four mutation checks (Tasks 1, 2, 3, 4, 5) were each run and each failed the test they were aimed at, and every mutation was reverted.
- [ ] Section 5 of the spec has no requirement without a task implementing it.
