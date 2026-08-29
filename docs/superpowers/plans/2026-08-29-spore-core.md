# spore Plan 1 — Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build spore's transport-free core — config, SQLite store, two model providers, call-site routing, context assembly, compaction, the agent loop, and a tracing skeleton — driven by a `spore once`/`chat`/`session` CLI.

**Architecture:** A single Go module. `internal/provider` defines the shared message vocabulary and a streaming `Provider` interface, with `anthropic` and `openaicompat` subpackages implementing it. `internal/store` persists sessions and messages in one SQLite file. `internal/agent` runs the loop as a pure event-emitting core that imports no transport: it assembles context (a pure function), asks `internal/router` which model serves the call site, streams from the provider, dispatches tool calls through a `ToolRunner` interface that Plan 2 will implement, and compacts when the context budget fills. `cmd/spore` is the first consumer of that event channel.

**Tech Stack:** Go 1.26.4, `github.com/mattn/go-sqlite3 v1.14.50` (cgo, FTS5), `github.com/BurntSushi/toml v1.6.0`, `go.opentelemetry.io/otel v1.46.0` + sdk + `otlptracehttp`, `github.com/google/go-cmp v0.7.0`. Stdlib `flag` and `net/http`; no CLI or HTTP framework.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md`

## Global Constraints

- Module path is `github.com/codered/spore`. Go directive `go 1.26`.
- **Every** `go build`, `go test`, and `go vet` invocation passes `-tags sqlite_fts5`. FTS5 is used in Plan 5; the tag is set from day one so the build never changes shape later.
- The agent core imports no transport package. `internal/agent` must not import `net/http`, `internal/daemon`, or any bridge. This is spec invariant 1 and a reviewer gate on every task.
- Every LLM call names a **call site** from the fixed set: `chat`, `compaction`, `title`, `classify`. There is no `embed` call site.
- Model references are `provider/model` strings, e.g. `anthropic/claude-opus-5`, `ollama/qwen3:8b`.
- No live network calls in tests. Providers are tested against recorded fixtures under `testdata/`; the loop is tested against a scripted fake provider.
- Data lives under `~/.spore` by default: `spore.db`, `config.toml`, `memory/`. Tests always override this to a `t.TempDir()`.
- Timestamps are stored as RFC3339 UTC strings.
- Repo-local git identity is already set (Harsh Singh / harshsingh24@gmail.com). Remote `origin` is `git@github.com:codered/spore.git`, branch `master`.
- Commit after every task. Conventional-commit prefixes (`feat:`, `test:`, `chore:`).

---

## File Structure

| File | Responsibility |
|---|---|
| `go.mod`, `go.sum` | Module definition, pinned dependencies |
| `Makefile` | `build`, `test`, `vet` — all with `-tags sqlite_fts5` |
| `internal/config/config.go` | Config struct, TOML load, `${ENV}` interpolation, defaults, validation |
| `internal/config/config_test.go` | Load/interpolation/validation tests |
| `internal/store/schema.go` | Embedded DDL and migration runner |
| `internal/store/store.go` | Open/Close, session and message CRUD, summary read/write |
| `internal/store/store_test.go` | Round-trip and ordering tests against a temp DB |
| `internal/provider/types.go` | `Message`, `Block`, `ToolSpec`, `Request`, `Event`, `Usage`, `Provider` |
| `internal/provider/registry.go` | `Registry`: `provider/model` ref → `Provider` + bare model id |
| `internal/provider/script.go` | `Script`, the scripted fake provider used by loop tests |
| `internal/provider/anthropic/anthropic.go` | Anthropic Messages API streaming adapter |
| `internal/provider/openaicompat/openaicompat.go` | OpenAI-compatible streaming adapter |
| `internal/router/router.go` | Call-site rules, first-match resolution, default fallback |
| `internal/agent/context.go` | `Assemble` — pure context assembly with budgets |
| `internal/agent/agent.go` | The loop: `Run`, tool dispatch, persistence, events |
| `internal/agent/compact.go` | Compaction trigger and summary generation |
| `internal/trace/trace.go` | OTel init/shutdown and OpenInference span helpers |
| `cmd/spore/main.go` | Subcommand dispatch |
| `cmd/spore/once.go`, `chat.go`, `session.go` | The three Plan 1 verbs |

---

### Task 1: Module scaffold and configuration

**Files:**
- Create: `go.mod`, `Makefile`, `.gitignore`, `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.Config`, `config.Load(path string) (*Config, error)`, `config.Default() *Config`, and the nested types `ProviderConfig{Kind, BaseURL, APIKey, PriceIn, PriceOut string/float64}`, `Route{When, Model string}`, `ContextConfig{MaxTokens int, CompactAt float64, KeepRecent int}`, `TraceConfig{Enabled bool, Endpoint string, SampleRate float64, Redact bool}`.

- [ ] **Step 1: Initialise the module and build files**

```bash
cd /home/code/development/spore
go mod init github.com/codered/spore
go mod edit -go=1.26
printf 'spore\n*.db\n*.db-wal\n*.db-shm\n' > .gitignore
cat > Makefile <<'MK'
TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o spore ./cmd/spore

test:
	go test $(TAGS) ./...

vet:
	go vet $(TAGS) ./...

.PHONY: build test vet
MK
go get github.com/BurntSushi/toml@v1.6.0
```

- [ ] **Step 2: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
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
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: FAIL — `undefined: Load`.

- [ ] **Step 4: Write the implementation**

Create `internal/config/config.go`:

```go
// Package config loads spore's single TOML configuration file.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

type Config struct {
	DefaultModel string                    `toml:"default_model"`
	SystemPrompt string                    `toml:"system_prompt"`
	DataDir      string                    `toml:"data_dir"`
	Providers    map[string]ProviderConfig `toml:"providers"`
	Routes       []Route                   `toml:"route"`
	Context      ContextConfig             `toml:"context"`
	Trace        TraceConfig               `toml:"trace"`
}

// ProviderConfig describes one upstream. Kind selects the adapter
// ("anthropic" or "openai"); prices are USD per million tokens and are used
// to attribute per-turn cost.
type ProviderConfig struct {
	Kind     string  `toml:"kind"`
	BaseURL  string  `toml:"base_url"`
	APIKey   string  `toml:"api_key"`
	PriceIn  float64 `toml:"price_in"`
	PriceOut float64 `toml:"price_out"`
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

func Default() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		SystemPrompt: "You are spore, a personal assistant running on the user's own machine.",
		DataDir:      filepath.Join(home, ".spore"),
		Providers:    map[string]ProviderConfig{},
		Context:      ContextConfig{MaxTokens: 180_000, CompactAt: 0.75, KeepRecent: 12},
		Trace:        TraceConfig{Endpoint: "http://localhost:6006/v1/traces", SampleRate: 1.0},
	}
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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
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
	return nil
}

// DBPath is the SQLite file backing every session.
func (c *Config) DBPath() string { return filepath.Join(c.DataDir, "spore.db") }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum Makefile .gitignore internal/config
git commit -m "feat: module scaffold and TOML config with env interpolation"
```

---

### Task 2: SQLite store

**Files:**
- Create: `internal/store/schema.go`, `internal/store/store.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (the store defines its own row structs; provider block JSON is stored opaquely as `[]byte`).
- Produces:
  - `store.Open(path string) (*Store, error)`, `(*Store).Close() error`
  - `(*Store).CreateSession(ctx context.Context, title string) (string, error)` — returns a new session id
  - `(*Store).ListSessions(ctx context.Context, limit int) ([]Session, error)`
  - `(*Store).AppendMessage(ctx context.Context, m Message) (int64, error)` — assigns `Seq` itself
  - `(*Store).Messages(ctx context.Context, sessionID string) ([]Message, error)` — ordered by `seq`
  - `(*Store).SetSummary(ctx context.Context, sessionID, summary string, throughSeq int) error`
  - `(*Store).Summary(ctx context.Context, sessionID string) (text string, throughSeq int, err error)`
  - Types `Session{ID, Title string, CreatedAt, UpdatedAt time.Time}` and `Message{ID int64, SessionID string, Seq int, Role string, BlocksJSON []byte, Model, CallSite string, TokensIn, TokensOut int, CostUSD float64, CreatedAt time.Time}`

- [ ] **Step 1: Add the driver**

```bash
go get github.com/mattn/go-sqlite3@v1.14.50
```

- [ ] **Step 2: Write the failing test**

Create `internal/store/store_test.go`:

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAppendAndReadMessagesInOrder(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id, err := s.CreateSession(ctx, "first session")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	for _, role := range []string{"user", "assistant", "user"} {
		if _, err := s.AppendMessage(ctx, Message{
			SessionID:  id,
			Role:       role,
			BlocksJSON: []byte(`[{"type":"text","text":"hi"}]`),
			Model:      "anthropic/claude-opus-5",
			CallSite:   "chat",
			TokensIn:   10,
			TokensOut:  4,
			CostUSD:    0.001,
		}); err != nil {
			t.Fatalf("AppendMessage: %v", err)
		}
	}

	msgs, err := s.Messages(ctx, id)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != i+1 {
			t.Errorf("message %d has Seq %d, want %d", i, m.Seq, i+1)
		}
	}
	if msgs[1].Role != "assistant" || msgs[1].TokensIn != 10 {
		t.Errorf("round-trip lost fields: %+v", msgs[1])
	}
}

func TestSummaryRoundTripAndSessionListing(t *testing.T) {
	ctx := context.Background()
	s := open(t)

	id, err := s.CreateSession(ctx, "summarised")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.SetSummary(ctx, id, "the user asked about spore", 8); err != nil {
		t.Fatalf("SetSummary: %v", err)
	}
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if text != "the user asked about spore" || through != 8 {
		t.Errorf("Summary = (%q, %d)", text, through)
	}

	// Overwriting replaces rather than accumulating.
	if err := s.SetSummary(ctx, id, "updated", 20); err != nil {
		t.Fatalf("SetSummary (update): %v", err)
	}
	text, through, _ = s.Summary(ctx, id)
	if text != "updated" || through != 20 {
		t.Errorf("after update Summary = (%q, %d)", text, through)
	}

	sessions, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != id {
		t.Errorf("ListSessions = %+v", sessions)
	}
}

func TestSummaryAbsentIsNotAnError(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	id, _ := s.CreateSession(ctx, "empty")
	text, through, err := s.Summary(ctx, id)
	if err != nil {
		t.Fatalf("Summary on session with no summary: %v", err)
	}
	if text != "" || through != 0 {
		t.Errorf("want zero values, got (%q, %d)", text, through)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v`
Expected: FAIL — `undefined: Open`.

- [ ] **Step 4: Write the schema**

Create `internal/store/schema.go`:

```go
package store

// schemaSQL is applied inside one transaction at Open. Every statement is
// idempotent, so Open doubles as the migration path for v1.
const schemaSQL = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL,
  role        TEXT NOT NULL,
  blocks      TEXT NOT NULL,
  model       TEXT NOT NULL DEFAULT '',
  call_site   TEXT NOT NULL DEFAULT '',
  tokens_in   INTEGER NOT NULL DEFAULT 0,
  tokens_out  INTEGER NOT NULL DEFAULT 0,
  cost_usd    REAL NOT NULL DEFAULT 0,
  created_at  TEXT NOT NULL,
  UNIQUE (session_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);

CREATE TABLE IF NOT EXISTS summaries (
  session_id  TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  text        TEXT NOT NULL,
  through_seq INTEGER NOT NULL,
  created_at  TEXT NOT NULL
);

-- Populated in Plan 2 (policy) and Plan 3 (scheduler); created now so the
-- schema has one definition point.
CREATE TABLE IF NOT EXISTS approvals (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool       TEXT NOT NULL,
  args       TEXT NOT NULL,
  decision   TEXT NOT NULL,
  scope      TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  schedule   TEXT NOT NULL,
  prompt     TEXT NOT NULL,
  session_id TEXT,
  enabled    INTEGER NOT NULL DEFAULT 1,
  last_run   TEXT,
  created_at TEXT NOT NULL
);
`
```

- [ ] **Step 5: Write the store**

Create `internal/store/store.go`:

```go
// Package store persists sessions, messages and summaries in one SQLite file.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct{ db *sql.DB }

type Session struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

type Message struct {
	ID         int64
	SessionID  string
	Seq        int
	Role       string
	BlocksJSON []byte
	Model      string
	CallSite   string
	TokensIn   int
	TokensOut  int
	CostUSD    float64
	CreatedAt  time.Time
}

const timeFormat = time.RFC3339Nano

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One writer keeps WAL contention out of the picture; the daemon is a
	// single process and writes are short.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func newID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Store) CreateSession(ctx context.Context, title string) (string, error) {
	id := newID()
	now := time.Now().UTC().Format(timeFormat)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		id, title, now, now)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

func (s *Store) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, created_at, updated_at FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.Title, &created, &updated); err != nil {
			return nil, err
		}
		sess.CreatedAt, _ = time.Parse(timeFormat, created)
		sess.UpdatedAt, _ = time.Parse(timeFormat, updated)
		out = append(out, sess)
	}
	return out, rows.Err()
}

// AppendMessage assigns the next seq for the session and writes the row.
func (s *Store) AppendMessage(ctx context.Context, m Message) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE session_id = ?`, m.SessionID).Scan(&next); err != nil {
		return 0, fmt.Errorf("next seq: %w", err)
	}
	now := time.Now().UTC().Format(timeFormat)
	res, err := tx.ExecContext(ctx,
		`INSERT INTO messages (session_id, seq, role, blocks, model, call_site, tokens_in, tokens_out, cost_usd, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.SessionID, next, m.Role, string(m.BlocksJSON), m.Model, m.CallSite,
		m.TokensIn, m.TokensOut, m.CostUSD, now)
	if err != nil {
		return 0, fmt.Errorf("append message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, now, m.SessionID); err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) Messages(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, session_id, seq, role, blocks, model, call_site, tokens_in, tokens_out, cost_usd, created_at
		 FROM messages WHERE session_id = ? ORDER BY seq`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var blocks, created string
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Seq, &m.Role, &blocks, &m.Model,
			&m.CallSite, &m.TokensIn, &m.TokensOut, &m.CostUSD, &created); err != nil {
			return nil, err
		}
		m.BlocksJSON = []byte(blocks)
		m.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetSummary(ctx context.Context, sessionID, summary string, throughSeq int) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO summaries (session_id, text, through_seq, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(session_id) DO UPDATE SET text = excluded.text, through_seq = excluded.through_seq, created_at = excluded.created_at`,
		sessionID, summary, throughSeq, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("set summary: %w", err)
	}
	return nil
}

// Summary returns ("", 0, nil) when the session has never been compacted.
func (s *Store) Summary(ctx context.Context, sessionID string) (string, int, error) {
	var text string
	var through int
	err := s.db.QueryRowContext(ctx,
		`SELECT text, through_seq FROM summaries WHERE session_id = ?`, sessionID).Scan(&text, &through)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("read summary: %w", err)
	}
	return text, through, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v`
Expected: PASS (3 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/store go.mod go.sum
git commit -m "feat: SQLite store for sessions, messages and summaries"
```

---

### Task 3: Provider vocabulary, registry, and scripted fake

**Files:**
- Create: `internal/provider/types.go`, `internal/provider/registry.go`, `internal/provider/script.go`, `internal/provider/script_test.go`

**Interfaces:**
- Consumes: `config.ProviderConfig` (Task 1).
- Produces:
  - `provider.Role` constants `RoleUser`, `RoleAssistant`, `RoleTool`
  - `provider.Block{Type, Text, ID, Name string, Input json.RawMessage, Content string, IsError, Truncated bool}` with type constants `BlockText`, `BlockToolUse`, `BlockToolResult`
  - `provider.Message{Role Role, Blocks []Block}`
  - `provider.ToolSpec{Name, Description string, Schema json.RawMessage}`
  - `provider.Request{Model, System string, Messages []Message, Tools []ToolSpec, MaxTokens int, Temperature float64}`
  - `provider.Usage{InputTokens, OutputTokens int}`
  - `provider.Event{Type EventType, Text string, Block *Block, Usage *Usage, Err error}` with `EventTextDelta`, `EventToolCall`, `EventDone`, `EventError`
  - `provider.Provider` interface: `Name() string`, `Stream(ctx, Request) (<-chan Event, error)`
  - `provider.Registry` with `Register(name string, p Provider, price ProviderPrice)`, `Resolve(ref string) (Provider, string, ProviderPrice, error)`
  - `provider.ProviderPrice{In, Out float64}` and `(ProviderPrice).Cost(u Usage) float64`
  - `provider.NewScript(turns ...ScriptTurn) *Script` and `ScriptTurn{Text string, ToolCalls []Block, Usage Usage, Err error}`

- [ ] **Step 1: Write the failing test**

Create `internal/provider/script_test.go`:

```go
package provider

import (
	"context"
	"encoding/json"
	"testing"
)

func drain(t *testing.T, ch <-chan Event) (text string, calls []Block, usage Usage) {
	t.Helper()
	for ev := range ch {
		switch ev.Type {
		case EventTextDelta:
			text += ev.Text
		case EventToolCall:
			calls = append(calls, *ev.Block)
		case EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case EventError:
			t.Fatalf("unexpected error event: %v", ev.Err)
		}
	}
	return
}

func TestScriptReplaysTurnsInOrder(t *testing.T) {
	s := NewScript(
		ScriptTurn{
			ToolCalls: []Block{{Type: BlockToolUse, ID: "c1", Name: "fs.read", Input: json.RawMessage(`{"path":"go.mod"}`)}},
			Usage:     Usage{InputTokens: 100, OutputTokens: 20},
		},
		ScriptTurn{Text: "module github.com/codered/spore", Usage: Usage{InputTokens: 150, OutputTokens: 8}},
	)
	ctx := context.Background()

	ch, err := s.Stream(ctx, Request{Model: "fake"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_, calls, usage := drain(t, ch)
	if len(calls) != 1 || calls[0].Name != "fs.read" {
		t.Fatalf("first turn calls = %+v", calls)
	}
	if usage.InputTokens != 100 {
		t.Errorf("usage = %+v", usage)
	}

	ch, _ = s.Stream(ctx, Request{Model: "fake"})
	text, calls, _ := drain(t, ch)
	if text != "module github.com/codered/spore" || len(calls) != 0 {
		t.Errorf("second turn text = %q, calls = %+v", text, calls)
	}
}

func TestScriptRecordsRequests(t *testing.T) {
	s := NewScript(ScriptTurn{Text: "ok"})
	ch, _ := s.Stream(context.Background(), Request{Model: "fake", System: "sys"})
	drain(t, ch)
	if got := s.Requests(); len(got) != 1 || got[0].System != "sys" {
		t.Fatalf("Requests() = %+v", got)
	}
}

func TestRegistryResolvesRefAndCost(t *testing.T) {
	r := NewRegistry()
	s := NewScript()
	r.Register("anthropic", s, ProviderPrice{In: 5, Out: 25})

	p, model, price, err := r.Resolve("anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p != Provider(s) || model != "claude-opus-5" {
		t.Errorf("Resolve gave (%v, %q)", p, model)
	}
	// 1M in at $5 + 1M out at $25.
	if got := price.Cost(Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}); got != 30 {
		t.Errorf("Cost = %v, want 30", got)
	}
	if _, _, _, err := r.Resolve("nope/model"); err == nil {
		t.Error("Resolve accepted an unregistered provider")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/provider/ -v`
Expected: FAIL — `undefined: NewScript`.

- [ ] **Step 3: Write the shared types**

Create `internal/provider/types.go`:

```go
// Package provider defines spore's model vocabulary and the streaming
// interface every adapter implements. It imports no transport and no store.
package provider

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

const (
	BlockText       = "text"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// Block is one piece of a message. Which fields are meaningful depends on
// Type: text uses Text; tool_use uses ID, Name and Input; tool_result uses ID,
// Content, IsError and Truncated.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}

type Message struct {
	Role   Role    `json:"role"`
	Blocks []Block `json:"blocks"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature float64
}

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type EventType string

const (
	EventTextDelta EventType = "text_delta"
	EventToolCall  EventType = "tool_call"
	EventDone      EventType = "done"
	EventError     EventType = "error"
)

type Event struct {
	Type  EventType
	Text  string
	Block *Block
	Usage *Usage
	Err   error
}

// Provider streams one assistant response. Implementations must close the
// returned channel exactly once, and must emit either EventDone or
// EventError as their final event.
type Provider interface {
	Name() string
	Stream(ctx context.Context, req Request) (<-chan Event, error)
}
```

- [ ] **Step 4: Write the registry**

Create `internal/provider/registry.go`:

```go
package provider

import (
	"fmt"
	"strings"
	"sync"
)

// ProviderPrice is USD per million tokens.
type ProviderPrice struct{ In, Out float64 }

func (p ProviderPrice) Cost(u Usage) float64 {
	return float64(u.InputTokens)/1e6*p.In + float64(u.OutputTokens)/1e6*p.Out
}

type entry struct {
	p     Provider
	price ProviderPrice
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

func (r *Registry) Register(name string, p Provider, price ProviderPrice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = entry{p: p, price: price}
}

// Resolve splits a "provider/model" ref and returns the registered provider,
// the bare model id to send upstream, and its pricing.
func (r *Registry) Resolve(ref string) (Provider, string, ProviderPrice, error) {
	name, model, ok := strings.Cut(ref, "/")
	if !ok || name == "" || model == "" {
		return nil, "", ProviderPrice{}, fmt.Errorf("model ref %q must be of the form provider/model", ref)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return nil, "", ProviderPrice{}, fmt.Errorf("no provider %q configured (ref %q)", name, ref)
	}
	return e.p, model, e.price, nil
}
```

- [ ] **Step 5: Write the scripted fake provider**

Create `internal/provider/script.go`:

```go
package provider

import (
	"context"
	"fmt"
	"sync"
)

// ScriptTurn is one canned assistant response.
type ScriptTurn struct {
	Text      string
	ToolCalls []Block
	Usage     Usage
	Err       error
}

// Script is a Provider that replays canned turns in order. It is the test
// double behind every agent-loop test, so the loop can be exercised with no
// network and byte-exact expectations.
type Script struct {
	mu       sync.Mutex
	turns    []ScriptTurn
	next     int
	requests []Request
}

func NewScript(turns ...ScriptTurn) *Script { return &Script{turns: turns} }

func (s *Script) Name() string { return "script" }

// Requests returns every request the loop has sent, in order.
func (s *Script) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

func (s *Script) Stream(ctx context.Context, req Request) (<-chan Event, error) {
	s.mu.Lock()
	if s.next >= len(s.turns) {
		s.mu.Unlock()
		return nil, fmt.Errorf("script exhausted after %d turns", len(s.turns))
	}
	turn := s.turns[s.next]
	s.next++
	s.requests = append(s.requests, req)
	s.mu.Unlock()

	ch := make(chan Event, len(turn.ToolCalls)+2)
	go func() {
		defer close(ch)
		if turn.Err != nil {
			ch <- Event{Type: EventError, Err: turn.Err}
			return
		}
		if turn.Text != "" {
			ch <- Event{Type: EventTextDelta, Text: turn.Text}
		}
		for i := range turn.ToolCalls {
			b := turn.ToolCalls[i]
			ch <- Event{Type: EventToolCall, Block: &b}
		}
		u := turn.Usage
		ch <- Event{Type: EventDone, Usage: &u}
	}()
	return ch, nil
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/provider/ -v`
Expected: PASS (4 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/provider
git commit -m "feat: provider vocabulary, registry and scripted fake provider"
```

---

### Task 4: Anthropic provider

**Files:**
- Create: `internal/provider/anthropic/anthropic.go`, `internal/provider/anthropic/anthropic_test.go`, `internal/provider/anthropic/testdata/tool_use.sse`

**Interfaces:**
- Consumes: `provider.Request`, `provider.Event`, `provider.Block`, `provider.Usage` (Task 3).
- Produces: `anthropic.New(baseURL, apiKey string, hc *http.Client) *Client` implementing `provider.Provider`. A zero `baseURL` defaults to `https://api.anthropic.com`; a nil `hc` defaults to a 10-minute-timeout client.

- [ ] **Step 1: Record the fixture**

Create `internal/provider/anthropic/testdata/tool_use.sse` — an abbreviated but structurally faithful Messages API stream containing text, a tool call whose JSON arrives in fragments, and usage:

```
event: message_start
data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","usage":{"input_tokens":112,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the file."}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"fs.read","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"go.mod\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":37}}

event: message_stop
data: {"type":"message_stop"}
```

- [ ] **Step 2: Write the failing test**

Create `internal/provider/anthropic/anthropic_test.go`:

```go
package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func TestStreamParsesTextToolCallAndUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_use.sse")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-test" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header missing")
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "claude-opus-5",
		System:    "you are spore",
		MaxTokens: 1024,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "what module is this?"}},
		}},
		Tools: []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	var calls []provider.Block
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventToolCall:
			calls = append(calls, *ev.Block)
		case provider.EventDone:
			usage = *ev.Usage
		case provider.EventError:
			t.Fatalf("error event: %v", ev.Err)
		}
	}

	if text != "Checking the file." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "fs.read" || string(calls[0].Input) != `{"path":"go.mod"}` {
		t.Errorf("tool call = %+v (input %s)", calls[0], calls[0].Input)
	}
	if usage.InputTokens != 112 || usage.OutputTokens != 37 {
		t.Errorf("usage = %+v", usage)
	}
	if gotBody["system"] != "you are spore" || gotBody["stream"] != true {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestStreamSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	_, err := c.Stream(context.Background(), provider.Request{Model: "nope", MaxTokens: 16})
	if err == nil {
		t.Fatal("Stream succeeded on a 400; want error")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/provider/anthropic/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Write the implementation**

Create `internal/provider/anthropic/anthropic.go`:

```go
// Package anthropic adapts the Anthropic Messages API to provider.Provider.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
)

const apiVersion = "2023-06-01"

type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, hc: hc}
}

func (c *Client) Name() string { return "anthropic" }

type wireBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content string          `json:"content,omitempty"`
	IsError bool            `json:"is_error,omitempty"`

	// tool_result references the originating tool_use id.
	ToolUseID string `json:"tool_use_id,omitempty"`
}

func toWire(msgs []provider.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := string(m.Role)
		blocks := make([]wireBlock, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockToolResult:
				// Anthropic carries tool results on a user-role message.
				role = "user"
				blocks = append(blocks, wireBlock{Type: "tool_result", ToolUseID: b.ID, Content: b.Content, IsError: b.IsError})
			case provider.BlockToolUse:
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
			default:
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	return out
}

func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     true,
		"messages":   toWire(req.Messages),
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "input_schema": t.Schema,
			})
		}
		body["tools"] = tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Event, 32)
	go c.parse(resp.Body, ch)
	return ch, nil
}

// parse turns the SSE stream into events. Tool-call JSON arrives in
// fragments, so partial input is accumulated per content-block index and
// emitted only at content_block_stop.
func (c *Client) parse(rc io.ReadCloser, ch chan<- provider.Event) {
	defer close(ch)
	defer rc.Close()

	type pending struct {
		id, name string
		input    strings.Builder
	}
	tools := map[int]*pending{}
	var usage provider.Usage

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct{ InputTokens, OutputTokens int } `json:"usage"`
			} `json:"message"`
			ContentBlock wireBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage struct{ InputTokens, OutputTokens int } `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("decode sse: %w", err)}
			return
		}

		switch ev.Type {
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				tools[ev.Index] = &pending{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				ch <- provider.Event{Type: provider.EventTextDelta, Text: ev.Delta.Text}
			case "input_json_delta":
				if p := tools[ev.Index]; p != nil {
					p.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if p := tools[ev.Index]; p != nil {
				input := p.input.String()
				if input == "" {
					input = "{}"
				}
				ch <- provider.Event{Type: provider.EventToolCall, Block: &provider.Block{
					Type: provider.BlockToolUse, ID: p.id, Name: p.name, Input: json.RawMessage(input),
				}}
				delete(tools, ev.Index)
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("anthropic stream error")}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}
	u := usage
	ch <- provider.Event{Type: provider.EventDone, Usage: &u}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/provider/anthropic/ -v`
Expected: PASS (2 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/provider/anthropic
git commit -m "feat: Anthropic Messages API streaming provider"
```

---

### Task 5: OpenAI-compatible provider

**Files:**
- Create: `internal/provider/openaicompat/openaicompat.go`, `internal/provider/openaicompat/openaicompat_test.go`, `internal/provider/openaicompat/testdata/tool_call.sse`

**Interfaces:**
- Consumes: `provider.*` (Task 3).
- Produces: `openaicompat.New(baseURL, apiKey string, hc *http.Client) *Client` implementing `provider.Provider`. `baseURL` is required (there is no sensible default across OpenAI, Ollama, vLLM); the client posts to `baseURL + "/chat/completions"`.

- [ ] **Step 1: Record the fixture**

Create `internal/provider/openaicompat/testdata/tool_call.sse`:

```
data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Looking "}}]}

data: {"choices":[{"index":0,"delta":{"content":"it up."}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{\"path\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"go.mod\"}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":88,"completion_tokens":25}}

data: [DONE]
```

- [ ] **Step 2: Write the failing test**

Create `internal/provider/openaicompat/openaicompat_test.go`:

```go
package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func TestStreamParsesFragmentedToolCallAndUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "qwen3:8b",
		System:    "you are spore",
		MaxTokens: 512,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "what module is this?"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	var calls []provider.Block
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventToolCall:
			calls = append(calls, *ev.Block)
		case provider.EventDone:
			usage = *ev.Usage
		case provider.EventError:
			t.Fatalf("error event: %v", ev.Err)
		}
	}

	if text != "Looking it up." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || string(calls[0].Input) != `{"path":"go.mod"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if usage.InputTokens != 88 || usage.OutputTokens != 25 {
		t.Errorf("usage = %+v", usage)
	}
	// The system prompt must travel as the first message, not a top-level field.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in request body")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are spore" {
		t.Errorf("first message = %+v", first)
	}
}

func TestStreamSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", srv.Client()).Stream(context.Background(), provider.Request{Model: "m"}); err == nil {
		t.Fatal("Stream succeeded on a 401; want error")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/provider/openaicompat/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 4: Write the implementation**

Create `internal/provider/openaicompat/openaicompat.go`:

```go
// Package openaicompat adapts any OpenAI-compatible chat-completions endpoint
// (OpenAI, DeepSeek, Groq, OpenRouter, vLLM, Ollama) to provider.Provider.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
)

type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, hc: hc}
}

func (c *Client) Name() string { return "openaicompat" }

// toWire flattens spore messages into OpenAI's shape: assistant tool calls
// become `tool_calls`, and each tool result becomes its own `tool` message.
func toWire(system string, msgs []provider.Message) []map[string]any {
	out := []map[string]any{}
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		var text strings.Builder
		var calls []map[string]any
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				text.WriteString(b.Text)
			case provider.BlockToolUse:
				calls = append(calls, map[string]any{
					"id": b.ID, "type": "function",
					"function": map[string]any{"name": b.Name, "arguments": string(b.Input)},
				})
			case provider.BlockToolResult:
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": b.ID, "content": b.Content,
				})
			}
		}
		if text.Len() == 0 && len(calls) == 0 {
			continue
		}
		msg := map[string]any{"role": string(m.Role), "content": text.String()}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		out = append(out, msg)
	}
	return out
}

func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	body := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       toWire(req.System, req.Messages),
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.Schema,
				},
			})
		}
		body["tools"] = tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai-compatible %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Event, 32)
	go parse(resp.Body, ch)
	return ch, nil
}

func parse(rc io.ReadCloser, ch chan<- provider.Event) {
	defer close(ch)
	defer rc.Close()

	type pending struct{ id, name string; args strings.Builder }
	calls := map[int]*pending{}
	var usage provider.Usage

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("decode chunk: %w", err)}
			return
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				ch <- provider.Event{Type: provider.EventTextDelta, Text: choice.Delta.Content}
			}
			for _, tc := range choice.Delta.ToolCalls {
				p := calls[tc.Index]
				if p == nil {
					p = &pending{}
					calls[tc.Index] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}

	// Emit accumulated calls in index order so replays are deterministic.
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		p := calls[i]
		args := p.args.String()
		if args == "" {
			args = "{}"
		}
		ch <- provider.Event{Type: provider.EventToolCall, Block: &provider.Block{
			Type: provider.BlockToolUse, ID: p.id, Name: p.name, Input: json.RawMessage(args),
		}}
	}
	u := usage
	ch <- provider.Event{Type: provider.EventDone, Usage: &u}
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/provider/openaicompat/ -v`
Expected: PASS (2 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/provider/openaicompat
git commit -m "feat: OpenAI-compatible streaming provider"
```

---

### Task 6: Call-site router

**Files:**
- Create: `internal/router/router.go`, `internal/router/router_test.go`

**Interfaces:**
- Consumes: `config.Route` (Task 1).
- Produces: `router.New(routes []config.Route, defaultModel string) (*Router, error)` and `(*Router).Model(callSite string) string`. Also the call-site constants `router.SiteChat`, `SiteCompaction`, `SiteTitle`, `SiteClassify` and `router.ValidSite(s string) bool`.

- [ ] **Step 1: Write the failing test**

Create `internal/router/router_test.go`:

```go
package router

import (
	"testing"

	"github.com/codered/spore/internal/config"
)

func TestFirstMatchWinsAndDefaultApplies(t *testing.T) {
	r, err := New([]config.Route{
		{When: "compaction|title|classify", Model: "ollama/qwen3:8b"},
		{When: "chat", Model: "anthropic/claude-opus-5"},
	}, "anthropic/claude-sonnet-5")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := map[string]string{
		SiteCompaction: "ollama/qwen3:8b",
		SiteTitle:      "ollama/qwen3:8b",
		SiteClassify:   "ollama/qwen3:8b",
		SiteChat:       "anthropic/claude-opus-5",
		"unmatched":    "anthropic/claude-sonnet-5",
	}
	for site, want := range cases {
		if got := r.Model(site); got != want {
			t.Errorf("Model(%q) = %q, want %q", site, got, want)
		}
	}
}

func TestPatternsAreAnchored(t *testing.T) {
	r, err := New([]config.Route{{When: "chat", Model: "ollama/qwen3:8b"}}, "anthropic/claude-opus-5")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// "chatty" must not match a rule written for "chat".
	if got := r.Model("chatty"); got != "anthropic/claude-opus-5" {
		t.Errorf("Model(\"chatty\") = %q, want the default", got)
	}
}

func TestNewRejectsBadPattern(t *testing.T) {
	if _, err := New([]config.Route{{When: "(unclosed", Model: "ollama/q"}}, "anthropic/m"); err == nil {
		t.Fatal("New accepted an invalid regexp")
	}
}

func TestValidSite(t *testing.T) {
	if !ValidSite(SiteChat) || ValidSite("embed") {
		t.Error("ValidSite is wrong: chat must be valid, embed must not")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/router/ -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/router/router.go`:

```go
// Package router picks the model ref for a call site from ordered config rules.
package router

import (
	"fmt"
	"regexp"

	"github.com/codered/spore/internal/config"
)

// The fixed set of call sites. Every LLM call in spore names one of these;
// there is deliberately no "embed" site — embeddings are computed by the
// recall backend, not routed here.
const (
	SiteChat       = "chat"
	SiteCompaction = "compaction"
	SiteTitle      = "title"
	SiteClassify   = "classify"
)

func ValidSite(s string) bool {
	switch s {
	case SiteChat, SiteCompaction, SiteTitle, SiteClassify:
		return true
	}
	return false
}

type rule struct {
	re    *regexp.Regexp
	model string
}

type Router struct {
	rules        []rule
	defaultModel string
}

// New compiles each rule's When as an anchored regexp, so "chat" matches the
// call site "chat" but not "chatty".
func New(routes []config.Route, defaultModel string) (*Router, error) {
	r := &Router{defaultModel: defaultModel}
	for i, rt := range routes {
		re, err := regexp.Compile(`\A(?:` + rt.When + `)\z`)
		if err != nil {
			return nil, fmt.Errorf("route %d: invalid pattern %q: %w", i, rt.When, err)
		}
		r.rules = append(r.rules, rule{re: re, model: rt.Model})
	}
	return r, nil
}

// Model returns the model ref for a call site: first matching rule, else the
// configured default.
func (r *Router) Model(callSite string) string {
	for _, rule := range r.rules {
		if rule.re.MatchString(callSite) {
			return rule.model
		}
	}
	return r.defaultModel
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/router/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/router
git commit -m "feat: call-site model router"
```

---

### Task 7: Context assembly

**Files:**
- Create: `internal/agent/context.go`, `internal/agent/context_test.go`

**Interfaces:**
- Consumes: `provider.Message`, `provider.Block` (Task 3), `config.ContextConfig` (Task 1).
- Produces:
  - `agent.Snapshot{System string, Facts []string, Summary string, Messages []provider.Message}`
  - `agent.Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request` — a pure function
  - `agent.EstimateTokens(s string) int`
  - `agent.SnapshotTokens(snap Snapshot) int`

- [ ] **Step 1: Write the failing test**

Create `internal/agent/context_test.go`:

```go
package agent

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

func userMsg(text string) provider.Message {
	return provider.Message{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: text}}}
}

func TestAssembleOrdersSystemFactsSummaryThenTail(t *testing.T) {
	snap := Snapshot{
		System:   "you are spore",
		Facts:    []string{"the user prefers Go", "the user is in London"},
		Summary:  "earlier: the user set up spore",
		Messages: []provider.Message{userMsg("first"), userMsg("second")},
	}
	req := Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 10})

	if !strings.HasPrefix(req.System, "you are spore") {
		t.Errorf("System does not start with the system prompt: %q", req.System)
	}
	factsAt := strings.Index(req.System, "the user prefers Go")
	summaryAt := strings.Index(req.System, "earlier: the user set up spore")
	if factsAt < 0 || summaryAt < 0 || factsAt > summaryAt {
		t.Errorf("facts must precede the summary; system = %q", req.System)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "first" {
		t.Errorf("message order changed: %+v", req.Messages)
	}
}

func TestAssembleKeepsOnlyRecentMessages(t *testing.T) {
	var msgs []provider.Message
	for _, s := range []string{"m1", "m2", "m3", "m4", "m5"} {
		msgs = append(msgs, userMsg(s))
	}
	req := Assemble(Snapshot{System: "s", Messages: msgs}, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 2})
	if len(req.Messages) != 2 {
		t.Fatalf("Messages = %d, want 2", len(req.Messages))
	}
	if req.Messages[0].Blocks[0].Text != "m4" || req.Messages[1].Blocks[0].Text != "m5" {
		t.Errorf("kept the wrong tail: %+v", req.Messages)
	}
}

func TestAssembleIsPure(t *testing.T) {
	snap := Snapshot{System: "s", Messages: []provider.Message{userMsg("a"), userMsg("b")}}
	before := len(snap.Messages)
	Assemble(snap, config.ContextConfig{MaxTokens: 1000, CompactAt: 0.75, KeepRecent: 1})
	if len(snap.Messages) != before {
		t.Errorf("Assemble mutated its input snapshot")
	}
}

func TestEstimateTokensGrowsWithLength(t *testing.T) {
	short := EstimateTokens("hello")
	long := EstimateTokens(strings.Repeat("hello ", 100))
	if short < 1 || long <= short {
		t.Errorf("EstimateTokens: short=%d long=%d", short, long)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -v`
Expected: FAIL — `undefined: Assemble`.

- [ ] **Step 3: Write the implementation**

Create `internal/agent/context.go`:

```go
package agent

import (
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
)

// Snapshot is everything context assembly is allowed to see. Taking it as a
// value is what makes Assemble a pure function and therefore testable with no
// store and no network.
type Snapshot struct {
	System   string
	Facts    []string
	Summary  string
	Messages []provider.Message
}

// EstimateTokens approximates tokens as bytes/4. It is deliberately crude:
// it only has to be monotonic and roughly right to drive the compaction
// trigger, and it costs nothing.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return len(s)/4 + 1
}

func messageTokens(m provider.Message) int {
	n := 4 // per-message overhead
	for _, b := range m.Blocks {
		n += EstimateTokens(b.Text) + EstimateTokens(string(b.Input)) + EstimateTokens(b.Content)
	}
	return n
}

// SnapshotTokens estimates the assembled size of a snapshot.
func SnapshotTokens(snap Snapshot) int {
	n := EstimateTokens(snap.System) + EstimateTokens(snap.Summary)
	for _, f := range snap.Facts {
		n += EstimateTokens(f)
	}
	for _, m := range snap.Messages {
		n += messageTokens(m)
	}
	return n
}

// Assemble builds the request in the spec's fixed order: system prompt,
// memory facts, compaction summary, then the live message tail. Facts and the
// summary ride in the system block so they stay pinned regardless of tail
// trimming.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys strings.Builder
	sys.WriteString(snap.System)
	if len(snap.Facts) > 0 {
		sys.WriteString("\n\n## What you know about the user\n")
		for _, f := range snap.Facts {
			sys.WriteString("- ")
			sys.WriteString(f)
			sys.WriteString("\n")
		}
	}
	if snap.Summary != "" {
		sys.WriteString("\n\n## Earlier in this conversation\n")
		sys.WriteString(snap.Summary)
		sys.WriteString("\n")
	}

	tail := snap.Messages
	if cfg.KeepRecent > 0 && len(tail) > cfg.KeepRecent {
		tail = tail[len(tail)-cfg.KeepRecent:]
	}
	// Copy so callers cannot alias the snapshot's backing array.
	msgs := make([]provider.Message, len(tail))
	copy(msgs, tail)

	return provider.Request{
		System:    sys.String(),
		Messages:  msgs,
		MaxTokens: 4096,
	}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "feat: pure context assembly with fact and summary ordering"
```

---

### Task 8: The agent loop

**Files:**
- Create: `internal/agent/agent.go`, `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `store.Store` (Task 2), `provider.Registry`/`Script` (Task 3), `router.Router` (Task 6), `Assemble`/`Snapshot` (Task 7), `config.Config` (Task 1).
- Produces:
  - `agent.ToolRunner` interface: `Specs() []provider.ToolSpec`, `ReadOnly(name string) bool`, `Run(ctx context.Context, call provider.Block) provider.Block`
  - `agent.Agent{Store, Registry, Router, Cfg, Tools}` and `agent.New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, tools ToolRunner) *Agent`
  - `(*Agent).Run(ctx context.Context, sessionID, input string) (<-chan Event, error)`
  - `agent.Event{Type EventType, Text string, Block *provider.Block, Model string, Usage provider.Usage, Cost float64, Err error}` with `EvText`, `EvToolCall`, `EvToolResult`, `EvTurnDone`, `EvError`
  - `(*Agent).Snapshot(ctx context.Context, sessionID string) (Snapshot, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/agent/agent_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// fakeTools answers every call with a fixed result and records what it saw.
type fakeTools struct {
	calls  []provider.Block
	result string
}

func (f *fakeTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (f *fakeTools) ReadOnly(string) bool { return true }
func (f *fakeTools) Run(_ context.Context, call provider.Block) provider.Block {
	f.calls = append(f.calls, call)
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: f.result}
}

func harness(t *testing.T, script *provider.Script, tools ToolRunner) (*Agent, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	reg := provider.NewRegistry()
	reg.Register("test", script, provider.ProviderPrice{In: 1, Out: 2})

	cfg := config.Default()
	cfg.DefaultModel = "test/model-a"
	cfg.SystemPrompt = "you are spore"

	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	return New(st, reg, rt, cfg, tools), st
}

func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for ev := range ch {
		if ev.Type == EvError {
			t.Fatalf("error event: %v", ev.Err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunSingleTurnPersistsAndReportsCost(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{
		Text:  "hello there",
		Usage: provider.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
	})
	a, st := harness(t, script, nil)

	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Run(ctx, sid, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	var text string
	var done *Event
	for i, ev := range events {
		if ev.Type == EvText {
			text += ev.Text
		}
		if ev.Type == EvTurnDone {
			done = &events[i]
		}
	}
	if text != "hello there" {
		t.Errorf("streamed text = %q", text)
	}
	if done == nil {
		t.Fatal("no EvTurnDone event")
	}
	if done.Model != "test/model-a" || done.Cost != 3 { // 1M in @ $1 + 1M out @ $2
		t.Errorf("turn done = model %q cost %v", done.Model, done.Cost)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("persisted %d messages: %+v", len(msgs), msgs)
	}
	if msgs[1].CallSite != router.SiteChat || msgs[1].TokensOut != 1_000_000 {
		t.Errorf("assistant row = %+v", msgs[1])
	}
}

func TestRunDispatchesToolsAndFeedsResultsBack(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{
			ToolCalls: []provider.Block{{
				Type: provider.BlockToolUse, ID: "c1", Name: "fs.read",
				Input: json.RawMessage(`{"path":"go.mod"}`),
			}},
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		provider.ScriptTurn{Text: "the module is spore", Usage: provider.Usage{InputTokens: 20, OutputTokens: 6}},
	)
	tools := &fakeTools{result: "module github.com/codered/spore"}
	a, st := harness(t, script, tools)

	sid, _ := st.CreateSession(ctx, "t")
	ch, err := a.Run(ctx, sid, "what module is this?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	var sawCall, sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case EvToolCall:
			sawCall = ev.Block.Name == "fs.read"
		case EvToolResult:
			sawResult = strings.Contains(ev.Block.Content, "codered/spore")
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("missing tool events: call=%v result=%v", sawCall, sawResult)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("tool invoked %d times", len(tools.calls))
	}

	// Four rows: user, assistant(tool_use), tool result, assistant(text).
	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 4 {
		t.Fatalf("persisted %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != "tool" {
		t.Errorf("row 3 role = %q, want tool", msgs[2].Role)
	}

	// The second upstream request must carry the tool result and the tool specs.
	reqs := script.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider called %d times, want 2", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "fs.read" {
		t.Errorf("tool specs not sent: %+v", reqs[0].Tools)
	}
	var foundResult bool
	for _, m := range reqs[1].Messages {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolResult && b.ID == "c1" {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Errorf("second request lacks the tool result: %+v", reqs[1].Messages)
	}
}

func TestRunStopsAtMaxIterations(t *testing.T) {
	ctx := context.Background()
	turns := make([]provider.ScriptTurn, maxIterations+2)
	for i := range turns {
		turns[i] = provider.ScriptTurn{
			ToolCalls: []provider.Block{{Type: provider.BlockToolUse, ID: "c", Name: "fs.read", Input: json.RawMessage(`{}`)}},
		}
	}
	a, st := harness(t, provider.NewScript(turns...), &fakeTools{result: "x"})
	sid, _ := st.CreateSession(ctx, "t")

	ch, _ := a.Run(ctx, sid, "loop forever")
	var sawError bool
	for ev := range ch {
		if ev.Type == EvError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("a runaway tool loop must end in an error event, not silence")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -run TestRun -v`
Expected: FAIL — `undefined: New`.

- [ ] **Step 3: Write the implementation**

Create `internal/agent/agent.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// maxIterations bounds one turn's provider round trips so a model that keeps
// calling tools cannot spin forever.
const maxIterations = 12

type EventType string

const (
	EvText       EventType = "text"
	EvToolCall   EventType = "tool_call"
	EvToolResult EventType = "tool_result"
	EvTurnDone   EventType = "turn_done"
	EvError      EventType = "error"
)

type Event struct {
	Type  EventType
	Text  string
	Block *provider.Block
	Model string
	Usage provider.Usage
	Cost  float64
	Err   error
}

// ToolRunner is the seam Plan 2 fills. The loop knows only that tools have
// specs, can declare themselves read-only, and return a tool_result block.
type ToolRunner interface {
	Specs() []provider.ToolSpec
	ReadOnly(name string) bool
	Run(ctx context.Context, call provider.Block) provider.Block
}

type Agent struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	Tools    ToolRunner
}

func New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, tools ToolRunner) *Agent {
	return &Agent{Store: st, Registry: reg, Router: rt, Cfg: cfg, Tools: tools}
}

// Snapshot reads the session's persisted state into the value context
// assembly consumes. Facts stay empty until Plan 5 adds the memory layer.
func (a *Agent) Snapshot(ctx context.Context, sessionID string) (Snapshot, error) {
	rows, err := a.Store.Messages(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	summary, through, err := a.Store.Summary(ctx, sessionID)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{System: a.Cfg.SystemPrompt, Summary: summary}
	for _, r := range rows {
		if r.Seq <= through {
			continue // folded into the summary already
		}
		var blocks []provider.Block
		if err := json.Unmarshal(r.BlocksJSON, &blocks); err != nil {
			return Snapshot{}, fmt.Errorf("decode message %d: %w", r.ID, err)
		}
		snap.Messages = append(snap.Messages, provider.Message{Role: provider.Role(r.Role), Blocks: blocks})
	}
	return snap, nil
}

func (a *Agent) appendMessage(ctx context.Context, sessionID string, role provider.Role, blocks []provider.Block, model, site string, u provider.Usage, cost float64) error {
	raw, err := json.Marshal(blocks)
	if err != nil {
		return err
	}
	_, err = a.Store.AppendMessage(ctx, store.Message{
		SessionID: sessionID, Role: string(role), BlocksJSON: raw,
		Model: model, CallSite: site, TokensIn: u.InputTokens, TokensOut: u.OutputTokens, CostUSD: cost,
	})
	return err
}

// Run executes one user turn and returns a channel of events. The channel is
// closed when the turn finishes; the caller may abandon it only by cancelling
// ctx.
func (a *Agent) Run(ctx context.Context, sessionID, input string) (<-chan Event, error) {
	if err := a.appendMessage(ctx, sessionID, provider.RoleUser,
		[]provider.Block{{Type: provider.BlockText, Text: input}}, "", "", provider.Usage{}, 0); err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		if err := a.loop(ctx, sessionID, out); err != nil {
			out <- Event{Type: EvError, Err: err}
		}
	}()
	return out, nil
}

func (a *Agent) loop(ctx context.Context, sessionID string, out chan<- Event) error {
	for i := 0; i < maxIterations; i++ {
		if err := a.MaybeCompact(ctx, sessionID); err != nil {
			return fmt.Errorf("compaction: %w", err)
		}
		snap, err := a.Snapshot(ctx, sessionID)
		if err != nil {
			return err
		}

		req := Assemble(snap, a.Cfg.Context)
		if a.Tools != nil {
			req.Tools = a.Tools.Specs()
		}
		ref := a.Router.Model(router.SiteChat)
		p, model, price, err := a.Registry.Resolve(ref)
		if err != nil {
			return err
		}
		req.Model = model

		ch, err := p.Stream(ctx, req)
		if err != nil {
			return fmt.Errorf("provider %s: %w", ref, err)
		}

		var blocks []provider.Block
		var text string
		var calls []provider.Block
		var usage provider.Usage
		for ev := range ch {
			switch ev.Type {
			case provider.EventTextDelta:
				text += ev.Text
				out <- Event{Type: EvText, Text: ev.Text}
			case provider.EventToolCall:
				calls = append(calls, *ev.Block)
				out <- Event{Type: EvToolCall, Block: ev.Block}
			case provider.EventDone:
				if ev.Usage != nil {
					usage = *ev.Usage
				}
			case provider.EventError:
				return ev.Err
			}
		}

		if text != "" {
			blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: text})
		}
		blocks = append(blocks, calls...)
		cost := price.Cost(usage)
		if err := a.appendMessage(ctx, sessionID, provider.RoleAssistant, blocks, ref, router.SiteChat, usage, cost); err != nil {
			return err
		}

		if len(calls) == 0 {
			out <- Event{Type: EvTurnDone, Model: ref, Usage: usage, Cost: cost}
			return nil
		}
		if a.Tools == nil {
			return fmt.Errorf("model called tool %q but no tools are registered", calls[0].Name)
		}

		results, err := a.runTools(ctx, calls, out)
		if err != nil {
			return err
		}
		if err := a.appendMessage(ctx, sessionID, provider.RoleTool, results, "", "", provider.Usage{}, 0); err != nil {
			return err
		}
	}
	return fmt.Errorf("turn exceeded %d provider round trips without settling", maxIterations)
}

// runTools dispatches a batch. Calls run concurrently only when every call in
// the batch is read-only; any mutating call forces strict sequential order.
func (a *Agent) runTools(ctx context.Context, calls []provider.Block, out chan<- Event) ([]provider.Block, error) {
	allReadOnly := true
	for _, c := range calls {
		if !a.Tools.ReadOnly(c.Name) {
			allReadOnly = false
			break
		}
	}

	results := make([]provider.Block, len(calls))
	if allReadOnly && len(calls) > 1 {
		done := make(chan struct{}, len(calls))
		for i := range calls {
			go func(i int) {
				results[i] = a.Tools.Run(ctx, calls[i])
				done <- struct{}{}
			}(i)
		}
		for range calls {
			<-done
		}
	} else {
		for i := range calls {
			results[i] = a.Tools.Run(ctx, calls[i])
		}
	}

	for i := range results {
		b := results[i]
		out <- Event{Type: EvToolResult, Block: &b}
	}
	return results, nil
}
```

- [ ] **Step 4: Add a temporary compaction stub so the package builds**

Create `internal/agent/compact.go` with the real signature and a no-op body; Task 9 replaces the body and adds its tests:

```go
package agent

import "context"

// MaybeCompact summarises old messages when the session outgrows its budget.
// Implemented in Task 9.
func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error { return nil }
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -v`
Expected: PASS (7 tests — 4 from Task 7, 3 here).

- [ ] **Step 6: Verify the core imports no transport**

Run: `go list -tags sqlite_fts5 -deps ./internal/agent | grep -E '^net/http$|internal/daemon|internal/bridge'`
Expected: no output. Any match violates the global constraint and must be fixed before committing.

- [ ] **Step 7: Commit**

```bash
git add internal/agent
git commit -m "feat: agent loop with streaming events and tool dispatch"
```

---

### Task 9: Compaction

**Files:**
- Modify: `internal/agent/compact.go` (replace the stub from Task 8)
- Create: `internal/agent/compact_test.go`

**Interfaces:**
- Consumes: `(*Agent).Snapshot`, `SnapshotTokens` (Tasks 7–8), `store.SetSummary` (Task 2), `router.SiteCompaction` (Task 6).
- Produces: `(*Agent).MaybeCompact(ctx context.Context, sessionID string) error` — unchanged signature, real behaviour.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/compact_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func seedMessages(t *testing.T, st *store.Store, sid string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		blocks, _ := json.Marshal([]provider.Block{{
			Type: provider.BlockText,
			Text: strings.Repeat("this is a long stretch of conversation history. ", 40),
		}})
		if _, err := st.AppendMessage(context.Background(), store.Message{
			SessionID: sid, Role: "user", BlocksJSON: blocks,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaybeCompactSummarisesAndShrinksContext(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{Text: "SUMMARY: the user rambled about spore", Usage: provider.Usage{InputTokens: 500, OutputTokens: 12}},
		provider.ScriptTurn{Text: "understood", Usage: provider.Usage{InputTokens: 40, OutputTokens: 3}},
	)
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "long")
	seedMessages(t, st, sid, 12)

	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}

	text, through, err := st.Summary(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "rambled about spore") {
		t.Errorf("summary = %q", text)
	}
	if through != 10 { // 12 messages, KeepRecent 2
		t.Errorf("through_seq = %d, want 10", through)
	}

	// The compaction call must use the compaction call site's model.
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times, want 1", len(reqs))
	}

	// The next snapshot must be smaller and carry the summary.
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 2 {
		t.Errorf("snapshot kept %d messages, want 2", len(snap.Messages))
	}
	if !strings.Contains(snap.Summary, "rambled") {
		t.Errorf("snapshot summary = %q", snap.Summary)
	}
}

func TestMaybeCompactIsANoOpUnderBudget(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript() // no turns: any call would error
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 200_000

	sid, _ := st.CreateSession(ctx, "short")
	seedMessages(t, st, sid, 2)

	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if text, _, _ := st.Summary(ctx, sid); text != "" {
		t.Errorf("compacted a session that was under budget: %q", text)
	}
}

func TestMaybeCompactPreservesOriginalMessages(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: things happened"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "long")
	seedMessages(t, st, sid, 12)
	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatal(err)
	}

	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 12 {
		t.Errorf("compaction deleted rows: %d remain, want all 12", len(msgs))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -run Compact -v`
Expected: FAIL — the stub does nothing, so `TestMaybeCompactSummarisesAndShrinksContext` reports an empty summary.

- [ ] **Step 3: Write the implementation**

Replace `internal/agent/compact.go`:

```go
package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

const compactionPrompt = `Summarise the conversation below for your own future reference.
Keep decisions, file paths, names, numbers and open questions. Drop pleasantries.
Write at most 300 words of plain prose, no preamble.`

// MaybeCompact summarises the older part of a session once its assembled size
// passes context.compact_at of context.max_tokens. Original messages are never
// deleted: only the summary boundary moves, and Snapshot skips rows at or
// below it.
func (a *Agent) MaybeCompact(ctx context.Context, sessionID string) error {
	snap, err := a.Snapshot(ctx, sessionID)
	if err != nil {
		return err
	}
	budget := int(float64(a.Cfg.Context.MaxTokens) * a.Cfg.Context.CompactAt)
	if SnapshotTokens(snap) <= budget {
		return nil
	}

	rows, err := a.Store.Messages(ctx, sessionID)
	if err != nil {
		return err
	}
	_, through, err := a.Store.Summary(ctx, sessionID)
	if err != nil {
		return err
	}

	// live rows are those not already folded into the summary; they line up
	// one-for-one with snap.Messages, which Snapshot built the same way.
	var live []store.Message
	for _, r := range rows {
		if r.Seq > through {
			live = append(live, r)
		}
	}
	if len(live) <= a.Cfg.Context.KeepRecent {
		return nil // nothing outside the protected window
	}
	foldCount := len(live) - a.Cfg.Context.KeepRecent
	cut := live[foldCount-1].Seq
	pending := snap.Messages[:foldCount]

	var transcript strings.Builder
	if snap.Summary != "" {
		transcript.WriteString("Summary so far:\n")
		transcript.WriteString(snap.Summary)
		transcript.WriteString("\n\n")
	}
	for _, m := range pending {
		transcript.WriteString(string(m.Role))
		transcript.WriteString(": ")
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				transcript.WriteString(b.Text)
			case provider.BlockToolUse:
				fmt.Fprintf(&transcript, "[called %s]", b.Name)
			case provider.BlockToolResult:
				fmt.Fprintf(&transcript, "[tool result: %d bytes]", len(b.Content))
			}
		}
		transcript.WriteString("\n")
	}

	ref := a.Router.Model(router.SiteCompaction)
	p, model, price, err := a.Registry.Resolve(ref)
	if err != nil {
		return err
	}
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    compactionPrompt,
		MaxTokens: 1024,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: transcript.String()}},
		}},
	})
	if err != nil {
		return fmt.Errorf("compaction provider %s: %w", ref, err)
	}

	var summary string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			summary += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			return ev.Err
		}
	}
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("compaction produced an empty summary")
	}
	_ = price.Cost(usage) // recorded on the span in Task 10

	return a.Store.SetSummary(ctx, sessionID, summary, cut)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/agent/ -v`
Expected: PASS (10 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/compact.go internal/agent/compact_test.go
git commit -m "feat: context compaction with a preserved message archive"
```

---

### Task 10: Tracing skeleton

**Files:**
- Create: `internal/trace/trace.go`, `internal/trace/trace_test.go`
- Modify: `internal/agent/agent.go` (wrap the turn and each provider call in spans), `internal/agent/compact.go` (span event for compaction)

**Interfaces:**
- Consumes: `config.TraceConfig` (Task 1), `provider.Usage` (Task 3).
- Produces:
  - `trace.Init(ctx context.Context, cfg config.TraceConfig) (shutdown func(context.Context) error, err error)`
  - `trace.StartTurn(ctx context.Context, sessionID, client string) (context.Context, trace.Span)`
  - `trace.StartLLM(ctx context.Context, callSite, modelRef string) (context.Context, trace.Span)`
  - `trace.EndLLM(span trace.Span, prompt, completion string, u provider.Usage, cost float64)`
  - `trace.StartTool(ctx context.Context, name string, args []byte) (context.Context, trace.Span)`
  - `trace.SetRedact(bool)` — package-level switch consulted by `EndLLM` and `StartTool`
  - `trace.Span` is an alias for `go.opentelemetry.io/otel/trace.Span`

- [ ] **Step 1: Add the dependencies**

```bash
go get go.opentelemetry.io/otel@v1.46.0 \
       go.opentelemetry.io/otel/sdk@v1.46.0 \
       go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.46.0
```

- [ ] **Step 2: Write the failing test**

Create `internal/trace/trace_test.go`:

```go
package trace

import (
	"context"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	return sr
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func TestLLMSpanCarriesOpenInferenceAttributes(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	ctx, turn := StartTurn(context.Background(), "sess-1", "cli")
	ctx, llm := StartLLM(ctx, "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "what module is this?", "spore", provider.Usage{InputTokens: 100, OutputTokens: 20}, 0.0021)
	turn.End()

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}
	var llmSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "llm" {
			llmSpan = s
		}
	}
	if llmSpan == nil {
		t.Fatal("no span named llm")
	}
	a := attrs(llmSpan.Attributes())
	if a["openinference.span.kind"] != "LLM" {
		t.Errorf("span kind = %q", a["openinference.span.kind"])
	}
	if a["llm.model_name"] != "anthropic/claude-opus-5" || a["spore.call_site"] != "chat" {
		t.Errorf("attrs = %+v", a)
	}
	if a["llm.token_count.prompt"] != "100" || a["llm.token_count.completion"] != "20" {
		t.Errorf("token attrs = %+v", a)
	}
	if a["input.value"] != "what module is this?" || a["output.value"] != "spore" {
		t.Errorf("io attrs = %+v", a)
	}
	// The llm span must be a child of the turn span.
	if !llmSpan.Parent().IsValid() {
		t.Error("llm span has no parent")
	}
}

func TestRedactDropsPromptAndCompletionButKeepsCounts(t *testing.T) {
	sr := recorder(t)
	SetRedact(true)
	t.Cleanup(func() { SetRedact(false) })

	_, llm := StartLLM(context.Background(), "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "secret prompt", "secret completion", provider.Usage{InputTokens: 7, OutputTokens: 3}, 0.1)

	a := attrs(sr.Ended()[0].Attributes())
	if _, ok := a["input.value"]; ok {
		t.Error("input.value present under redaction")
	}
	if _, ok := a["output.value"]; ok {
		t.Error("output.value present under redaction")
	}
	if a["llm.token_count.prompt"] != "7" {
		t.Errorf("token counts must survive redaction: %+v", a)
	}
}

func TestInitDisabledIsANoOpWithUsableShutdown(t *testing.T) {
	shutdown, err := Init(context.Background(), config.TraceConfig{Enabled: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/trace/ -v`
Expected: FAIL — `undefined: StartTurn`.

- [ ] **Step 4: Write the implementation**

Create `internal/trace/trace.go`:

```go
// Package trace wires spore to OpenTelemetry using OpenInference semantic
// conventions, so Phoenix renders LLM, tool and retriever spans natively.
// Attribute names live here and nowhere else.
package trace

import (
	"context"
	"sync/atomic"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

type Span = oteltrace.Span

// OpenInference attribute keys.
const (
	attrSpanKind   = "openinference.span.kind"
	attrModelName  = "llm.model_name"
	attrTokensIn   = "llm.token_count.prompt"
	attrTokensOut  = "llm.token_count.completion"
	attrInput      = "input.value"
	attrOutput     = "output.value"
	attrToolName   = "tool.name"
	attrToolParams = "tool.parameters"
	attrCallSite   = "spore.call_site"
	attrCostUSD    = "spore.cost_usd"
	attrSessionID  = "session.id"
	attrClient     = "spore.client"
)

var redact atomic.Bool

func SetRedact(on bool) { redact.Store(on) }

func tracer() oteltrace.Tracer { return otel.Tracer("github.com/codered/spore") }

// Init installs the global tracer provider. When tracing is disabled it
// returns a no-op shutdown, so callers never branch on configuration.
func Init(ctx context.Context, cfg config.TraceConfig) (func(context.Context) error, error) {
	SetRedact(cfg.Redact)
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(cfg.Endpoint))
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(),
		resource.NewWithAttributes("", attribute.String("service.name", "spore")))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.TraceIDRatioBased(cfg.SampleRate)),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

func StartTurn(ctx context.Context, sessionID, client string) (context.Context, Span) {
	return tracer().Start(ctx, "turn", oteltrace.WithAttributes(
		attribute.String(attrSpanKind, "CHAIN"),
		attribute.String(attrSessionID, sessionID),
		attribute.String(attrClient, client),
	))
}

func StartLLM(ctx context.Context, callSite, modelRef string) (context.Context, Span) {
	return tracer().Start(ctx, "llm", oteltrace.WithAttributes(
		attribute.String(attrSpanKind, "LLM"),
		attribute.String(attrModelName, modelRef),
		attribute.String(attrCallSite, callSite),
	))
}

// EndLLM records usage and, unless redacting, the prompt and completion.
func EndLLM(span Span, prompt, completion string, u provider.Usage, cost float64) {
	span.SetAttributes(
		attribute.Int(attrTokensIn, u.InputTokens),
		attribute.Int(attrTokensOut, u.OutputTokens),
		attribute.Float64(attrCostUSD, cost),
	)
	if !redact.Load() {
		span.SetAttributes(
			attribute.String(attrInput, prompt),
			attribute.String(attrOutput, completion),
		)
	}
	span.End()
}

func StartTool(ctx context.Context, name string, args []byte) (context.Context, Span) {
	kv := []attribute.KeyValue{
		attribute.String(attrSpanKind, "TOOL"),
		attribute.String(attrToolName, name),
	}
	if !redact.Load() {
		kv = append(kv, attribute.String(attrToolParams, string(args)))
	}
	return tracer().Start(ctx, "tool "+name, oteltrace.WithAttributes(kv...))
}
```

- [ ] **Step 5: Instrument the loop**

In `internal/agent/agent.go`, import `sporetrace "github.com/codered/spore/internal/trace"` and:

1. In `Run`, wrap the goroutine body:

```go
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		ctx, turn := sporetrace.StartTurn(ctx, sessionID, "core")
		defer turn.End()
		if err := a.loop(ctx, sessionID, out); err != nil {
			turn.RecordError(err)
			out <- Event{Type: EvError, Err: err}
		}
	}()
```

2. In `loop`, around the provider call — start the span before `p.Stream` and end it after the event range:

```go
		llmCtx, llmSpan := sporetrace.StartLLM(ctx, router.SiteChat, ref)
		ch, err := p.Stream(llmCtx, req)
		if err != nil {
			llmSpan.RecordError(err)
			llmSpan.End()
			return fmt.Errorf("provider %s: %w", ref, err)
		}
```

and, immediately after computing `cost`:

```go
		sporetrace.EndLLM(llmSpan, req.System, text, usage, cost)
```

3. In `runTools`, wrap each dispatch:

```go
	run := func(call provider.Block) provider.Block {
		_, span := sporetrace.StartTool(ctx, call.Name, call.Input)
		defer span.End()
		return a.Tools.Run(ctx, call)
	}
```

and call `run(calls[i])` in both the concurrent and sequential branches.

4. In `internal/agent/compact.go`, replace `_ = price.Cost(usage)` with a span:

```go
	_, span := sporetrace.StartLLM(ctx, router.SiteCompaction, ref)
	sporetrace.EndLLM(span, transcript.String(), summary, usage, price.Cost(usage))
```

placing `StartLLM` immediately before `p.Stream` and `EndLLM` after the event loop, mirroring the chat path.

- [ ] **Step 6: Run the full suite**

Run: `go test -tags sqlite_fts5 ./... -v`
Expected: PASS everywhere; the agent tests still pass with tracing wired to the default no-op provider.

- [ ] **Step 7: Re-check the transport constraint**

Run: `go list -tags sqlite_fts5 -deps ./internal/agent | grep -E 'internal/daemon|internal/bridge'`
Expected: no output. (`net/http` now appears transitively via the OTLP exporter, which is why this check drops the `net/http` term from Task 8's version — the constraint is about spore's own transports.)

- [ ] **Step 8: Commit**

```bash
git add internal/trace internal/agent go.mod go.sum
git commit -m "feat: OpenTelemetry tracing with OpenInference conventions"
```

---

### Task 11: CLI — once, chat, session

**Files:**
- Create: `cmd/spore/main.go`, `cmd/spore/wire.go`, `cmd/spore/wire_test.go`, `cmd/spore/once.go`, `cmd/spore/chat.go`, `cmd/spore/session.go`, `README.md`

**Interfaces:**
- Consumes: everything from Tasks 1–10.
- Produces: the `spore` binary with subcommands `once`, `chat`, `session list`, `session show`, and the helper `buildAgent(cfg *config.Config, st *store.Store) (*agent.Agent, error)`.

- [ ] **Step 1: Write the failing test**

Create `cmd/spore/wire_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func TestBuildAgentRegistersConfiguredProviders(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := config.Default()
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{
		"anthropic": {Kind: "anthropic", APIKey: "sk-x", PriceIn: 5, PriceOut: 25},
		"ollama":    {Kind: "openai", BaseURL: "http://localhost:11434/v1"},
	}
	cfg.Routes = []config.Route{{When: "compaction|title|classify", Model: "ollama/qwen3:8b"}}

	a, err := buildAgent(cfg, st)
	if err != nil {
		t.Fatalf("buildAgent: %v", err)
	}
	if _, model, price, err := a.Registry.Resolve("anthropic/claude-opus-5"); err != nil || model != "claude-opus-5" || price.In != 5 {
		t.Errorf("anthropic not registered correctly: model=%q price=%+v err=%v", model, price, err)
	}
	if _, _, _, err := a.Registry.Resolve("ollama/qwen3:8b"); err != nil {
		t.Errorf("ollama not registered: %v", err)
	}
	if got := a.Router.Model("compaction"); got != "ollama/qwen3:8b" {
		t.Errorf("router not wired: compaction -> %q", got)
	}
}

func TestBuildAgentRejectsUnknownProviderKind(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	defer st.Close()

	cfg := config.Default()
	cfg.DefaultModel = "weird/model"
	cfg.Providers = map[string]config.ProviderConfig{"weird": {Kind: "telepathy"}}

	if _, err := buildAgent(cfg, st); err == nil {
		t.Fatal("buildAgent accepted an unknown provider kind")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -v`
Expected: FAIL — `undefined: buildAgent`.

- [ ] **Step 3: Write the wiring**

Create `cmd/spore/wire.go`:

```go
package main

import (
	"fmt"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/anthropic"
	"github.com/codered/spore/internal/provider/openaicompat"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// buildAgent turns configuration into a wired agent. Plan 1 registers no
// tools, so the agent runs text-only turns; Plan 2 passes a real ToolRunner.
func buildAgent(cfg *config.Config, st *store.Store) (*agent.Agent, error) {
	reg := provider.NewRegistry()
	for name, pc := range cfg.Providers {
		price := provider.ProviderPrice{In: pc.PriceIn, Out: pc.PriceOut}
		switch pc.Kind {
		case "anthropic":
			reg.Register(name, anthropic.New(pc.BaseURL, pc.APIKey, nil), price)
		case "openai", "openai-compatible":
			if pc.BaseURL == "" {
				return nil, fmt.Errorf("provider %q: base_url is required for kind %q", name, pc.Kind)
			}
			reg.Register(name, openaicompat.New(pc.BaseURL, pc.APIKey, nil), price)
		default:
			return nil, fmt.Errorf("provider %q: unknown kind %q (want anthropic or openai)", name, pc.Kind)
		}
	}
	rt, err := router.New(cfg.Routes, cfg.DefaultModel)
	if err != nil {
		return nil, err
	}
	return agent.New(st, reg, rt, cfg, nil), nil
}
```

- [ ] **Step 4: Write the entry point and subcommands**

Create `cmd/spore/main.go`:

```go
// Command spore is the CLI front end to the agent core.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

const usage = `spore — a personal agent

usage:
  spore once <prompt>          run one turn in a fresh session and print the reply
  spore chat [session-id]      interactive session (resumes when given an id)
  spore session list           list recent sessions
  spore session show <id>      print a session transcript

flags:
  -config <path>   config file (default ~/.spore/config.toml)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "spore:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	configPath := ""
	if len(args) >= 2 && args[0] == "-config" {
		configPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		configPath = filepath.Join(home, ".spore", "config.toml")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	shutdown, err := sporetrace.Init(ctx, cfg.Trace)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer shutdown(ctx)

	switch args[0] {
	case "once":
		if len(args) < 2 {
			return fmt.Errorf("once needs a prompt")
		}
		return cmdOnce(ctx, cfg, st, args[1])
	case "chat":
		id := ""
		if len(args) > 1 {
			id = args[1]
		}
		return cmdChat(ctx, cfg, st, id)
	case "session":
		return cmdSession(ctx, st, args[1:])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
```

Create `cmd/spore/once.go`:

```go
package main

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// stream prints one turn's events and returns the turn error, if any.
func stream(ch <-chan agent.Event) error {
	for ev := range ch {
		switch ev.Type {
		case agent.EvText:
			fmt.Print(ev.Text)
		case agent.EvToolCall:
			fmt.Printf("\n  → %s %s\n", ev.Block.Name, string(ev.Block.Input))
		case agent.EvToolResult:
			fmt.Printf("  ← %d bytes\n", len(ev.Block.Content))
		case agent.EvTurnDone:
			fmt.Printf("\n\n[%s · %d in / %d out · $%.4f]\n",
				ev.Model, ev.Usage.InputTokens, ev.Usage.OutputTokens, ev.Cost)
		case agent.EvError:
			return ev.Err
		}
	}
	return nil
}

func cmdOnce(ctx context.Context, cfg *config.Config, st *store.Store, prompt string) error {
	a, err := buildAgent(cfg, st)
	if err != nil {
		return err
	}
	sid, err := st.CreateSession(ctx, prompt)
	if err != nil {
		return err
	}
	ch, err := a.Run(ctx, sid, prompt)
	if err != nil {
		return err
	}
	return stream(ch)
}
```

Create `cmd/spore/chat.go`:

```go
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func cmdChat(ctx context.Context, cfg *config.Config, st *store.Store, sessionID string) error {
	a, err := buildAgent(cfg, st)
	if err != nil {
		return err
	}
	if sessionID == "" {
		sessionID, err = st.CreateSession(ctx, "chat")
		if err != nil {
			return err
		}
	}
	fmt.Printf("session %s — ctrl-d to exit\n", sessionID)

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ch, err := a.Run(ctx, sessionID, line)
		if err != nil {
			return err
		}
		if err := stream(ch); err != nil {
			fmt.Fprintln(os.Stderr, "turn failed:", err)
		}
	}
}
```

Create `cmd/spore/session.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func cmdSession(ctx context.Context, st *store.Store, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("session needs a subcommand: list or show")
	}
	switch args[0] {
	case "list":
		sessions, err := st.ListSessions(ctx, 50)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			fmt.Printf("%s  %s  %s\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Title)
		}
		return nil
	case "show":
		if len(args) < 2 {
			return fmt.Errorf("session show needs a session id")
		}
		msgs, err := st.Messages(ctx, args[1])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			var blocks []provider.Block
			if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
				return err
			}
			fmt.Printf("\n[%d %s]\n", m.Seq, m.Role)
			for _, b := range blocks {
				switch b.Type {
				case provider.BlockText:
					fmt.Println(b.Text)
				case provider.BlockToolUse:
					fmt.Printf("→ %s %s\n", b.Name, string(b.Input))
				case provider.BlockToolResult:
					fmt.Printf("← %s\n", b.Content)
				}
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown session subcommand %q", args[0])
	}
}
```

- [ ] **Step 5: Run the tests and build**

Run: `go test -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && make build`
Expected: all tests PASS, vet clean, `./spore` produced.

- [ ] **Step 6: Smoke-test against a real model**

Write `~/.spore/config.toml`:

```toml
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind      = "anthropic"
api_key   = "${ANTHROPIC_API_KEY}"
price_in  = 5.0
price_out = 25.0

[providers.ollama]
kind     = "openai"
base_url = "http://localhost:11434/v1"

[[route]]
when  = "compaction|title|classify"
model = "ollama/qwen3:8b"
```

Run: `./spore once "in one sentence, what are you?"`
Expected: a streamed reply followed by a `[model · tokens · cost]` line. Then `./spore session list` shows the session and `./spore session show <id>` prints both messages.

This step needs a real API key and is the one place the plan touches the network. If no key is available, record that the smoke test was skipped rather than marking it done.

- [ ] **Step 7: Write the README**

Create `README.md`:

```markdown
# spore

A personal AI agent in a single Go binary: your providers, your tools, your
policy. Built to run as an always-on daemon on your own machine.

Status: **Plan 1 (core)** — config, SQLite store, Anthropic and
OpenAI-compatible providers, call-site model routing, context assembly,
compaction, the agent loop, and OpenTelemetry tracing, driven by a CLI.
Tools, the daemon and web UI, MCP, Telegram, and Weaviate recall land in
Plans 2–5.

## Build

Every build and test needs the FTS5 tag:

    make build    # go build -tags sqlite_fts5 -o spore ./cmd/spore
    make test
    make vet

## Configure

spore reads `~/.spore/config.toml` and keeps everything else in
`~/.spore/spore.db`. Secrets are interpolated from the environment with
`${VAR}` and never stored in the file.

    default_model = "anthropic/claude-opus-5"

    [providers.anthropic]
    kind      = "anthropic"
    api_key   = "${ANTHROPIC_API_KEY}"
    price_in  = 5.0
    price_out = 25.0

    [providers.ollama]
    kind     = "openai"
    base_url = "http://localhost:11434/v1"

    [[route]]
    when  = "compaction|title|classify"
    model = "ollama/qwen3:8b"

Routing rules match a **call site** — `chat`, `compaction`, `title`, or
`classify` — so mechanical work runs on a cheap local model while
conversation runs on the good one.

## Use

    spore once "what is this repo?"
    spore chat
    spore session list
    spore session show <id>

## Design

`docs/superpowers/specs/2026-08-29-spore-design.md`
```

- [ ] **Step 8: Commit and push**

```bash
git add cmd README.md
git commit -m "feat: spore CLI with once, chat and session commands"
git push origin master
```
