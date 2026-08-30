# spore Plan 3 — Daemon and Web UI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn spore from a CLI that owns an agent into a daemon that owns it — an HTTP + SSE API on loopback, a `go:embed`ed web UI, a cron scheduler that fires jobs into fresh sessions, and `chat`/`once` reduced to thin clients that auto-start that daemon.

**Architecture:** `internal/daemon` is the only new consumer of `agent.Run`'s event channel. It puts a **hub** between that single-consumer channel and N attached clients: one turn runs per session, its events are broadcast, and a client disconnecting neither cancels the turn nor loses it. Approvals invert through a **broker** that satisfies `policy.Approver`: `Ask` publishes an approval event and blocks on a waiter, and an HTTP resolve either hands the answer to that waiter or — when the daemon restarted and no waiter exists — falls through to the `Guard.Resolve` path Plan 2 already built. `internal/scheduler` reads the `jobs` table, computes the next fire time, and calls back into a `Runner` that opens a fresh session, so the scheduler knows nothing about the agent. The CLI keeps its terminal approver but now renders asks arriving over SSE and answers them over HTTP, so the daemon path is the only path a turn ever takes.

**Tech Stack:** Go 1.26.4, stdlib only — `net/http` with its own pattern routing (no framework, no router library), `html/template` plus vanilla JS with no build step, and a hand-written five-field cron parser. `net/http/httptest` for API tests; the existing `provider.Script` fake for end-to-end turns. **No new third-party dependencies.**

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` (sections 2, 6, 8, 10; staging item 3 in section 11)

## Global Constraints

- Module path is `github.com/codered/spore`. Go directive `go 1.26`.
- **Every** `go build`, `go test`, and `go vet` invocation passes `-tags sqlite_fts5`. Use `make build`, `make test`, `make vet`.
- **The core never imports a transport (spec invariant 1).** `internal/agent` must not import `net/http`, `internal/daemon`, `internal/scheduler`, or any bridge. `internal/daemon` may import `internal/agent`; never the reverse. A reviewer gate on every task, enforced by the seam test extended in Task 2.
- **The daemon binds loopback only and carries no authentication.** A configured `addr` that is not `127.0.0.1`, `::1` or `localhost` is a config validation error. Multi-user operation and public endpoints are non-goals (spec section 1); do not add a token, a login, or a CORS allowance for another origin.
- **Deny is checked before everything else and is absolute.** Nothing in this plan may offer a human an approval prompt for a call the engine denied. The daemon's approver is reached only on the `ask` branch of `Guard.Run`, exactly as the terminal approver is.
- **A turn's lifetime belongs to the daemon, not to the connection that started it (spec invariant 2).** No handler may pass its request context to `agent.Run`. Turns run on a context derived from the server's lifetime; a client disconnecting unsubscribes from the hub and nothing more.
- **A job fires into a fresh session (spec section 8).** The scheduler never appends to an existing session, and a missed run fires once on the next start — never backfilled N times.
- Every LLM call names a call site from the fixed set: `chat`, `compaction`, `title`, `classify`. Plan 3 adds no new call sites; scheduled turns run at `chat`.
- No live network calls in tests. HTTP handlers are tested through `httptest.NewServer`; the scheduler through an injected clock, never `time.Sleep`.
- Data lives under `~/.spore` by default. Tests always override `DataDir` to a `t.TempDir()`.
- Timestamps are stored as RFC3339 UTC strings via the existing `store.timeFormat`.
- No Node toolchain, no bundler, no npm. `web/` is hand-written HTML, CSS and JS served through `embed.FS`. No CDN `<script src>` either — the binary must work with no internet.
- Commit after every task. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `chore:`).

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | **Modify** — add `DaemonConfig`, defaults, loopback validation, `PidPath()` |
| `internal/config/config_test.go` | **Modify** — daemon defaults and the non-loopback rejection |
| `internal/store/schema.go` | **Modify** — reshape `jobs` (kind, next_run, last_session_id) |
| `internal/store/jobs.go` | **Create** — job CRUD and the one-time `jobs` migration |
| `internal/store/jobs_test.go` | **Create** — CRUD round-trip, due-job selection, migration from the Plan 2 stub |
| `internal/daemon/event.go` | **Create** — `WireEvent`, the JSON shape every client reads; `FromAgent` |
| `internal/daemon/event_test.go` | **Create** — every `agent.EventType` maps to a wire type; errors survive as text |
| `internal/daemon/hub.go` | **Create** — per-session broadcast, subscribe/unsubscribe, one-turn-at-a-time claim |
| `internal/daemon/hub_test.go` | **Create** — fan-out to two subscribers, slow subscriber, double-claim rejection |
| `internal/daemon/seam_test.go` | **Create** — the core still imports no transport |
| `internal/daemon/server.go` | **Create** — `Server`, routes, `Handler()`, `Run()` |
| `internal/daemon/sessions.go` | **Create** — session list/create/show and message POST handlers |
| `internal/daemon/stream.go` | **Create** — the SSE endpoint |
| `internal/daemon/api_test.go` | **Create** — the HTTP surface against `httptest` |
| `internal/daemon/approver.go` | **Create** — `Broker`: `policy.Approver` over SSE plus out-of-band resolve |
| `internal/daemon/approver_test.go` | **Create** — waiter delivery, no-waiter fallback, timeout, double answer |
| `internal/daemon/pidfile.go` | **Create** — write, read, liveness-check and remove the pidfile |
| `internal/daemon/pidfile_test.go` | **Create** — stale pidfile is not mistaken for a live daemon |
| `internal/daemon/e2e_test.go` | **Create** — boot the daemon with the scripted provider and drive it over HTTP |
| `internal/scheduler/cron.go` | **Create** — `Parse`: 5-field cron and `@once <RFC3339>`; `Schedule.Next` |
| `internal/scheduler/cron_test.go` | **Create** — table tests for both forms, including one-shot exhaustion |
| `internal/scheduler/scheduler.go` | **Create** — the tick loop over due jobs, `Runner` seam, missed-run rule |
| `internal/scheduler/scheduler_test.go` | **Create** — fires once when overdue, reschedules cron, disables one-shot |
| `internal/tool/schedule/schedule.go` | **Create** — `schedule_create`, `schedule_list`, `schedule_cancel` |
| `internal/tool/schedule/schedule_test.go` | **Create** — per-operation tests against a temp store |
| `web/embed.go` | **Create** — package `web`: the `embed.FS` holding the UI (go:embed cannot reach up out of `internal/daemon`) |
| `web/index.html` | **Create** — the UI shell rendered by `html/template` |
| `web/app.js` | **Create** — SSE subscription, transcript rendering, approval buttons |
| `web/style.css` | **Create** — the stylesheet |
| `internal/daemon/web.go` | **Create** — `go:embed` of `web/`, template parsing, static handler |
| `internal/daemon/web_test.go` | **Create** — the shell renders and the assets are actually embedded |
| `cmd/spore/serve.go` | **Create** — `spore serve`, `--detach`, `--status`, `--stop` |
| `cmd/spore/client.go` | **Create** — the HTTP + SSE client `chat`/`once` talk through |
| `cmd/spore/autostart.go` | **Create** — `ensureDaemon`: connect, else spawn detached and wait for health |
| `cmd/spore/autostart_test.go` | **Create** — health polling, spawn-once, failure surfaces the daemon's stderr |
| `cmd/spore/chat.go` | **Modify** — thin client over the API |
| `cmd/spore/once.go` | **Modify** — thin client over the API |
| `cmd/spore/wire.go` | **Modify** — `buildServer`, and the schedule tool registered alongside the rest |
| `cmd/spore/main.go` | **Modify** — register `serve`; route `chat`/`once` through the client |
| `README.md` | **Modify** — document `serve`, `[daemon]`, the web UI and the scheduler |

---

### Task 1: Daemon configuration and the jobs table

The scheduler and every later task read a `[daemon]` block and a reshaped `jobs` table, so both are settled first. Plan 2 shipped a `jobs` table as an unused stub (`schedule`, `prompt`, `session_id`, `enabled`, `last_run`, `created_at`); nothing has ever written a row to it, so this task replaces it with the shape the scheduler needs and includes a guarded one-time migration for databases that already have the stub.

**Files:**
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `internal/store/schema.go`
- Create: `internal/store/jobs.go`, `internal/store/jobs_test.go`

**Interfaces:**
- Consumes: `config.Config` and `store.Store` as they exist on master.
- Produces:
  - `config.DaemonConfig{Addr string, TickSeconds int}` reachable as `cfg.Daemon`
  - `(*config.Config).PidPath() string` → `<DataDir>/spore.pid`
  - `store.Job{ID int64, Kind, Spec, Prompt string, Enabled bool, NextRun, LastRun time.Time, LastSessionID string, CreatedAt time.Time}`
  - `(*store.Store).CreateJob(ctx, Job) (int64, error)`
  - `(*store.Store).ListJobs(ctx) ([]Job, error)`
  - `(*store.Store).DueJobs(ctx, now time.Time) ([]Job, error)`
  - `(*store.Store).MarkJobRun(ctx, id int64, ran time.Time, next time.Time, sessionID string) error`
  - `(*store.Store).SetJobEnabled(ctx, id int64, enabled bool) error`

- [ ] **Step 1: Write the failing config test**

Append to `internal/config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run TestDaemonDefaults -v`
Expected: FAIL — `cfg.Daemon` undefined.

- [ ] **Step 3: Add the daemon config**

In `internal/config/config.go`, add the field to `Config` after `Shell`:

```go
	Daemon    DaemonConfig              `toml:"daemon"`
```

and the type, next to `ShellConfig`:

```go
// DaemonConfig configures the HTTP + SSE server. Addr is validated to be a
// loopback address: spore serves one person on one machine, and an exposed
// endpoint is an explicit non-goal, so a wildcard bind is a config error
// rather than a documented footgun.
type DaemonConfig struct {
	Addr string `toml:"addr"`
	// TickSeconds is how often the scheduler looks for due jobs.
	TickSeconds int `toml:"tick_seconds"`
}
```

In `Default()`, inside the returned `&Config{...}`:

```go
		Daemon: DaemonConfig{Addr: "127.0.0.1:7777", TickSeconds: 30},
```

In `Load`, alongside the other zero-value fallbacks (just after the `cfg.Shell.TimeoutSeconds` block):

```go
	if cfg.Daemon.Addr == "" {
		cfg.Daemon.Addr = d.Daemon.Addr
	}
	if cfg.Daemon.TickSeconds == 0 {
		cfg.Daemon.TickSeconds = d.Daemon.TickSeconds
	}
```

In `Validate()`, before the final `return nil`:

```go
	if err := ValidateDaemonAddr(c.Daemon.Addr); err != nil {
		return err
	}
```

And the helper, plus `PidPath`, at the end of the file:

```go
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
```

Add `"net"` to the import block, and `"path/filepath"` to the test file's imports.

- [ ] **Step 4: Run the config test**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 5: Write the failing jobs test**

Create `internal/store/jobs_test.go`:

```go
package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestJobCRUDAndDueSelection(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	due, err := s.CreateJob(ctx, Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "summarise yesterday",
		Enabled: true, NextRun: base.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	later, err := s.CreateJob(ctx, Job{
		Kind: "once", Spec: "2026-12-25T09:00:00Z", Prompt: "wrap presents",
		Enabled: true, NextRun: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	off, err := s.CreateJob(ctx, Job{
		Kind: "cron", Spec: "*/5 * * * *", Prompt: "disabled one",
		Enabled: false, NextRun: base.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	jobs, err := s.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("ListJobs returned %d jobs, want 3", len(jobs))
	}

	got, err := s.DueJobs(ctx, base)
	if err != nil {
		t.Fatalf("DueJobs: %v", err)
	}
	if len(got) != 1 || got[0].ID != due {
		t.Fatalf("DueJobs = %+v, want only job %d (past next_run, enabled)", got, due)
	}
	if got[0].Prompt != "summarise yesterday" || got[0].Kind != "cron" {
		t.Errorf("due job round-tripped as %+v", got[0])
	}
	_ = later
	_ = off

	// Marking a run advances next_run and records the session the job opened,
	// so the same job is not picked up again on the next tick.
	next := base.Add(24 * time.Hour)
	if err := s.MarkJobRun(ctx, due, base, next, "sess-abc"); err != nil {
		t.Fatalf("MarkJobRun: %v", err)
	}
	if got, err = s.DueJobs(ctx, base); err != nil || len(got) != 0 {
		t.Fatalf("DueJobs after MarkJobRun = %+v (err %v), want none", got, err)
	}
	jobs, _ = s.ListJobs(ctx)
	for _, j := range jobs {
		if j.ID != due {
			continue
		}
		if !j.NextRun.Equal(next) {
			t.Errorf("next_run = %v, want %v", j.NextRun, next)
		}
		if j.LastSessionID != "sess-abc" {
			t.Errorf("last_session_id = %q, want sess-abc", j.LastSessionID)
		}
	}

	if err := s.SetJobEnabled(ctx, due, false); err != nil {
		t.Fatalf("SetJobEnabled: %v", err)
	}
	if got, err := s.DueJobs(ctx, next.Add(time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("a disabled job came back due: %+v (err %v)", got, err)
	}
}

// The Plan 2 schema shipped an unused jobs table with a different shape.
// Opening a database that still has it must produce the new shape rather
// than failing on a missing column.
func TestOpenMigratesThePlan2JobsStub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE jobs`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		schedule TEXT NOT NULL, prompt TEXT NOT NULL, session_id TEXT,
		enabled INTEGER NOT NULL DEFAULT 1, last_run TEXT, created_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("recreate stub: %v", err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen over the stub: %v", err)
	}
	defer s2.Close()
	if _, err := s2.CreateJob(context.Background(), Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "after migration",
		Enabled: true, NextRun: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateJob after migration: %v", err)
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'TestJob|TestOpenMigrates' -v`
Expected: FAIL — `Job` undefined.

- [ ] **Step 7: Reshape the schema**

In `internal/store/schema.go`, replace the whole `CREATE TABLE IF NOT EXISTS jobs (...)` statement with:

```sql
-- jobs drives the scheduler. kind is cron | once; spec is a five-field cron
-- expression or an RFC3339 instant. next_run is the computed fire time and is
-- the only column the tick loop queries on. A job always opens a FRESH
-- session, so last_session_id is a record of what it produced, never a target.
CREATE TABLE IF NOT EXISTS jobs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  kind            TEXT NOT NULL,
  spec            TEXT NOT NULL,
  prompt          TEXT NOT NULL,
  enabled         INTEGER NOT NULL DEFAULT 1,
  next_run        TEXT NOT NULL,
  last_run        TEXT,
  last_session_id TEXT NOT NULL DEFAULT '',
  created_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_jobs_due ON jobs(enabled, next_run);
```

- [ ] **Step 8: Write the store code and the migration**

Create `internal/store/jobs.go`:

```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Job is one scheduled prompt. Kind is "cron" (Spec is a five-field cron
// expression) or "once" (Spec is an RFC3339 instant). Firing opens a fresh
// session; LastSessionID records the most recent one it produced.
type Job struct {
	ID            int64
	Kind          string
	Spec          string
	Prompt        string
	Enabled       bool
	NextRun       time.Time
	LastRun       time.Time
	LastSessionID string
	CreatedAt     time.Time
}

// migrateJobs replaces the unused Plan 2 jobs stub with the scheduler's
// shape. The stub had a "schedule" column and no "kind"; nothing ever wrote
// a row to it, so dropping it loses no user data. Guarded on the column
// check so a database already on the new shape is left untouched — Open runs
// this on every start.
func migrateJobs(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return fmt.Errorf("inspect jobs table: %w", err)
	}
	defer rows.Close()
	var columns int
	hasKind := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns++
		if name == "kind" {
			hasKind = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// No table at all: the schema statement will create the right one.
	if columns == 0 || hasKind {
		return nil
	}
	if _, err := db.Exec(`DROP TABLE jobs`); err != nil {
		return fmt.Errorf("drop legacy jobs table: %w", err)
	}
	return nil
}

func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	enabled := 0
	if j.Enabled {
		enabled = 1
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (kind, spec, prompt, enabled, next_run, last_session_id, created_at)
		 VALUES (?, ?, ?, ?, ?, '', ?)`,
		j.Kind, j.Spec, j.Prompt, enabled,
		j.NextRun.UTC().Format(timeFormat), time.Now().UTC().Format(timeFormat))
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	return res.LastInsertId()
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var enabled int
		var next, created string
		var last, lastSession sql.NullString
		if err := rows.Scan(&j.ID, &j.Kind, &j.Spec, &j.Prompt, &enabled, &next, &last, &lastSession, &created); err != nil {
			return nil, err
		}
		j.Enabled = enabled != 0
		j.NextRun, _ = time.Parse(timeFormat, next)
		if last.Valid {
			j.LastRun, _ = time.Parse(timeFormat, last.String)
		}
		j.LastSessionID = lastSession.String
		j.CreatedAt, _ = time.Parse(timeFormat, created)
		out = append(out, j)
	}
	return out, rows.Err()
}

const jobColumns = `id, kind, spec, prompt, enabled, next_run, last_run, last_session_id, created_at`

func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	return scanJobs(rows)
}

// DueJobs returns enabled jobs whose next_run has passed, oldest first.
func (s *Store) DueJobs(ctx context.Context, now time.Time) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE enabled = 1 AND next_run <= ? ORDER BY next_run`,
		now.UTC().Format(timeFormat))
	if err != nil {
		return nil, fmt.Errorf("read due jobs: %w", err)
	}
	return scanJobs(rows)
}

// MarkJobRun records a firing and moves the job forward. A zero next time
// means the job has no further runs and is disabled — the one-shot case.
func (s *Store) MarkJobRun(ctx context.Context, id int64, ran, next time.Time, sessionID string) error {
	if next.IsZero() {
		_, err := s.db.ExecContext(ctx,
			`UPDATE jobs SET enabled = 0, last_run = ?, last_session_id = ? WHERE id = ?`,
			ran.UTC().Format(timeFormat), sessionID, id)
		if err != nil {
			return fmt.Errorf("retire job %d: %w", id, err)
		}
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET last_run = ?, next_run = ?, last_session_id = ? WHERE id = ?`,
		ran.UTC().Format(timeFormat), next.UTC().Format(timeFormat), sessionID, id)
	if err != nil {
		return fmt.Errorf("advance job %d: %w", id, err)
	}
	return nil
}

func (s *Store) SetJobEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET enabled = ? WHERE id = ?`, v, id)
	if err != nil {
		return fmt.Errorf("set job %d enabled: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no job %d", id)
	}
	return nil
}
```

In `internal/store/store.go`, call the migration inside `Open`, between the `sql.Open` and the `schemaSQL` exec:

```go
	if err := migrateJobs(db); err != nil {
		db.Close()
		return nil, err
	}
```

- [ ] **Step 9: Run the store tests**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v`
Expected: PASS, including the pre-existing message, summary and pending-call tests.

- [ ] **Step 10: Commit**

```bash
make vet && make test
git add internal/config internal/store
git commit -m "feat(config,store): daemon settings and the scheduler's jobs table"
```

---

### Task 2: The event hub

`agent.Run` returns a channel with exactly one consumer, but the spec requires that one turn's deltas reach every attached client and that a client disconnecting never cancels the turn. The hub is that adapter, and it is the piece every later task depends on, so it is built before any HTTP exists.

**Files:**
- Create: `internal/daemon/event.go`, `internal/daemon/event_test.go`
- Create: `internal/daemon/hub.go`, `internal/daemon/hub_test.go`
- Create: `internal/daemon/seam_test.go`

**Interfaces:**
- Consumes: `agent.Event`, `agent.EventType` constants, `provider.Block`, `provider.Usage`.
- Produces:
  - `daemon.WireEvent` (JSON shape below) and `daemon.FromAgent(agent.Event) WireEvent`
  - wire type constants `WireText`, `WireToolCall`, `WireToolResult`, `WireTurnDone`, `WireError`, `WireApproval`, `WireResolved`
  - `daemon.NewHub() *Hub`
  - `(*Hub).Subscribe(sessionID string) (<-chan WireEvent, func())`
  - `(*Hub).Publish(sessionID string, ev WireEvent)`
  - `(*Hub).Begin(sessionID string) bool` / `(*Hub).End(sessionID string)`

- [ ] **Step 1: Write the failing event test**

Create `internal/daemon/event_test.go`:

```go
package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/provider"
)

func TestFromAgentCoversEveryEventType(t *testing.T) {
	cases := []struct {
		name string
		in   agent.Event
		want string
	}{
		{"text", agent.Event{Type: agent.EvText, Text: "hello"}, WireText},
		{"tool call", agent.Event{Type: agent.EvToolCall, Block: &provider.Block{
			Type: provider.BlockToolUse, ID: "t1", Name: "fs_read", Input: json.RawMessage(`{"path":"a"}`),
		}}, WireToolCall},
		{"tool result", agent.Event{Type: agent.EvToolResult, Block: &provider.Block{
			Type: provider.BlockToolResult, ID: "t1", Content: "file body",
		}}, WireToolResult},
		{"turn done", agent.Event{Type: agent.EvTurnDone, Model: "anthropic/claude-opus-5",
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 4}, Cost: 0.002}, WireTurnDone},
		{"error", agent.Event{Type: agent.EvError, Err: errors.New("provider exploded")}, WireError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := FromAgent(tc.in)
			if got.Type != tc.want {
				t.Fatalf("type = %q, want %q", got.Type, tc.want)
			}
			// Everything on the wire must survive a JSON round trip: an
			// agent.Event carries an error value, which does not marshal.
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var back WireEvent
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if back != got {
				t.Errorf("round trip changed the event:\n got %+v\nwant %+v", back, got)
			}
		})
	}
}

func TestFromAgentKeepsTheErrorText(t *testing.T) {
	ev := FromAgent(agent.Event{Type: agent.EvError, Err: errors.New("provider exploded")})
	if ev.Error != "provider exploded" {
		t.Errorf("Error = %q, want the error's text", ev.Error)
	}
}

func TestFromAgentToleratesAMissingBlock(t *testing.T) {
	// A malformed event must not panic the broadcast goroutine and take the
	// whole daemon down with it.
	ev := FromAgent(agent.Event{Type: agent.EvToolCall})
	if ev.Type != WireToolCall || ev.Tool != "" {
		t.Errorf("got %+v, want an empty tool_call rather than a panic", ev)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -v`
Expected: FAIL — no such package.

- [ ] **Step 3: Write the wire event**

Create `internal/daemon/event.go`:

```go
// Package daemon serves spore over HTTP and SSE. It is a consumer of the
// agent's event channel and of the policy guard; the core knows nothing
// about it (spec invariant 1).
package daemon

import (
	"encoding/json"

	"github.com/codered/spore/internal/agent"
)

// Wire event types. These strings are the API: the web UI and the CLI client
// both switch on them, so they are append-only.
const (
	WireText       = "text"
	WireToolCall   = "tool_call"
	WireToolResult = "tool_result"
	WireTurnDone   = "turn_done"
	WireError      = "error"
	WireApproval   = "approval"
	WireResolved   = "resolved"
)

// WireEvent is one server-sent event. It is comparable on purpose — tests
// assert round-trip equality — so it holds no slices or maps; Args is a
// string carrying JSON rather than a json.RawMessage.
type WireEvent struct {
	Type string `json:"type"`

	// text
	Text string `json:"text,omitempty"`

	// tool_call / tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Args      string `json:"args,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`

	// turn_done
	Model     string  `json:"model,omitempty"`
	TokensIn  int     `json:"tokens_in,omitempty"`
	TokensOut int     `json:"tokens_out,omitempty"`
	CostUSD   float64 `json:"cost_usd,omitempty"`

	// error
	Error string `json:"error,omitempty"`

	// approval / resolved
	PendingID int64  `json:"pending_id,omitempty"`
	Rule      string `json:"rule,omitempty"`
	Pattern   string `json:"pattern,omitempty"`
	Decision  string `json:"decision,omitempty"`
}

// FromAgent converts a core event into its wire form. The agent's Err field
// is an error value, which does not marshal; it becomes text here.
func FromAgent(ev agent.Event) WireEvent {
	switch ev.Type {
	case agent.EvText:
		return WireEvent{Type: WireText, Text: ev.Text}
	case agent.EvToolCall:
		w := WireEvent{Type: WireToolCall}
		if ev.Block != nil {
			w.ToolUseID, w.Tool, w.Args = ev.Block.ID, ev.Block.Name, string(ev.Block.Input)
		}
		return w
	case agent.EvToolResult:
		w := WireEvent{Type: WireToolResult}
		if ev.Block != nil {
			w.ToolUseID, w.Content = ev.Block.ID, ev.Block.Content
			w.IsError, w.Truncated = ev.Block.IsError, ev.Block.Truncated
		}
		return w
	case agent.EvTurnDone:
		return WireEvent{
			Type: WireTurnDone, Model: ev.Model,
			TokensIn: ev.Usage.InputTokens, TokensOut: ev.Usage.OutputTokens,
			CostUSD: ev.Cost,
		}
	case agent.EvError:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		return WireEvent{Type: WireError, Error: msg}
	default:
		return WireEvent{Type: string(ev.Type)}
	}
}

// Encode renders the event as an SSE data frame body.
func (w WireEvent) Encode() ([]byte, error) { return json.Marshal(w) }
```

- [ ] **Step 4: Run the event test**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestFromAgent -v`
Expected: PASS.

- [ ] **Step 5: Write the failing hub test**

Create `internal/daemon/hub_test.go`:

```go
package daemon

import (
	"testing"
	"time"
)

func drain(t *testing.T, ch <-chan WireEvent, want int) []WireEvent {
	t.Helper()
	var got []WireEvent
	deadline := time.After(2 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of %d events", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), want)
		}
	}
	return got
}

func TestHubBroadcastsToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, closeA := h.Subscribe("s1")
	defer closeA()
	b, closeB := h.Subscribe("s1")
	defer closeB()
	other, closeOther := h.Subscribe("s2")
	defer closeOther()

	h.Publish("s1", WireEvent{Type: WireText, Text: "hello"})

	if got := drain(t, a, 1); got[0].Text != "hello" {
		t.Errorf("subscriber A got %+v", got[0])
	}
	if got := drain(t, b, 1); got[0].Text != "hello" {
		t.Errorf("subscriber B got %+v", got[0])
	}
	select {
	case ev := <-other:
		t.Errorf("session s2 received s1's event: %+v", ev)
	default:
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe("s1")
	stop()
	// The channel is closed by stop, and a later publish must not panic on a
	// send to a closed channel — a disconnected browser tab must not be able
	// to kill the process.
	h.Publish("s1", WireEvent{Type: WireText, Text: "after"})
	if _, open := <-ch; open {
		t.Error("subscribing channel still delivered after unsubscribe")
	}
}

func TestHubDropsForASubscriberThatStoppedReading(t *testing.T) {
	h := NewHub()
	slow, stopSlow := h.Subscribe("s1")
	defer stopSlow()
	fast, stopFast := h.Subscribe("s1")
	defer stopFast()

	// More events than any buffer: a client that wandered off must not block
	// the turn or the other client.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.Publish("s1", WireEvent{Type: WireText, Text: "x"})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on a subscriber that is not reading")
	}
	_ = slow
	if got := drain(t, fast, 1); got[0].Text != "x" {
		t.Errorf("fast subscriber got %+v", got[0])
	}
}

func TestHubAllowsOneTurnPerSession(t *testing.T) {
	h := NewHub()
	if !h.Begin("s1") {
		t.Fatal("first Begin was refused")
	}
	if h.Begin("s1") {
		t.Error("second Begin succeeded; one session must run one turn at a time")
	}
	if !h.Begin("s2") {
		t.Error("a different session was blocked by s1's turn")
	}
	h.End("s1")
	if !h.Begin("s1") {
		t.Error("Begin refused after End")
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestHub -v`
Expected: FAIL — `NewHub` undefined.

- [ ] **Step 7: Write the hub**

Create `internal/daemon/hub.go`:

```go
package daemon

import "sync"

// subscriberBuffer is how far a client may fall behind before it starts
// missing events. A browser tab that is not reading must never block the
// turn, so a full buffer drops rather than waits.
const subscriberBuffer = 256

type sessionHub struct {
	subs    map[chan WireEvent]struct{}
	running bool
}

// Hub fans one turn's events out to every client attached to a session, and
// tracks which sessions have a turn in flight. It is the whole of what makes
// a session "a row, not a process": the turn publishes here and never holds
// a reference to any client.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionHub
}

func NewHub() *Hub { return &Hub{sessions: map[string]*sessionHub{}} }

func (h *Hub) get(sessionID string) *sessionHub {
	sh, ok := h.sessions[sessionID]
	if !ok {
		sh = &sessionHub{subs: map[chan WireEvent]struct{}{}}
		h.sessions[sessionID] = sh
	}
	return sh
}

// Subscribe attaches a client to a session. The returned function detaches it
// and closes the channel; it is safe to call more than once.
func (h *Hub) Subscribe(sessionID string) (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, subscriberBuffer)
	h.mu.Lock()
	h.get(sessionID).subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			sh, ok := h.sessions[sessionID]
			if !ok {
				return
			}
			if _, still := sh.subs[ch]; !still {
				return
			}
			delete(sh.subs, ch)
			// Closing under the lock is what makes Publish safe: Publish
			// holds the same lock, so it can never be mid-send on a channel
			// that is being closed.
			close(ch)
			h.gc(sessionID, sh)
		})
	}
}

// Publish delivers to every current subscriber. A subscriber whose buffer is
// full is skipped, not waited on.
func (h *Hub) Publish(sessionID string, ev WireEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	for ch := range sh.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Begin claims the session's turn slot, reporting false when a turn is
// already running. Two clients posting at once must not interleave two turns
// into one transcript.
func (h *Hub) Begin(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh := h.get(sessionID)
	if sh.running {
		return false
	}
	sh.running = true
	return true
}

func (h *Hub) End(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	sh.running = false
	h.gc(sessionID, sh)
}

// gc drops the bookkeeping for a session with no subscribers and no turn, so
// a long-lived daemon does not accumulate one entry per session ever opened.
// Callers hold h.mu.
func (h *Hub) gc(sessionID string, sh *sessionHub) {
	if len(sh.subs) == 0 && !sh.running {
		delete(h.sessions, sessionID)
	}
}
```

- [ ] **Step 8: Run the hub tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -race -v`
Expected: PASS with no race reports.

- [ ] **Step 9: Write the seam test**

Create `internal/daemon/seam_test.go`:

```go
package daemon_test

import (
	"go/build"
	"strings"
	"testing"
)

// Spec invariant 1: the core never imports a transport. internal/daemon
// depends on internal/agent, so the check has to run from outside the core —
// this asserts the dependency stays one-directional.
func TestAgentCoreImportsNoTransport(t *testing.T) {
	banned := map[string]string{
		"net/http":                             "the core must not know about HTTP",
		"github.com/codered/spore/internal/daemon":    "the core must not import its own transport",
		"github.com/codered/spore/internal/scheduler": "the core must not import the scheduler",
	}
	for _, pkg := range []string{
		"github.com/codered/spore/internal/agent",
		"github.com/codered/spore/internal/provider",
		"github.com/codered/spore/internal/router",
	} {
		p, err := build.Import(pkg, "", 0)
		if err != nil {
			t.Fatalf("import %s: %v", pkg, err)
		}
		for _, imp := range p.Imports {
			if why, bad := banned[imp]; bad {
				t.Errorf("%s imports %s: %s", pkg, imp, why)
			}
			if strings.HasPrefix(imp, "github.com/codered/spore/internal/tool") {
				t.Errorf("%s imports %s: the core reaches tools through the ToolRunner seam", pkg, imp)
			}
		}
	}
}
```

- [ ] **Step 10: Run it and commit**

Run: `make vet && make test`
Expected: PASS across all packages.

```bash
git add internal/daemon
git commit -m "feat(daemon): wire events and the per-session broadcast hub"
```

---

### Task 3: The HTTP API — sessions, messages and the SSE stream

The API surface the spec names, minus approvals and jobs (Tasks 4 and 5). This is where invariant 2 is either honoured or lost, so the tests are written around exactly that: a turn started by one client is visible to another, and killing the first client does not kill the turn.

**Files:**
- Create: `internal/daemon/server.go`, `internal/daemon/sessions.go`, `internal/daemon/stream.go`, `internal/daemon/api_test.go`

**Interfaces:**
- Consumes: `Hub` (Task 2), `agent.Agent`, `store.Store`, `config.Config`, `policy.WithSession`, `policy.ProfileLocal`, `sporetrace.StartTurn`.
- Produces:
  - `daemon.Options{Agent *agent.Agent, Store *store.Store, Cfg *config.Config, Guard *policy.Guard}`
  - `daemon.New(Options) *Server`
  - `(*Server).Handler() http.Handler`
  - `(*Server).Run(ctx context.Context, addr string) error`
  - `(*Server).Hub() *Hub`
  - JSON types `daemon.SessionJSON`, `daemon.MessageJSON`, `daemon.TranscriptJSON`
  - Routes: `GET /healthz`, `GET /api/sessions`, `POST /api/sessions`, `GET /api/sessions/{id}`, `POST /api/sessions/{id}/messages`, `GET /api/sessions/{id}/events`

- [ ] **Step 1: Write the failing API test**

Create `internal/daemon/api_test.go`:

```go
package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// newTestServer wires a real store, a real agent and a scripted provider.
// Only the model is fake — the goal is to test the transport against the
// real core, not against a mock of it.
func newTestServer(t *testing.T, turns ...provider.ScriptTurn) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.DefaultModel = "script/fake"
	cfg.DataDir = t.TempDir()

	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	a := agent.New(st, preg, rt, cfg, nil)

	s := New(Options{Agent: a, Store: st, Cfg: cfg})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return res
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestCreateListAndShowSession(t *testing.T) {
	_, ts := newTestServer(t)

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "first"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}
	var created SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created session has no id")
	}

	listRes, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer listRes.Body.Close()
	var list []SessionJSON
	if err := json.NewDecoder(listRes.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the one created session", list)
	}

	showRes, err := http.Get(ts.URL + "/api/sessions/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer showRes.Body.Close()
	var tr TranscriptJSON
	if err := json.NewDecoder(showRes.Body).Decode(&tr); err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if tr.Session.ID != created.ID || len(tr.Messages) != 0 {
		t.Errorf("transcript = %+v, want the empty session", tr)
	}

	missing, err := http.Get(ts.URL + "/api/sessions/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", missing.StatusCode)
	}
}

// readSSE reads server-sent events off a live response until it has n of
// them or the deadline passes.
func readSSE(t *testing.T, body *bufio.Reader, n int) []WireEvent {
	t.Helper()
	var out []WireEvent
	for len(out) < n {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE after %d events: %v", len(out), err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev WireEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE payload %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestPostMessageStreamsToTwoAttachedClients(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{
		Text:  "hello from the model",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	})

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "stream"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Two clients attach BEFORE the turn starts.
	var readers []*bufio.Reader
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/"+sess.ID+"/events", nil)
		streamRes, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		t.Cleanup(func() { streamRes.Body.Close() })
		if ct := streamRes.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("content type = %q, want text/event-stream", ct)
		}
		readers = append(readers, bufio.NewReader(streamRes.Body))
	}

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "hi"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("post message status = %d, want 202", post.StatusCode)
	}

	for i, r := range readers {
		events := readSSE(t, r, 2)
		if events[0].Type != WireText || events[0].Text != "hello from the model" {
			t.Errorf("client %d first event = %+v", i, events[0])
		}
		if events[1].Type != WireTurnDone || events[1].Model != "script/fake" {
			t.Errorf("client %d second event = %+v", i, events[1])
		}
	}
}

func TestTurnSurvivesTheClientThatStartedItDisconnecting(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{Text: "persisted anyway"})

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "abandoned"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Post with a context that is cancelled the instant the response returns:
	// the turn's lifetime must belong to the daemon, not to this request.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST",
		ts.URL+"/api/sessions/"+sess.ID+"/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	post, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	post.Body.Close()
	cancel()

	// The reply must land in the transcript despite nobody listening.
	deadline := time.Now().Add(3 * time.Second)
	for {
		show, err := http.Get(ts.URL + "/api/sessions/" + sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		var tr TranscriptJSON
		json.NewDecoder(show.Body).Decode(&tr)
		show.Body.Close()
		// user message plus assistant reply
		if len(tr.Messages) >= 2 {
			if tr.Messages[1].Role != "assistant" {
				t.Fatalf("second message role = %q, want assistant", tr.Messages[1].Role)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn did not complete after the client disconnected; transcript has %d messages", len(tr.Messages))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSecondTurnIsRejectedWhileOneIsRunning(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "one"})
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "busy"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Claim the slot directly so the state is unambiguous.
	if !s.Hub().Begin(sess.ID) {
		t.Fatal("could not claim the turn slot")
	}
	defer s.Hub().End(sess.ID)

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "hi"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 while a turn is running", post.StatusCode)
	}
}

func TestPostMessageRejectsEmptyTextAndUnknownSession(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "validate"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	empty := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "  "})
	defer empty.Body.Close()
	if empty.StatusCode != http.StatusBadRequest {
		t.Errorf("empty text status = %d, want 400", empty.StatusCode)
	}

	unknown := postJSON(t, ts.URL+"/api/sessions/nope/messages", map[string]string{"text": "hi"})
	defer unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", unknown.StatusCode)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'TestHealthz|TestCreate|TestPost|TestTurn|TestSecond' -v`
Expected: FAIL — `New`, `Options`, `SessionJSON` undefined.

- [ ] **Step 3: Write the server**

Create `internal/daemon/server.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

// Options are the daemon's collaborators. Guard may be nil in tests that do
// not exercise tools; everything else is required.
type Options struct {
	Agent *agent.Agent
	Store *store.Store
	Cfg   *config.Config
	Guard *policy.Guard
}

type Server struct {
	agent *agent.Agent
	store *store.Store
	cfg   *config.Config
	guard *policy.Guard
	hub   *Hub

	// base bounds every turn's lifetime. It is the SERVER's context, never a
	// request's: a turn survives the client that started it (spec invariant
	// 2), so a handler must never hand its own context to agent.Run.
	base   context.Context
	cancel context.CancelFunc
}

func New(o Options) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		agent: o.Agent, store: o.Store, cfg: o.Cfg, guard: o.Guard,
		hub: NewHub(), base: ctx, cancel: cancel,
	}
}

func (s *Server) Hub() *Hub { return s.hub }

// Close cancels every in-flight turn. Run calls it on shutdown.
func (s *Server) Close() { s.cancel() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleShowSession)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	return mux
}

// Run serves until ctx is cancelled, then drains with a short grace period.
func (s *Server) Run(ctx context.Context, addr string) error {
	if err := config.ValidateDaemonAddr(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}
```

`config.ValidateDaemonAddr` is the loopback check Task 1 added; `Run` re-checks the address it is handed rather than trusting its caller.

- [ ] **Step 4: Write the session handlers**

Create `internal/daemon/sessions.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

type SessionJSON struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type MessageJSON struct {
	Seq       int              `json:"seq"`
	Role      string           `json:"role"`
	Blocks    []provider.Block `json:"blocks"`
	Model     string           `json:"model,omitempty"`
	TokensIn  int              `json:"tokens_in,omitempty"`
	TokensOut int              `json:"tokens_out,omitempty"`
	CostUSD   float64          `json:"cost_usd,omitempty"`
	CreatedAt time.Time        `json:"created_at"`
}

type TranscriptJSON struct {
	Session  SessionJSON   `json:"session"`
	Messages []MessageJSON `json:"messages"`
	// Running reports whether a turn is in flight, so a client attaching
	// mid-turn knows to expect deltas rather than assuming it is idle.
	Running bool `json:"running"`
}

func toSessionJSON(s store.Session) SessionJSON {
	return SessionJSON{ID: s.ID, Title: s.Title, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sessions: %v", err)
		return
	}
	out := make([]SessionJSON, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, toSessionJSON(sess))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
	}
	// An empty body is fine — a session with no title is legal.
	_ = json.NewDecoder(r.Body).Decode(&body)
	id, err := s.store.CreateSession(r.Context(), strings.TrimSpace(body.Title))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create session: %v", err)
		return
	}
	writeJSON(w, http.StatusCreated, SessionJSON{ID: id, Title: body.Title,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
}

// findSession returns the session row, or writes a 404 and reports false.
func (s *Server) findSession(w http.ResponseWriter, r *http.Request, id string) (store.Session, bool) {
	sessions, err := s.store.ListSessions(r.Context(), 1000)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read sessions: %v", err)
		return store.Session{}, false
	}
	for _, sess := range sessions {
		if sess.ID == id {
			return sess, true
		}
	}
	writeError(w, http.StatusNotFound, "no session %s", id)
	return store.Session{}, false
}

func (s *Server) handleShowSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.findSession(w, r, id)
	if !ok {
		return
	}
	rows, err := s.store.Messages(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read messages: %v", err)
		return
	}
	out := TranscriptJSON{Session: toSessionJSON(sess), Messages: []MessageJSON{}, Running: s.hub.Running(id)}
	for _, m := range rows {
		var blocks []provider.Block
		if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
			writeError(w, http.StatusInternalServerError, "decode message %d: %v", m.Seq, err)
			return
		}
		out.Messages = append(out.Messages, MessageJSON{
			Seq: m.Seq, Role: m.Role, Blocks: blocks, Model: m.Model,
			TokensIn: m.TokensIn, TokensOut: m.TokensOut, CostUSD: m.CostUSD,
			CreatedAt: m.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handlePostMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if !s.hub.Begin(id) {
		writeError(w, http.StatusConflict, "session %s already has a turn running", id)
		return
	}
	if err := s.startTurn(id, text, "http"); err != nil {
		s.hub.End(id)
		writeError(w, http.StatusInternalServerError, "start turn: %v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

// startTurn runs one turn on the SERVER's context and pumps its events into
// the hub. The caller must already hold the session's turn slot; startTurn
// releases it when the turn ends.
func (s *Server) startTurn(sessionID, text, client string) error {
	ctx := policy.WithSession(s.base, sessionID, policy.ProfileLocal)
	ctx, turn := sporetrace.StartTurn(ctx, sessionID, client)

	ch, err := s.agent.Run(ctx, sessionID, text)
	if err != nil {
		turn.End()
		return err
	}
	go func() {
		defer s.hub.End(sessionID)
		defer turn.End()
		for ev := range ch {
			if ev.Type == agent.EvError && ev.Err != nil {
				turn.RecordError(ev.Err)
			}
			s.hub.Publish(sessionID, FromAgent(ev))
		}
	}()
	return nil
}
```

This file's imports are: `encoding/json`, `net/http`, `strings`, `time`, and the spore packages `internal/agent`, `internal/policy`, `internal/provider`, `internal/store`, `internal/trace`.

Add `Running` to the hub — in `internal/daemon/hub.go`:

```go
// Running reports whether a turn is in flight for the session.
func (h *Hub) Running(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	return ok && sh.running
}
```

- [ ] **Step 5: Write the SSE endpoint**

Create `internal/daemon/stream.go`:

```go
package daemon

import (
	"fmt"
	"net/http"
	"time"
)

// heartbeat keeps an idle SSE connection from being closed by an intermediary
// and gives the client a liveness signal between turns.
const heartbeat = 25 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Subscribe BEFORE writing the header so no event published between the
	// two is lost by a client that has already been told the stream is open.
	events, unsubscribe := s.hub.Subscribe(id)
	defer unsubscribe()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A client attaching after a restart, or as a second client, is told
	// about anything already waiting on a human before it sees any deltas.
	for _, ev := range s.pendingApprovalEvents(r.Context(), id) {
		writeSSE(w, flusher, ev)
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case ev, open := <-events:
			if !open {
				return
			}
			if !writeSSE(w, flusher, ev) {
				return
			}
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			// The client went away. Unsubscribing is all that happens: the
			// turn belongs to the daemon and keeps running.
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev WireEvent) bool {
	payload, err := ev.Encode()
	if err != nil {
		return true // skip an unencodable event rather than dropping the stream
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	f.Flush()
	return true
}
```

`pendingApprovalEvents` arrives in Task 4. Until then, add this stub to `stream.go` so the package compiles, and delete it in Task 4 Step 5:

```go
// pendingApprovalEvents is filled in by Task 4; it lists approvals already
// waiting on a human when a client attaches.
func (s *Server) pendingApprovalEvents(ctx context.Context, sessionID string) []WireEvent { return nil }
```

(add `"context"` to the imports for the stub).

- [ ] **Step 6: Run the API tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -race -v`
Expected: PASS. If `TestTurnSurvives...` fails, the handler is passing `r.Context()` into `agent.Run` — that is the invariant this task exists to hold.

- [ ] **Step 7: Commit**

```bash
make vet && make test
git add internal/daemon internal/config
git commit -m "feat(daemon): session API and the per-session SSE stream"
```

---

### Task 4: Approvals over HTTP

`policy.Approver.Ask` blocks until a human answers, but HTTP has no way to block a browser. The broker inverts it: `Ask` publishes an approval event and waits on a channel; the resolve endpoint hands the answer to that waiter. When no waiter exists — the daemon restarted while an approval was open — the endpoint falls through to `Guard.Resolve`, the out-of-band path Plan 2 already built. Getting the two paths mutually exclusive is the point of this task: both of them record an audit row, and a call that took both would record two.

**Files:**
- Create: `internal/daemon/approver.go`, `internal/daemon/approver_test.go`
- Modify: `internal/daemon/server.go` (routes), `internal/daemon/stream.go` (drop the stub)

**Interfaces:**
- Consumes: `policy.Approver`, `policy.Ask`, `policy.Answer`, `policy.Scope*`, `(*policy.Guard).Pending`, `(*policy.Guard).Resolve`, `store.PendingCall`, `Hub` (Task 2).
- Produces:
  - `daemon.NewBroker(h *Hub) *Broker`, satisfying `policy.Approver`
  - `(*Broker).Ask(ctx context.Context, a policy.Ask) (policy.Answer, error)`
  - `(*Broker).Answer(pendingID int64, ans policy.Answer) bool`
  - `(*Server).pendingApprovalEvents(ctx context.Context, sessionID string) []WireEvent`
  - Routes: `GET /api/sessions/{id}/approvals`, `POST /api/sessions/{id}/approvals/{pending}`

- [ ] **Step 1: Write the failing broker test**

Create `internal/daemon/approver_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
)

func TestBrokerDeliversAnAnswerToTheWaitingAsk(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	type result struct {
		ans policy.Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := b.Ask(context.Background(), policy.Ask{
			SessionID: "s1", Tool: "shell_exec", PendingID: 7,
			Args: json.RawMessage(`{"cmd":"ls"}`), Rule: "shell_exec", Pattern: "shell_exec",
		})
		done <- result{ans, err}
	}()

	// The ask reaches every attached client as an approval event.
	select {
	case ev := <-events:
		if ev.Type != WireApproval || ev.PendingID != 7 || ev.Tool != "shell_exec" {
			t.Fatalf("published %+v, want an approval for pending 7", ev)
		}
		if ev.Args != `{"cmd":"ls"}` || ev.Rule != "shell_exec" {
			t.Errorf("approval event lost its detail: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask published no approval event")
	}

	if !b.Answer(7, policy.Answer{Allow: true, Scope: policy.ScopeSession}) {
		t.Fatal("Answer reported no waiter, but Ask is waiting")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Ask returned %v", got.err)
		}
		if !got.ans.Allow || got.ans.Scope != policy.ScopeSession {
			t.Errorf("Ask returned %+v, want the answer that was posted", got.ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after Answer")
	}
}

func TestBrokerReportsNoWaiterForAnUnknownPendingID(t *testing.T) {
	b := NewBroker(NewHub())
	// This is the case that must fall through to Guard.Resolve rather than
	// silently succeeding: nothing is waiting, so nothing was answered.
	if b.Answer(99, policy.Answer{Allow: true}) {
		t.Error("Answer claimed to deliver to a waiter that does not exist")
	}
}

func TestBrokerOnlyOneAnswerWins(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	go b.Ask(context.Background(), policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 3})

	deadline := time.After(2 * time.Second)
	for {
		if b.Answer(3, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the waiter never registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// A second client answering the same approval must be told it lost, so
	// the handler can report "already answered" instead of recording twice.
	if b.Answer(3, policy.Answer{Allow: false, Scope: policy.ScopeOnce}) {
		t.Error("a second Answer for the same pending id also won")
	}
}

func TestBrokerAskHonoursCancellation(t *testing.T) {
	b := NewBroker(NewHub())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(ctx, policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 5})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Ask returned no error after its context was cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask ignored cancellation; an unanswered approval would wait forever")
	}
	// The waiter must be gone, or the map grows without bound.
	if b.Answer(5, policy.Answer{Allow: true}) {
		t.Error("a cancelled Ask left its waiter registered")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestBroker -v`
Expected: FAIL — `NewBroker` undefined.

- [ ] **Step 3: Write the broker**

Create `internal/daemon/approver.go`:

```go
package daemon

import (
	"context"
	"sync"

	"github.com/codered/spore/internal/policy"
)

// Broker is the daemon's policy.Approver. Ask cannot prompt a browser
// synchronously, so it publishes the request to every client attached to the
// session and blocks on a waiter keyed by the persisted suspension's id; the
// resolve endpoint delivers the answer.
//
// Exactly one of two paths records an approval: this waiter (after which
// Guard.Run writes the audit row), or Guard.Resolve when no waiter exists.
// Answer's bool is what keeps them mutually exclusive.
type Broker struct {
	hub *Hub

	mu      sync.Mutex
	waiters map[int64]chan policy.Answer
}

func NewBroker(h *Hub) *Broker {
	return &Broker{hub: h, waiters: map[int64]chan policy.Answer{}}
}

// Ask implements policy.Approver.
func (b *Broker) Ask(ctx context.Context, a policy.Ask) (policy.Answer, error) {
	ch := make(chan policy.Answer, 1)
	b.mu.Lock()
	b.waiters[a.PendingID] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiters, a.PendingID)
		b.mu.Unlock()
	}()

	b.hub.Publish(a.SessionID, approvalEvent(a))

	select {
	case ans := <-ch:
		b.hub.Publish(a.SessionID, WireEvent{
			Type: WireResolved, PendingID: a.PendingID, Tool: a.Tool,
			Decision: decisionOf(ans),
		})
		return ans, nil
	case <-ctx.Done():
		// The guard turns a deadline into a denial and records it; the broker
		// only reports why it stopped waiting.
		return policy.Answer{}, ctx.Err()
	}
}

// Answer hands an answer to a waiting Ask, reporting whether one was there.
// False means the daemon restarted (or the turn was abandoned) since the
// suspension was written, and the caller should use Guard.Resolve instead.
func (b *Broker) Answer(pendingID int64, ans policy.Answer) bool {
	b.mu.Lock()
	ch, ok := b.waiters[pendingID]
	if ok {
		// Delete under the lock so a second concurrent Answer finds nothing
		// and is told it lost.
		delete(b.waiters, pendingID)
	}
	b.mu.Unlock()
	if !ok {
		return false
	}
	ch <- ans
	return true
}

func approvalEvent(a policy.Ask) WireEvent {
	return WireEvent{
		Type: WireApproval, PendingID: a.PendingID, Tool: a.Tool,
		Args: string(a.Args), Rule: a.Rule, Pattern: a.Pattern,
	}
}

func decisionOf(a policy.Answer) string {
	if a.Allow {
		return "allow"
	}
	return "deny"
}
```

- [ ] **Step 4: Run the broker tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestBroker -race -v`
Expected: PASS.

- [ ] **Step 5: Wire the broker into the server**

In `internal/daemon/server.go`, add the field and constructor line:

```go
	broker *Broker
```

```go
	s := &Server{
		agent: o.Agent, store: o.Store, cfg: o.Cfg, guard: o.Guard,
		hub: NewHub(), base: ctx, cancel: cancel,
	}
	s.broker = NewBroker(s.hub)
	return s
```

and expose it so `cmd/spore` can hand it to the guard:

```go
// Approver is the policy.Approver the guard must be built with. The daemon
// creates it because it owns the hub the approval events travel over.
func (s *Server) Approver() policy.Approver { return s.broker }
```

Add the routes in `Handler()`:

```go
	mux.HandleFunc("GET /api/sessions/{id}/approvals", s.handleListApprovals)
	mux.HandleFunc("POST /api/sessions/{id}/approvals/{pending}", s.handleResolveApproval)
```

Delete the `pendingApprovalEvents` stub from `stream.go` (and its `"context"` import if now unused), and add the real handlers to `approver.go`:

```go
// pendingApprovalEvents lists the approvals already waiting on a human, as
// wire events. A client attaching mid-suspension — a second browser tab, or
// the first one after a daemon restart — sees them before any deltas.
func (s *Server) pendingApprovalEvents(ctx context.Context, sessionID string) []WireEvent {
	if s.guard == nil {
		return nil
	}
	pending, err := s.guard.Pending(ctx, sessionID)
	if err != nil {
		return nil
	}
	out := make([]WireEvent, 0, len(pending))
	for _, p := range pending {
		out = append(out, WireEvent{
			Type: WireApproval, PendingID: p.ID, Tool: p.Tool,
			Args: string(p.ArgsJSON), Rule: p.Rule,
			Pattern: policy.PatternFor(policy.Call{Tool: p.Tool, Args: p.ArgsJSON}),
		})
	}
	return out
}

func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.pendingApprovalEvents(r.Context(), id))
}

func (s *Server) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	pendingID, err := strconv.ParseInt(r.PathValue("pending"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "pending id must be a number: %v", err)
		return
	}
	var body struct {
		Allow bool   `json:"allow"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	scope := policy.Scope(body.Scope)
	switch scope {
	case policy.ScopeOnce, policy.ScopeSession, policy.ScopePattern:
	case "":
		scope = policy.ScopeOnce
	default:
		writeError(w, http.StatusBadRequest, "scope must be once, session or pattern, got %q", body.Scope)
		return
	}
	ans := policy.Answer{Allow: body.Allow, Scope: scope}

	// A live waiter is the normal path: the suspended turn takes the answer
	// and Guard.Run records it. Only when nothing is waiting — the daemon
	// restarted while the approval was open — does the out-of-band path
	// apply. Taking both would write two audit rows for one decision.
	if s.broker.Answer(pendingID, ans) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "delivered"})
		return
	}
	if s.guard == nil {
		writeError(w, http.StatusNotFound, "no approval %d is waiting", pendingID)
		return
	}
	if err := s.guard.Resolve(r.Context(), sessionID, pendingID, ans); err != nil {
		writeError(w, http.StatusConflict, "%v", err)
		return
	}
	s.hub.Publish(sessionID, WireEvent{Type: WireResolved, PendingID: pendingID, Decision: decisionOf(ans)})
	writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
}
```

Add `encoding/json`, `net/http` and `strconv` to `approver.go`'s imports.

- [ ] **Step 6: Test the resolve endpoint end to end**

Append to `internal/daemon/approver_test.go`:

```go
func TestResolveEndpointDeliversToTheWaiter(t *testing.T) {
	s, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "approve"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	done := make(chan policy.Answer, 1)
	go func() {
		ans, _ := s.Approver().Ask(context.Background(), policy.Ask{
			SessionID: sess.ID, Tool: "fs_write", PendingID: 42, Rule: "fs_write",
		})
		done <- ans
	}()
	waitForWaiter(t, s, 42)

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/42",
		map[string]any{"allow": true, "scope": "once"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", post.StatusCode)
	}
	select {
	case ans := <-done:
		if !ans.Allow {
			t.Error("the waiter received a denial")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting Ask never returned")
	}
}

func TestResolveEndpointRejectsABadScope(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "approve"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/1",
		map[string]any{"allow": true, "scope": "forever"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown scope", post.StatusCode)
	}
}
```

Add this helper to `api_test.go`:

```go
// waitForWaiter blocks until the broker has registered a waiter for id, so a
// test never races the goroutine that calls Ask.
func waitForWaiter(t *testing.T, s *Server, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.broker.mu.Lock()
		_, ok := s.broker.waiters[id]
		s.broker.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no waiter registered for pending %d", id)
}
```

Add `"net/http"`, `"time"` and `"github.com/codered/spore/internal/policy"` to `approver_test.go`'s imports.

- [ ] **Step 7: Run the whole daemon package**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
make vet && make test
git add internal/daemon
git commit -m "feat(daemon): approvals over SSE with an out-of-band resolve fallback"
```

---

### Task 5: The scheduler

A job is a prompt plus a schedule. The scheduler owns two things and nothing else: turning a spec string into "when next", and calling a `Runner` when that time has passed. It never touches the agent, which is what keeps it testable with an injected clock and no model.

The cron parser is written here rather than taken as a dependency. `Next` steps minute by minute over a bounded horizon instead of doing calendar arithmetic — pathological specs cost a few hundred thousand cheap iterations once per fire, and the implementation is obviously correct rather than subtly wrong about month lengths.

**Files:**
- Create: `internal/scheduler/cron.go`, `internal/scheduler/cron_test.go`
- Create: `internal/scheduler/scheduler.go`, `internal/scheduler/scheduler_test.go`
- Modify: `internal/daemon/server.go` (job routes), `internal/daemon/jobs.go` (create)

**Interfaces:**
- Consumes: `store.Job`, `(*store.Store).DueJobs`, `CreateJob`, `ListJobs`, `MarkJobRun`, `SetJobEnabled` (Task 1).
- Produces:
  - `scheduler.Schedule` interface: `Next(t time.Time) time.Time`, `Kind() string`
  - `scheduler.Parse(spec string) (Schedule, error)`
  - `scheduler.Runner` interface: `StartJob(ctx context.Context, job store.Job) (sessionID string, err error)`
  - `scheduler.New(st *store.Store, r Runner, now func() time.Time) *Scheduler`
  - `(*Scheduler).Tick(ctx context.Context) error`
  - `(*Scheduler).Run(ctx context.Context, every time.Duration) error`
  - `daemon.JobJSON`, and routes `GET /api/jobs`, `POST /api/jobs`, `DELETE /api/jobs/{id}`

- [ ] **Step 1: Write the failing cron test**

Create `internal/scheduler/cron_test.go`:

```go
package scheduler

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, spec string) Schedule {
	t.Helper()
	s, err := Parse(spec)
	if err != nil {
		t.Fatalf("Parse(%q): %v", spec, err)
	}
	return s
}

func TestParseCronNext(t *testing.T) {
	from := time.Date(2026, 9, 1, 8, 30, 0, 0, time.UTC) // a Tuesday
	cases := []struct {
		spec string
		want time.Time
	}{
		{"* * * * *", time.Date(2026, 9, 1, 8, 31, 0, 0, time.UTC)},
		{"0 9 * * *", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)},
		{"0 8 * * *", time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)},
		{"*/15 * * * *", time.Date(2026, 9, 1, 8, 45, 0, 0, time.UTC)},
		{"0 9 * * 1", time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)},   // next Monday
		{"30 6 1 * *", time.Date(2026, 10, 1, 6, 30, 0, 0, time.UTC)}, // first of next month
		{"0 0 29 2 *", time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC)},  // next leap day
		{"0 9,17 * * *", time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)},
		{"0 0 * * 0", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},   // Sunday as 0
		{"0 0 * * 7", time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)},   // Sunday as 7
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			s := mustParse(t, tc.spec)
			if s.Kind() != "cron" {
				t.Errorf("Kind() = %q, want cron", s.Kind())
			}
			if got := s.Next(from); !got.Equal(tc.want) {
				t.Errorf("Next(%v) = %v, want %v", from, got, tc.want)
			}
		})
	}
}

func TestNextIsStrictlyAfterTheGivenTime(t *testing.T) {
	// A schedule that matches "now" must return the NEXT match, or the tick
	// loop fires the same job forever.
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	s := mustParse(t, "0 9 * * *")
	if got := s.Next(at); !got.Equal(at.Add(24 * time.Hour)) {
		t.Errorf("Next(exact match) = %v, want tomorrow", got)
	}
}

func TestParseOneShot(t *testing.T) {
	s := mustParse(t, "2026-12-25T09:00:00Z")
	if s.Kind() != "once" {
		t.Errorf("Kind() = %q, want once", s.Kind())
	}
	before := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	want := time.Date(2026, 12, 25, 9, 0, 0, 0, time.UTC)
	if got := s.Next(before); !got.Equal(want) {
		t.Errorf("Next(before) = %v, want %v", got, want)
	}
	// After it has fired there is no next run; the zero time is how a
	// schedule says "retire me".
	if got := s.Next(want); !got.IsZero() {
		t.Errorf("Next(at or after the instant) = %v, want the zero time", got)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, spec := range []string{
		"", "  ", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *",
		"* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8", "a * * * *",
		"*/0 * * * *", "5-1 * * * *", "2026-12-25", "not-a-time",
	} {
		if _, err := Parse(spec); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", spec)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/scheduler/ -v`
Expected: FAIL — no such package.

- [ ] **Step 3: Write the cron parser**

Create `internal/scheduler/cron.go`:

```go
// Package scheduler fires stored jobs. It knows about the jobs table and a
// Runner callback and nothing else — in particular it never imports the
// agent, so its whole surface tests against an injected clock.
package scheduler

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule answers one question: given a time, when does this next fire?
type Schedule interface {
	// Next returns the first fire time strictly after t, or the zero time
	// when the schedule has no further runs.
	Next(t time.Time) time.Time
	// Kind is "cron" or "once"; it is stored on the job row.
	Kind() string
}

// maxHorizon bounds the search in Next. A schedule with no match inside a
// leap-year cycle (29 February on a weekday that never coincides, say) is
// reported as having no next run rather than looping forever.
const maxHorizon = 5 * 366 * 24 * time.Hour

// Parse accepts either an RFC3339 instant (a one-shot job) or a five-field
// cron expression: minute hour day-of-month month day-of-week.
func Parse(spec string) (Schedule, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("schedule is empty")
	}
	if at, err := time.Parse(time.RFC3339, spec); err == nil {
		return onceSchedule{at: at.UTC()}, nil
	}
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule %q must be an RFC3339 instant or five cron fields, got %d fields", spec, len(fields))
	}
	var c cronSchedule
	var err error
	if c.minute, err = parseField(fields[0], 0, 59); err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	if c.hour, err = parseField(fields[1], 0, 23); err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	if c.dom, err = parseField(fields[2], 1, 31); err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	if c.month, err = parseField(fields[3], 1, 12); err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	// Both 0 and 7 mean Sunday, which is the one place the two common cron
	// dialects agree, so both are accepted and normalised to 0.
	if c.dow, err = parseField(fields[4], 0, 7); err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}
	if c.dow[7] {
		c.dow[0] = true
	}
	c.restrictedDOM = fields[2] != "*"
	c.restrictedDOW = fields[4] != "*"
	return c, nil
}

// parseField expands one cron field into a membership set. Supported forms:
// "*", "*/n", "a", "a-b", "a-b/n", and any comma-separated list of those.
func parseField(field string, min, max int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("empty element in %q", field)
		}
		step := 1
		if slash := strings.Index(part, "/"); slash >= 0 {
			var err error
			step, err = strconv.Atoi(part[slash+1:])
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("step in %q must be a positive number", part)
			}
			part = part[:slash]
		}
		lo, hi := min, max
		if part != "*" {
			if dash := strings.Index(part, "-"); dash >= 0 {
				var err error
				if lo, err = strconv.Atoi(part[:dash]); err != nil {
					return nil, fmt.Errorf("range start in %q: %w", part, err)
				}
				if hi, err = strconv.Atoi(part[dash+1:]); err != nil {
					return nil, fmt.Errorf("range end in %q: %w", part, err)
				}
				if lo > hi {
					return nil, fmt.Errorf("range %q runs backwards", part)
				}
			} else {
				v, err := strconv.Atoi(part)
				if err != nil {
					return nil, fmt.Errorf("%q is not a number", part)
				}
				lo, hi = v, v
			}
		}
		if lo < min || hi > max {
			return nil, fmt.Errorf("%q is outside the valid range %d-%d", part, min, max)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("field %q matches nothing", field)
	}
	return out, nil
}

type cronSchedule struct {
	minute, hour, dom, month, dow map[int]bool
	// Standard cron oddity: when BOTH day-of-month and day-of-week are
	// restricted, a day matching EITHER fires. When only one is restricted,
	// only it applies.
	restrictedDOM, restrictedDOW bool
}

func (c cronSchedule) Kind() string { return "cron" }

func (c cronSchedule) matches(t time.Time) bool {
	if !c.minute[t.Minute()] || !c.hour[t.Hour()] || !c.month[int(t.Month())] {
		return false
	}
	domOK, dowOK := c.dom[t.Day()], c.dow[int(t.Weekday())]
	switch {
	case c.restrictedDOM && c.restrictedDOW:
		return domOK || dowOK
	case c.restrictedDOM:
		return domOK
	case c.restrictedDOW:
		return dowOK
	default:
		return true
	}
}

// Next steps minute by minute. A year of minutes is about half a million
// cheap comparisons and this runs once per fire, so the simplicity is worth
// more than calendar arithmetic that has to be right about leap years.
func (c cronSchedule) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := t.Add(maxHorizon)
	for ; t.Before(limit); t = t.Add(time.Minute) {
		if c.matches(t) {
			return t
		}
	}
	return time.Time{}
}

type onceSchedule struct{ at time.Time }

func (o onceSchedule) Kind() string { return "once" }

func (o onceSchedule) Next(t time.Time) time.Time {
	if t.UTC().Before(o.at) {
		return o.at
	}
	return time.Time{}
}
```

- [ ] **Step 4: Run the cron tests**

Run: `go test -tags sqlite_fts5 ./internal/scheduler/ -v`
Expected: PASS. `TestParseCronNext/0_0_29_2_*` is the slow one and should still finish well under a second.

- [ ] **Step 5: Write the failing scheduler test**

Create `internal/scheduler/scheduler_test.go`:

```go
package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []store.Job
	next  int
	err   error
}

func (f *fakeRunner) StartJob(ctx context.Context, j store.Job) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, j)
	f.next++
	return "sess-" + string(rune('a'+f.next-1)), nil
}

func (f *fakeRunner) started() []store.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Job(nil), f.calls...)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTickFiresADueCronJobAndReschedulesIt(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)

	id, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing",
		Enabled: true, NextRun: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	started := r.started()
	if len(started) != 1 || started[0].ID != id {
		t.Fatalf("started = %+v, want exactly job %d", started, id)
	}

	jobs, _ := st.ListJobs(ctx)
	want := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	if !jobs[0].NextRun.Equal(want) {
		t.Errorf("next_run = %v, want %v", jobs[0].NextRun, want)
	}
	if jobs[0].LastSessionID == "" {
		t.Error("last_session_id was not recorded")
	}
	if !jobs[0].Enabled {
		t.Error("a cron job was disabled after firing")
	}

	// A second tick at the same instant must do nothing: next_run has moved.
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Errorf("job fired %d times on two ticks, want 1", len(got))
	}
}

func TestAMissedRunFiresOnceAndIsNotBackfilled(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// The daemon was down for eight days; a daily job has eight missed slots.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "daily",
		Enabled: true, NextRun: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Fatalf("a job missed 8 times started %d turns, want exactly 1", len(got))
	}
	jobs, _ := st.ListJobs(ctx)
	want := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	if !jobs[0].NextRun.Equal(want) {
		t.Errorf("next_run = %v, want the next FUTURE slot %v", jobs[0].NextRun, want)
	}
}

func TestAOneShotJobIsDisabledAfterItFires(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "once", Spec: "2026-09-01T09:00:00Z", Prompt: "one time",
		Enabled: true, NextRun: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	jobs, _ := st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("a one-shot job is still enabled after firing")
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Errorf("one-shot job fired %d times, want 1", len(got))
	}
}

func TestAJobWithAnUnparseableSpecIsDisabledNotRetriedForever(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "not a schedule", Prompt: "broken",
		Enabled: true, NextRun: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick returned an error for one bad job: %v", err)
	}
	if got := r.started(); len(got) != 0 {
		t.Errorf("a job with an invalid spec was started: %+v", got)
	}
	jobs, _ := st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("a job with an unparseable spec is still enabled and will be retried every tick")
	}
}

func TestOneFailingJobDoesNotStopTheOthers(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	for _, prompt := range []string{"first", "second"} {
		if _, err := st.CreateJob(ctx, store.Job{
			Kind: "cron", Spec: "0 9 * * *", Prompt: prompt,
			Enabled: true, NextRun: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}
	r := &fakeRunner{err: context.DeadlineExceeded}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick propagated a job failure: %v", err)
	}
	// Both jobs must still have been advanced, so a runner that is briefly
	// unavailable does not leave every job permanently due.
	jobs, _ := st.ListJobs(ctx)
	for _, j := range jobs {
		if !j.NextRun.After(now) {
			t.Errorf("job %d was left due after a failed start (next_run %v)", j.ID, j.NextRun)
		}
	}
}
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/scheduler/ -run 'TestTick|TestA|TestOne' -v`
Expected: FAIL — `New` undefined.

- [ ] **Step 7: Write the scheduler**

Create `internal/scheduler/scheduler.go`:

```go
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/store"
)

var (
	ErrPromptRequired = errors.New("prompt is required")
	ErrNoFutureRun    = errors.New("schedule has no future run")
)

// Runner starts a job's turn. The scheduler holds no reference to the agent;
// the daemon supplies this.
type Runner interface {
	// StartJob opens a FRESH session for the job and begins its turn,
	// returning the new session id. It returns as soon as the turn is under
	// way — a long turn must not stall the tick loop.
	StartJob(ctx context.Context, job store.Job) (sessionID string, err error)
}

type Scheduler struct {
	store  *store.Store
	runner Runner
	now    func() time.Time
}

func New(st *store.Store, r Runner, now func() time.Time) *Scheduler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Scheduler{store: st, runner: r, now: now}
}

// Tick fires every job whose next_run has passed. A job is advanced BEFORE
// it is started: a crash mid-turn then costs one skipped run rather than an
// endless re-fire loop on every restart.
//
// The next time is computed from NOW, not from the missed slot, which is the
// whole of the no-backfill rule: a daemon down for a week fires each daily
// job once, not seven times.
func (s *Scheduler) Tick(ctx context.Context) error {
	now := s.now().UTC()
	due, err := s.store.DueJobs(ctx, now)
	if err != nil {
		return fmt.Errorf("read due jobs: %w", err)
	}
	for _, job := range due {
		sched, err := Parse(job.Spec)
		if err != nil {
			// A spec that cannot be parsed will never become valid on its
			// own, and leaving it enabled means retrying it every tick
			// forever. Disable it and say why.
			slog.Error("disabling job with an invalid schedule", "job", job.ID, "spec", job.Spec, "err", err)
			if err := s.store.SetJobEnabled(ctx, job.ID, false); err != nil {
				slog.Error("could not disable the invalid job", "job", job.ID, "err", err)
			}
			continue
		}
		next := sched.Next(now)
		sessionID, startErr := s.runner.StartJob(ctx, job)
		if startErr != nil {
			// The job still advances. A runner that is briefly unavailable
			// must not leave every job permanently due and firing on every
			// tick once it recovers.
			slog.Error("job did not start", "job", job.ID, "err", startErr)
		}
		if err := s.store.MarkJobRun(ctx, job.ID, now, next, sessionID); err != nil {
			slog.Error("could not advance job", "job", job.ID, "err", err)
		}
	}
	return nil
}

// CreateJob validates a spec, computes the first fire time and stores the
// job. Both the HTTP API and the schedule builtin go through it, so a job
// the model created and a job a human created are indistinguishable
// afterwards and there is exactly one place that decides what a valid
// schedule is.
func CreateJob(ctx context.Context, st *store.Store, spec, prompt string, now time.Time) (store.Job, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return store.Job{}, ErrPromptRequired
	}
	sched, err := Parse(spec)
	if err != nil {
		return store.Job{}, err
	}
	next := sched.Next(now.UTC())
	if next.IsZero() {
		return store.Job{}, ErrNoFutureRun
	}
	job := store.Job{
		Kind: sched.Kind(), Spec: strings.TrimSpace(spec), Prompt: prompt,
		Enabled: true, NextRun: next,
	}
	id, err := st.CreateJob(ctx, job)
	if err != nil {
		return store.Job{}, err
	}
	job.ID = id
	return job, nil
}

// Run ticks until ctx is cancelled. It ticks once immediately, which is how
// a run missed while the daemon was down fires on the next start.
func (s *Scheduler) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 30 * time.Second
	}
	if err := s.Tick(ctx); err != nil {
		slog.Error("scheduler tick failed", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				slog.Error("scheduler tick failed", "err", err)
			}
		}
	}
}
```

- [ ] **Step 8: Run the scheduler tests**

Run: `go test -tags sqlite_fts5 ./internal/scheduler/ -race -v`
Expected: PASS.

- [ ] **Step 9: Add the job API and the daemon's Runner**

Create `internal/daemon/jobs.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

type JobJSON struct {
	ID            int64     `json:"id"`
	Kind          string    `json:"kind"`
	Spec          string    `json:"spec"`
	Prompt        string    `json:"prompt"`
	Enabled       bool      `json:"enabled"`
	NextRun       time.Time `json:"next_run"`
	LastRun       time.Time `json:"last_run,omitempty"`
	LastSessionID string    `json:"last_session_id,omitempty"`
}

func toJobJSON(j store.Job) JobJSON {
	return JobJSON{
		ID: j.ID, Kind: j.Kind, Spec: j.Spec, Prompt: j.Prompt, Enabled: j.Enabled,
		NextRun: j.NextRun, LastRun: j.LastRun, LastSessionID: j.LastSessionID,
	}
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.store.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list jobs: %v", err)
		return
	}
	out := make([]JobJSON, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobJSON(j))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Spec   string `json:"spec"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	job, err := scheduler.CreateJob(r.Context(), s.store, body.Spec, body.Prompt, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeJSON(w, http.StatusCreated, toJobJSON(job))
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "job id must be a number: %v", err)
		return
	}
	if err := s.store.SetJobEnabled(r.Context(), id, false); err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// StartJob implements scheduler.Runner. A job always opens a FRESH session,
// so a recurring job never accumulates one unbounded thread, and the policy
// engine sees the turn exactly as it sees a human's.
func (s *Server) StartJob(ctx context.Context, job store.Job) (string, error) {
	title := job.Prompt
	if len(title) > 60 {
		title = title[:60]
	}
	sessionID, err := s.store.CreateSession(ctx, title)
	if err != nil {
		return "", err
	}
	if !s.hub.Begin(sessionID) {
		return sessionID, errSessionBusy
	}
	if err := s.startTurn(sessionID, job.Prompt, "job"); err != nil {
		s.hub.End(sessionID)
		return sessionID, err
	}
	return sessionID, nil
}
```

Add the sentinel errors at the bottom of `internal/daemon/server.go`:

```go
var errSessionBusy = errors.New("the freshly created session already has a turn running")
```

and the routes in `Handler()`:

```go
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
```

- [ ] **Step 10: Test the job API**

Append to `internal/daemon/api_test.go`:

```go
func TestJobAPICreateListCancel(t *testing.T) {
	_, ts := newTestServer(t)

	res := postJSON(t, ts.URL+"/api/jobs", map[string]string{
		"spec": "0 9 * * *", "prompt": "morning briefing",
	})
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create job status = %d, want 201", res.StatusCode)
	}
	var created JobJSON
	json.NewDecoder(res.Body).Decode(&created)
	if created.Kind != "cron" || created.NextRun.IsZero() {
		t.Fatalf("created job = %+v, want kind cron and a computed next_run", created)
	}

	bad := postJSON(t, ts.URL+"/api/jobs", map[string]string{"spec": "nonsense", "prompt": "x"})
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid spec status = %d, want 400", bad.StatusCode)
	}

	noPrompt := postJSON(t, ts.URL+"/api/jobs", map[string]string{"spec": "0 9 * * *", "prompt": "  "})
	defer noPrompt.Body.Close()
	if noPrompt.StatusCode != http.StatusBadRequest {
		t.Errorf("empty prompt status = %d, want 400", noPrompt.StatusCode)
	}

	req, _ := http.NewRequest("DELETE", ts.URL+"/api/jobs/"+strconv.FormatInt(created.ID, 10), nil)
	del, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer del.Body.Close()
	if del.StatusCode != http.StatusOK {
		t.Fatalf("cancel status = %d, want 200", del.StatusCode)
	}

	listRes, _ := http.Get(ts.URL + "/api/jobs")
	defer listRes.Body.Close()
	var jobs []JobJSON
	json.NewDecoder(listRes.Body).Decode(&jobs)
	if len(jobs) != 1 || jobs[0].Enabled {
		t.Errorf("jobs after cancel = %+v, want the one job disabled", jobs)
	}
}
```

Add `"strconv"` to `api_test.go`'s imports.

- [ ] **Step 11: Run everything and commit**

Run: `make vet && make test`
Expected: PASS.

```bash
git add internal/scheduler internal/daemon
git commit -m "feat(scheduler): cron and one-shot jobs firing into fresh sessions"
```

---

### Task 6: The schedule builtin

The spec's tool table gives the model `schedule` with create, list and cancel. It goes through the same registry and the same guard as every other tool, so `schedule.*` in the `ask` list means the model cannot silently give itself a recurring wake-up.

**Files:**
- Create: `internal/tool/schedule/schedule.go`, `internal/tool/schedule/schedule_test.go`
- Modify: `cmd/spore/wire.go`

**Interfaces:**
- Consumes: `tool.Tool` (Plan 2), `store.Store` job CRUD (Task 1), `daemon.CreateJob` (Task 5), `scheduler.Parse` (Task 5).
- Produces: `schedule.New(st *store.Store) []tool.Tool` registering `schedule_create`, `schedule_list`, `schedule_cancel`.

- [ ] **Step 1: Write the failing test**

Create `internal/tool/schedule/schedule_test.go`:

```go
package schedule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

func newTools(t *testing.T) (map[string]tool.Tool, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := map[string]tool.Tool{}
	for _, tl := range New(st) {
		m[tl.Name()] = tl
	}
	return m, st
}

func TestScheduleCreateStoresAJob(t *testing.T) {
	ctx := context.Background()
	tools, st := newTools(t)

	out, err := tools["schedule_create"].Call(ctx, json.RawMessage(
		`{"spec":"0 9 * * 1-5","prompt":"weekday briefing"}`))
	if err != nil {
		t.Fatalf("schedule_create: %v", err)
	}
	if !strings.Contains(out, "weekday briefing") {
		t.Errorf("result %q does not describe the job it created", out)
	}

	jobs, err := st.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("stored %d jobs, want 1", len(jobs))
	}
	if jobs[0].Kind != "cron" || jobs[0].NextRun.IsZero() || !jobs[0].Enabled {
		t.Errorf("stored job = %+v", jobs[0])
	}
}

func TestScheduleCreateRejectsABadSpec(t *testing.T) {
	tools, st := newTools(t)
	if _, err := tools["schedule_create"].Call(context.Background(),
		json.RawMessage(`{"spec":"every tuesday-ish","prompt":"x"}`)); err == nil {
		t.Fatal("schedule_create accepted an unparseable spec")
	}
	jobs, _ := st.ListJobs(context.Background())
	if len(jobs) != 0 {
		t.Errorf("a rejected spec still stored %d jobs", len(jobs))
	}
}

func TestScheduleListAndCancel(t *testing.T) {
	ctx := context.Background()
	tools, st := newTools(t)
	tools["schedule_create"].Call(ctx, json.RawMessage(`{"spec":"0 9 * * *","prompt":"daily"}`))

	listed, err := tools["schedule_list"].Call(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("schedule_list: %v", err)
	}
	if !strings.Contains(listed, "daily") || !strings.Contains(listed, "0 9 * * *") {
		t.Errorf("listing %q does not show the job", listed)
	}

	jobs, _ := st.ListJobs(ctx)
	id := jobs[0].ID
	if _, err := tools["schedule_cancel"].Call(ctx,
		json.RawMessage(`{"id":`+strconv.FormatInt(id, 10)+`}`)); err != nil {
		t.Fatalf("schedule_cancel: %v", err)
	}
	jobs, _ = st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("schedule_cancel left the job enabled")
	}

	if _, err := tools["schedule_cancel"].Call(ctx, json.RawMessage(`{"id":999}`)); err == nil {
		t.Error("cancelling a job that does not exist reported success")
	}
}

func TestOnlyListIsReadOnly(t *testing.T) {
	tools, _ := newTools(t)
	if !tools["schedule_list"].ReadOnly() {
		t.Error("schedule_list should be read-only")
	}
	for _, name := range []string{"schedule_create", "schedule_cancel"} {
		if tools[name].ReadOnly() {
			t.Errorf("%s claims to be read-only; it mutates the jobs table", name)
		}
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/tool/schedule/ -v`
Expected: FAIL — no such package.

- [ ] **Step 3: Write the tool**

Create `internal/tool/schedule/schedule.go`:

```go
// Package schedule exposes the jobs table to the model as three tools. It
// shares one validation path with the HTTP API so a job the model creates
// and a job a human creates are indistinguishable afterwards.
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

// New returns the schedule builtins. They are ask-gated by the default
// policy: a model that can silently give itself a recurring wake-up is a
// model that can work around any per-turn limit.
func New(st *store.Store) []tool.Tool {
	return []tool.Tool{
		createTool{st: st},
		listTool{st: st},
		cancelTool{st: st},
	}
}

type createTool struct{ st *store.Store }

func (createTool) Name() string { return "schedule_create" }
func (createTool) Description() string {
	return "Schedule a prompt to run later. spec is either a five-field cron expression " +
		"(minute hour day-of-month month day-of-week, UTC) for a repeating job, or an " +
		"RFC3339 instant such as 2026-12-25T09:00:00Z for a one-off. Each run starts a " +
		"NEW session; it cannot post into this one."
}
func (createTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "spec": {"type": "string", "description": "cron expression or RFC3339 instant, UTC"},
    "prompt": {"type": "string", "description": "the prompt to run at each firing"}
  },
  "required": ["spec", "prompt"]
}`)
}
func (createTool) ReadOnly() bool { return false }

func (c createTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Spec   string `json:"spec"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	// scheduler.CreateJob is the one place that decides what a valid
	// schedule is; the HTTP API calls exactly the same function.
	job, err := scheduler.CreateJob(ctx, c.st, in.Spec, in.Prompt, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("job %d created (%s %q) — %q first runs at %s",
		job.ID, job.Kind, job.Spec, job.Prompt, job.NextRun.Format(time.RFC3339)), nil
}

type listTool struct{ st *store.Store }

func (listTool) Name() string { return "schedule_list" }
func (listTool) Description() string {
	return "List scheduled jobs with their id, schedule, prompt and next run time."
}
func (listTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (listTool) ReadOnly() bool { return true }

func (l listTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	jobs, err := l.st.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "no scheduled jobs", nil
	}
	var b strings.Builder
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "cancelled"
		}
		fmt.Fprintf(&b, "%d\t%s\t%s\t%s\tnext %s\t%s\n",
			j.ID, state, j.Kind, j.Spec, j.NextRun.Format(time.RFC3339), j.Prompt)
	}
	return b.String(), nil
}

type cancelTool struct{ st *store.Store }

func (cancelTool) Name() string        { return "schedule_cancel" }
func (cancelTool) Description() string { return "Cancel a scheduled job by id." }
func (cancelTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "integer", "description": "the job id from schedule_list"}},
  "required": ["id"]
}`)
}
func (cancelTool) ReadOnly() bool { return false }

func (c cancelTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if err := c.st.SetJobEnabled(ctx, in.ID, false); err != nil {
		return "", err
	}
	return fmt.Sprintf("job %d cancelled", in.ID), nil
}
```

- [ ] **Step 4: Run the tool tests**

Run: `go test -tags sqlite_fts5 ./internal/tool/schedule/ -v`
Expected: PASS.

- [ ] **Step 5: Register it and gate it**

In `cmd/spore/wire.go`, inside `buildTools`, after the `web.New` line:

```go
	tools = append(tools, schedule.New(st)...)
```

with the import `"github.com/codered/spore/internal/tool/schedule"`.

In `internal/config/config.go`, add the schedule tools to the default ask list so a fresh install gates them:

```go
			Ask:             []string{"fs_write", "fs_edit", "shell_exec", "schedule_*", "mcp__*"},
```

- [ ] **Step 6: Confirm the gate**

Run:
```bash
make build
./spore policy check schedule_create '{"spec":"* * * * *","prompt":"x"}'
```
Expected: the decision line reads `ask`, matched by the `schedule_*` rule. (This needs a config at `~/.spore/config.toml`; use `-config` with a scratch file if there is none.)

- [ ] **Step 7: Commit**

```bash
make vet && make test
git add internal/tool/schedule internal/config cmd/spore
git commit -m "feat(tool): schedule_create, schedule_list and schedule_cancel"
```

---

### Task 7: The embedded web UI

Server-rendered HTML plus vanilla JS over the SSE stream, with no build step (a spec requirement, not a preference). Scope is fixed by the spec and this task implements exactly it: session list, transcript with collapsible tool calls, inline approval prompts, per-turn model and cost. `go:embed` cannot reach out of a package directory, so the assets live in a tiny root package `web` that `internal/daemon` imports — which also matches the tree in spec section 2.

**Files:**
- Create: `web/embed.go`, `web/index.html`, `web/style.css`, `web/app.js`
- Create: `internal/daemon/web.go`, `internal/daemon/web_test.go`
- Modify: `internal/daemon/server.go` (routes)

**Interfaces:**
- Consumes: the JSON API from Tasks 3–5 and the `WireEvent` types from Task 2.
- Produces: `web.FS embed.FS`; routes `GET /`, `GET /static/{file}`.

- [ ] **Step 1: Write the failing web test**

Create `internal/daemon/web_test.go`:

```go
package daemon

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIndexRenders(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	body, _ := io.ReadAll(res.Body)
	page := string(body)
	for _, want := range []string{"<title>spore</title>", `id="transcript"`, "/static/app.js", "/static/style.css"} {
		if !strings.Contains(page, want) {
			t.Errorf("index page is missing %q", want)
		}
	}
}

func TestStaticAssetsAreEmbedded(t *testing.T) {
	_, ts := newTestServer(t)
	for _, tc := range []struct{ path, contentType, needle string }{
		{"/static/app.js", "javascript", "EventSource"},
		{"/static/style.css", "css", "#transcript"},
	} {
		res, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200", tc.path, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, tc.contentType) {
			t.Errorf("%s content type = %q, want something containing %q", tc.path, ct, tc.contentType)
		}
		if !strings.Contains(string(body), tc.needle) {
			t.Errorf("%s does not contain %q; is the file actually embedded?", tc.path, tc.needle)
		}
	}
}

// The binary must work with no internet: an asset pulled from a CDN would
// leave the UI blank on an offline machine, which is the deployment target.
func TestUIReferencesNoExternalResources(t *testing.T) {
	_, ts := newTestServer(t)
	for _, path := range []string{"/", "/static/app.js", "/static/style.css"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		for _, bad := range []string{"http://", "https://", "//cdn.", "unpkg", "jsdelivr", "googleapis"} {
			if strings.Contains(string(body), bad) {
				t.Errorf("%s references an external resource (%q)", path, bad)
			}
		}
	}
}

func TestUnknownStaticFileIs404(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/static/../server.go")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		t.Error("a path traversal out of the embedded assets returned 200")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'TestIndex|TestStatic|TestUI|TestUnknownStatic' -v`
Expected: FAIL — 404 on `/`.

- [ ] **Step 3: Create the embed package**

Create `web/embed.go`:

```go
// Package web holds spore's UI assets. They are embedded into the binary, so
// there is no build step, no Node toolchain and nothing to install: the
// single file that gets scp'd to a server carries its own front end.
//
// The package exists at the repository root rather than inside
// internal/daemon because go:embed cannot reach outside its own directory,
// and because spec section 2 puts web/ here.
package web

import "embed"

//go:embed index.html style.css app.js
var FS embed.FS
```

- [ ] **Step 4: Write the page**

Create `web/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>spore</title>
<link rel="stylesheet" href="/static/style.css">
</head>
<body>
<aside id="sidebar">
  <header>
    <h1>spore</h1>
    <button id="new-session" type="button">new session</button>
  </header>
  <nav id="sessions" aria-label="sessions"></nav>
  <section id="jobs-panel">
    <h2>scheduled</h2>
    <ul id="jobs"></ul>
  </section>
</aside>

<main>
  <div id="transcript" aria-live="polite"></div>
  <div id="approvals"></div>
  <form id="composer" autocomplete="off">
    <textarea id="input" rows="3" placeholder="message spore… (enter to send, shift+enter for a newline)"></textarea>
    <button type="submit" id="send">send</button>
  </form>
  <div id="status"></div>
</main>

<script src="/static/app.js"></script>
</body>
</html>
```

Create `web/style.css`:

```css
:root {
  --bg: #14161a;
  --panel: #1b1e24;
  --line: #2a2f38;
  --text: #dfe3ea;
  --dim: #8b93a1;
  --accent: #7aa2f7;
  --warn: #e0af68;
  --error: #f7768e;
  color-scheme: dark;
}

* { box-sizing: border-box; }

body {
  margin: 0;
  display: grid;
  grid-template-columns: 260px 1fr;
  height: 100vh;
  background: var(--bg);
  color: var(--text);
  font: 14px/1.5 ui-monospace, SFMono-Regular, Menlo, monospace;
}

#sidebar {
  background: var(--panel);
  border-right: 1px solid var(--line);
  display: flex;
  flex-direction: column;
  overflow-y: auto;
}

#sidebar header { padding: 12px; border-bottom: 1px solid var(--line); }
#sidebar h1 { font-size: 15px; margin: 0 0 8px; letter-spacing: 0.08em; }
#sidebar h2 { font-size: 11px; color: var(--dim); text-transform: uppercase; margin: 12px 12px 4px; }

button {
  background: transparent;
  border: 1px solid var(--line);
  color: var(--text);
  padding: 4px 10px;
  border-radius: 4px;
  cursor: pointer;
  font: inherit;
}
button:hover { border-color: var(--accent); color: var(--accent); }

#sessions a {
  display: block;
  padding: 8px 12px;
  color: var(--dim);
  text-decoration: none;
  border-bottom: 1px solid var(--line);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
#sessions a:hover { color: var(--text); }
#sessions a.active { color: var(--accent); border-left: 2px solid var(--accent); }

#jobs { list-style: none; margin: 0; padding: 0 12px 12px; color: var(--dim); font-size: 12px; }
#jobs li { padding: 4px 0; border-bottom: 1px solid var(--line); }
#jobs .cancelled { text-decoration: line-through; opacity: 0.5; }

main { display: flex; flex-direction: column; min-width: 0; }

#transcript { flex: 1; overflow-y: auto; padding: 16px; }

.msg { margin-bottom: 14px; white-space: pre-wrap; word-wrap: break-word; }
.msg .role { color: var(--dim); font-size: 11px; text-transform: uppercase; letter-spacing: 0.08em; }
.msg.user .body { color: var(--text); }
.msg.assistant .body { color: var(--text); }
.msg .footer { color: var(--dim); font-size: 11px; margin-top: 4px; }

details.tool {
  border: 1px solid var(--line);
  border-radius: 4px;
  margin: 6px 0;
  padding: 4px 8px;
  background: var(--panel);
}
details.tool summary { cursor: pointer; color: var(--accent); font-size: 12px; }
details.tool.error summary { color: var(--error); }
details.tool pre { margin: 6px 0 0; white-space: pre-wrap; word-wrap: break-word; color: var(--dim); font-size: 12px; }

#approvals { padding: 0 16px; }
.approval {
  border: 1px solid var(--warn);
  border-radius: 4px;
  padding: 10px;
  margin-bottom: 10px;
}
.approval h3 { margin: 0 0 4px; font-size: 13px; color: var(--warn); }
.approval pre { margin: 4px 0; color: var(--dim); font-size: 12px; white-space: pre-wrap; }
.approval .buttons { display: flex; gap: 6px; margin-top: 8px; flex-wrap: wrap; }

#composer { display: flex; gap: 8px; padding: 12px 16px; border-top: 1px solid var(--line); }
#input {
  flex: 1;
  background: var(--panel);
  border: 1px solid var(--line);
  border-radius: 4px;
  color: var(--text);
  padding: 8px;
  font: inherit;
  resize: vertical;
}
#input:focus { outline: none; border-color: var(--accent); }

#status { padding: 0 16px 10px; color: var(--dim); font-size: 12px; min-height: 18px; }
#status.error { color: var(--error); }
```

- [ ] **Step 5: Write the client script**

Create `web/app.js`:

```js
"use strict";

// spore's whole front end. No framework, no build step: the daemon serves
// this file straight out of the binary.

let sessionID = null;
let stream = null;
// The assistant message currently being streamed, so text deltas append to
// one node instead of creating one per delta.
let liveMessage = null;

const el = (id) => document.getElementById(id);

function setStatus(text, isError) {
  const node = el("status");
  node.textContent = text || "";
  node.className = isError ? "error" : "";
}

async function api(method, path, body) {
  const opts = { method, headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  const text = await res.text();
  let payload = null;
  if (text) {
    try { payload = JSON.parse(text); } catch (e) { payload = null; }
  }
  if (!res.ok) {
    const msg = payload && payload.error ? payload.error : res.status + " " + res.statusText;
    throw new Error(msg);
  }
  return payload;
}

// ---------- rendering ----------

function messageNode(role) {
  const wrap = document.createElement("div");
  wrap.className = "msg " + role;
  const label = document.createElement("div");
  label.className = "role";
  label.textContent = role;
  const body = document.createElement("div");
  body.className = "body";
  wrap.appendChild(label);
  wrap.appendChild(body);
  el("transcript").appendChild(wrap);
  scrollDown();
  return wrap;
}

function scrollDown() {
  const t = el("transcript");
  t.scrollTop = t.scrollHeight;
}

function appendToolCall(parent, tool, args) {
  const d = document.createElement("details");
  d.className = "tool";
  const s = document.createElement("summary");
  s.textContent = "→ " + tool;
  const pre = document.createElement("pre");
  pre.textContent = args || "";
  d.appendChild(s);
  d.appendChild(pre);
  parent.appendChild(d);
  scrollDown();
  return d;
}

function appendToolResult(parent, content, isError, truncated) {
  const d = document.createElement("details");
  d.className = isError ? "tool error" : "tool";
  const s = document.createElement("summary");
  s.textContent = (isError ? "← error" : "← result") +
    " (" + (content || "").length + " bytes" + (truncated ? ", truncated" : "") + ")";
  const pre = document.createElement("pre");
  pre.textContent = content || "";
  d.appendChild(s);
  d.appendChild(pre);
  parent.appendChild(d);
  scrollDown();
}

function renderTranscript(tr) {
  const t = el("transcript");
  t.textContent = "";
  liveMessage = null;
  for (const m of tr.messages) {
    const node = messageNode(m.role);
    const body = node.querySelector(".body");
    for (const b of m.blocks) {
      if (b.type === "text") {
        body.appendChild(document.createTextNode(b.text || ""));
      } else if (b.type === "tool_use") {
        appendToolCall(body, b.name, b.input ? JSON.stringify(b.input) : "");
      } else if (b.type === "tool_result") {
        appendToolResult(body, b.content, b.is_error, b.truncated);
      }
    }
    if (m.model) {
      const f = document.createElement("div");
      f.className = "footer";
      f.textContent = m.model + " · " + m.tokens_in + " in / " + m.tokens_out + " out" +
        (m.cost_usd ? " · $" + m.cost_usd.toFixed(4) : "");
      node.appendChild(f);
    }
  }
  setStatus(tr.running ? "a turn is running…" : "");
}

// ---------- approvals ----------

function renderApproval(ev) {
  if (document.getElementById("approval-" + ev.pending_id)) return;
  const box = document.createElement("div");
  box.className = "approval";
  box.id = "approval-" + ev.pending_id;

  const h = document.createElement("h3");
  h.textContent = "spore wants to run " + ev.tool;
  box.appendChild(h);

  if (ev.rule) {
    const why = document.createElement("div");
    why.textContent = "matched policy rule " + ev.rule;
    box.appendChild(why);
  }
  const pre = document.createElement("pre");
  pre.textContent = ev.args || "";
  box.appendChild(pre);

  const buttons = document.createElement("div");
  buttons.className = "buttons";
  const options = [
    ["allow once", true, "once"],
    ["deny", false, "once"],
    // "session" approves the TOOL for the rest of the session, not these
    // arguments. Say so on the button; a vaguer label would understate it.
    ["allow " + ev.tool + " for this session", true, "session"],
  ];
  if (ev.pattern) {
    options.push(["always allow " + ev.pattern, true, "pattern"]);
  }
  for (const [label, allow, scope] of options) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = label;
    b.addEventListener("click", async () => {
      for (const other of buttons.querySelectorAll("button")) other.disabled = true;
      try {
        await api("POST", "/api/sessions/" + sessionID + "/approvals/" + ev.pending_id,
          { allow: allow, scope: scope });
      } catch (err) {
        setStatus("could not answer: " + err.message, true);
        for (const other of buttons.querySelectorAll("button")) other.disabled = false;
      }
    });
    buttons.appendChild(b);
  }
  box.appendChild(buttons);
  el("approvals").appendChild(box);
}

function clearApproval(pendingID) {
  const node = document.getElementById("approval-" + pendingID);
  if (node) node.remove();
}

// ---------- streaming ----------

function handleEvent(ev) {
  switch (ev.type) {
    case "text":
      if (!liveMessage) liveMessage = messageNode("assistant");
      liveMessage.querySelector(".body").appendChild(document.createTextNode(ev.text));
      scrollDown();
      break;
    case "tool_call":
      if (!liveMessage) liveMessage = messageNode("assistant");
      appendToolCall(liveMessage.querySelector(".body"), ev.tool, ev.args);
      break;
    case "tool_result":
      if (!liveMessage) liveMessage = messageNode("assistant");
      appendToolResult(liveMessage.querySelector(".body"), ev.content, ev.is_error, ev.truncated);
      break;
    case "turn_done": {
      const target = liveMessage || messageNode("assistant");
      const f = document.createElement("div");
      f.className = "footer";
      f.textContent = ev.model + " · " + (ev.tokens_in || 0) + " in / " + (ev.tokens_out || 0) + " out" +
        (ev.cost_usd ? " · $" + ev.cost_usd.toFixed(4) : "");
      target.appendChild(f);
      liveMessage = null;
      setStatus("");
      loadSessions();
      break;
    }
    case "error":
      setStatus(ev.error, true);
      liveMessage = null;
      break;
    case "approval":
      renderApproval(ev);
      break;
    case "resolved":
      clearApproval(ev.pending_id);
      setStatus("");
      break;
  }
}

function attach(id) {
  if (stream) stream.close();
  stream = new EventSource("/api/sessions/" + id + "/events");
  stream.onmessage = (e) => {
    try {
      handleEvent(JSON.parse(e.data));
    } catch (err) {
      setStatus("bad event from the daemon: " + err.message, true);
    }
  };
  // The browser reconnects an EventSource on its own; say so rather than
  // showing an error the user cannot act on.
  stream.onerror = () => setStatus("reconnecting…");
}

async function openSession(id) {
  sessionID = id;
  el("approvals").textContent = "";
  renderTranscript(await api("GET", "/api/sessions/" + id));
  attach(id);
  for (const a of document.querySelectorAll("#sessions a")) {
    a.classList.toggle("active", a.dataset.id === id);
  }
}

// ---------- sidebar ----------

async function loadSessions() {
  const sessions = await api("GET", "/api/sessions");
  const nav = el("sessions");
  nav.textContent = "";
  for (const s of sessions || []) {
    const a = document.createElement("a");
    a.href = "#" + s.id;
    a.dataset.id = s.id;
    a.textContent = s.title || s.id;
    a.classList.toggle("active", s.id === sessionID);
    a.addEventListener("click", (e) => {
      e.preventDefault();
      openSession(s.id).catch((err) => setStatus(err.message, true));
    });
    nav.appendChild(a);
  }
  return sessions || [];
}

async function loadJobs() {
  const jobs = await api("GET", "/api/jobs");
  const list = el("jobs");
  list.textContent = "";
  for (const j of jobs || []) {
    const li = document.createElement("li");
    if (!j.enabled) li.className = "cancelled";
    li.textContent = j.spec + " — " + j.prompt;
    list.appendChild(li);
  }
}

// ---------- composer ----------

async function send() {
  const input = el("input");
  const text = input.value.trim();
  if (!text || !sessionID) return;
  input.value = "";
  const node = messageNode("user");
  node.querySelector(".body").textContent = text;
  setStatus("thinking…");
  try {
    await api("POST", "/api/sessions/" + sessionID + "/messages", { text: text });
  } catch (err) {
    setStatus(err.message, true);
  }
}

async function main() {
  el("composer").addEventListener("submit", (e) => {
    e.preventDefault();
    send();
  });
  el("input").addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  });
  el("new-session").addEventListener("click", async () => {
    try {
      const s = await api("POST", "/api/sessions", { title: "web" });
      await loadSessions();
      await openSession(s.id);
    } catch (err) {
      setStatus(err.message, true);
    }
  });

  try {
    const sessions = await loadSessions();
    await loadJobs();
    const wanted = location.hash.replace("#", "");
    const target = wanted || (sessions[0] && sessions[0].id);
    if (target) await openSession(target);
  } catch (err) {
    setStatus(err.message, true);
  }
}

main();
```

- [ ] **Step 6: Serve it**

Create `internal/daemon/web.go`:

```go
package daemon

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/codered/spore/web"
)

// indexTemplate is parsed once at start. The page is mostly static — the
// live parts arrive over SSE — but it goes through html/template so anything
// interpolated into it later is escaped by construction rather than by
// remembering to.
var indexTemplate = template.Must(template.ParseFS(web.FS, "index.html"))

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// ServeMux's "/" pattern matches everything unmatched; only the root is
	// the UI, so anything else is a genuine 404 rather than a silent index.
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "no such path %s", r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "render index: %v", err)
	}
}

// handleStatic serves the embedded assets. It reads from the embed.FS by
// exact file name rather than through http.FileServer, so there is no
// directory listing and no path to traverse.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType := ""
	switch {
	case strings.HasSuffix(name, ".js"):
		contentType = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		contentType = "text/css; charset=utf-8"
	default:
		writeError(w, http.StatusNotFound, "no such asset %s", name)
		return
	}
	body, err := web.FS.ReadFile(name)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such asset %s", name)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(body)
}
```

Add the routes in `Handler()`:

```go
	mux.HandleFunc("GET /static/{file}", s.handleStatic)
	mux.HandleFunc("GET /", s.handleIndex)
```

- [ ] **Step 7: Run the web tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -race -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
make vet && make test
git add web internal/daemon
git commit -m "feat(web): embedded server-rendered UI over the SSE stream"
```

---

### Task 8: `spore serve`, the pidfile, and process control

The daemon needs a way to be started, found and stopped — by a human and, in Task 9, by the CLI's auto-start. The pidfile is the whole of that mechanism, and the thing it must get right is not mistaking a stale file for a live process.

**Files:**
- Create: `internal/daemon/pidfile.go`, `internal/daemon/pidfile_test.go`
- Create: `cmd/spore/serve.go`
- Modify: `cmd/spore/wire.go`, `cmd/spore/main.go`

**Interfaces:**
- Consumes: `daemon.New`, `(*Server).Run`, `scheduler.New`, `(*Scheduler).Run`, `config.Config`.
- Produces:
  - `daemon.WritePidFile(path string) error`
  - `daemon.ReadPidFile(path string) (int, error)`
  - `daemon.PidAlive(pid int) bool`
  - `daemon.RemovePidFile(path string) error`
  - `main.buildServer(cfg *config.Config, st *store.Store) (*daemon.Server, error)`
  - `main.cmdServe(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error`

- [ ] **Step 1: Write the failing pidfile test**

Create `internal/daemon/pidfile_test.go`:

```go
package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPidFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	if _, err := ReadPidFile(path); err == nil {
		t.Error("reading a missing pidfile succeeded")
	}
	if err := WritePidFile(path); err != nil {
		t.Fatalf("WritePidFile: %v", err)
	}
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want this process %d", pid, os.Getpid())
	}
	if !PidAlive(pid) {
		t.Error("PidAlive says this very process is dead")
	}
	if err := RemovePidFile(path); err != nil {
		t.Fatalf("RemovePidFile: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("RemovePidFile left the file behind")
	}
	// Removing a file that is already gone is not an error: shutdown paths
	// call it more than once.
	if err := RemovePidFile(path); err != nil {
		t.Errorf("second RemovePidFile: %v", err)
	}
}

func TestAStalePidFileIsNotALiveDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	// A pid that cannot be running: the kernel reserves 0, and pid 1 is not
	// ours to signal, so use a very high one that no test machine will have.
	if err := os.WriteFile(path, []byte("4194303\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := ReadPidFile(path)
	if err != nil {
		t.Fatalf("ReadPidFile: %v", err)
	}
	if PidAlive(pid) {
		t.Skip("pid 4194303 happens to exist on this machine")
	}
	// This is the case that matters: a crashed daemon leaves its pidfile, and
	// treating that as "already running" would make the CLI refuse to start
	// forever.
	if _, err := ReadPidFile(filepath.Join(t.TempDir(), "absent.pid")); err == nil {
		t.Error("a missing pidfile read as present")
	}
}

func TestReadPidFileRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.pid")
	if err := os.WriteFile(path, []byte("not a pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPidFile(path); err == nil {
		t.Error("ReadPidFile accepted a file that is not a number")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'TestPid|TestAStale|TestReadPid' -v`
Expected: FAIL — `WritePidFile` undefined.

- [ ] **Step 3: Write the pidfile**

Create `internal/daemon/pidfile.go`:

```go
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// WritePidFile records this process's id so a later CLI invocation can find
// the daemon it started.
func WritePidFile(path string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create pidfile dir: %w", err)
		}
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600)
}

func ReadPidFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("pidfile %s does not contain a pid: %w", path, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pidfile %s contains an impossible pid %d", path, pid)
	}
	return pid, nil
}

// PidAlive reports whether a process with that id exists. Signal 0 performs
// the permission and existence checks without delivering anything, which is
// the portable way to ask. A daemon that crashed leaves its pidfile behind,
// and treating that as "already running" would wedge the CLI permanently.
func PidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means the process exists and belongs to someone else.
	return os.IsPermission(err)
}

// RemovePidFile deletes the file, tolerating its absence: shutdown paths run
// more than once.
func RemovePidFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
```

Note for the implementer: `syscall.Signal(0)` compiles on Windows but always fails there, so `PidAlive` reports false. That is acceptable for now — spec section 9 targets linux for deployment — and the CI build for windows is not part of this plan. Do not add build tags for it.

- [ ] **Step 4: Run the pidfile tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'TestPid|TestAStale|TestReadPid' -v`
Expected: PASS.

- [ ] **Step 5: Write the serve command**

Create `cmd/spore/serve.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// cmdServe runs the daemon, or reports on and stops one that is already
// running. Flags: --status, --stop, --detach.
func cmdServe(ctx context.Context, cfg *config.Config, st *store.Store, args []string) error {
	status, stop, detach := false, false, false
	for _, a := range args {
		switch a {
		case "--status", "-status":
			status = true
		case "--stop", "-stop":
			stop = true
		case "--detach", "-detach":
			detach = true
		default:
			return fmt.Errorf("unknown serve flag %q (want --status, --stop or --detach)", a)
		}
	}

	pidPath := cfg.PidPath()
	switch {
	case status:
		pid, err := daemon.ReadPidFile(pidPath)
		if err != nil || !daemon.PidAlive(pid) {
			fmt.Println("not running")
			return nil
		}
		fmt.Printf("running (pid %d) on %s\n", pid, cfg.Daemon.Addr)
		return nil
	case stop:
		pid, err := daemon.ReadPidFile(pidPath)
		if err != nil {
			fmt.Println("not running")
			return nil
		}
		if !daemon.PidAlive(pid) {
			fmt.Println("not running (removing a stale pidfile)")
			return daemon.RemovePidFile(pidPath)
		}
		proc, err := os.FindProcess(pid)
		if err != nil {
			return err
		}
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return fmt.Errorf("signal %d: %w", pid, err)
		}
		fmt.Printf("stopping (pid %d)\n", pid)
		return nil
	}

	// A second daemon on the same database and port helps nobody.
	if pid, err := daemon.ReadPidFile(pidPath); err == nil && daemon.PidAlive(pid) {
		return fmt.Errorf("spore is already running (pid %d); use spore serve --stop first", pid)
	}
	if err := daemon.WritePidFile(pidPath); err != nil {
		return err
	}
	defer daemon.RemovePidFile(pidPath)

	srv, err := buildServer(cfg, st)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	sched := scheduler.New(st, srv, nil)
	go sched.Run(ctx, time.Duration(cfg.Daemon.TickSeconds)*time.Second)

	if !detach {
		fmt.Printf("spore listening on http://%s\n", cfg.Daemon.Addr)
	}
	return srv.Run(ctx, cfg.Daemon.Addr)
}
```

- [ ] **Step 6: Build the server in wire.go**

In `cmd/spore/wire.go`, `buildTools` currently hard-codes the terminal approver. Give it the approver as a parameter so the daemon can pass its broker, and add `buildServer`:

```go
// buildTools assembles the registry, the policy engine and the guard that
// wraps them. The approver is a parameter because the CLI asks on a terminal
// and the daemon asks over SSE; everything else about the leash is identical.
func buildTools(cfg *config.Config, st *store.Store, approver policy.Approver) (*policy.Guard, error) {
```

with the body's `approver := terminalApprover{...}` line deleted, and `buildAgent` taking and forwarding the same parameter:

```go
func buildAgent(cfg *config.Config, st *store.Store, approver policy.Approver) (*agent.Agent, error) {
```

```go
	tools, err := buildTools(cfg, st, approver)
```

Then append `buildServer`:

```go
// buildServer wires the daemon. The ordering here is load-bearing: the guard
// needs the daemon's approver, and the daemon needs the guard, so the server
// is constructed first with no agent, and the agent is attached once its
// tools have been built around the server's broker.
func buildServer(cfg *config.Config, st *store.Store) (*daemon.Server, error) {
	srv := daemon.New(daemon.Options{Store: st, Cfg: cfg})
	a, err := buildAgent(cfg, st, srv.Approver())
	if err != nil {
		return nil, err
	}
	guard, ok := a.Tools.(*policy.Guard)
	if !ok {
		return nil, fmt.Errorf("internal: agent tools are %T, want *policy.Guard", a.Tools)
	}
	srv.Attach(a, guard)
	return srv, nil
}
```

and in `internal/daemon/server.go`, the setter that closes the cycle:

```go
// Attach supplies the agent and guard after construction. The daemon owns
// the approver the guard is built with, so the two cannot both be passed to
// New; this is the seam where the cycle is broken.
func (s *Server) Attach(a *agent.Agent, g *policy.Guard) {
	s.agent = a
	s.guard = g
}
```

Update the two existing call sites in `cmd/spore/once.go` and `cmd/spore/chat.go` to `buildAgent(cfg, st, terminalApprover{lines: stdinLines, out: os.Stdout})` — Task 9 replaces both of these files entirely, so this is only to keep the tree compiling between commits.

- [ ] **Step 7: Register the command**

In `cmd/spore/main.go`, add to the switch:

```go
	case "serve":
		return cmdServe(ctx, cfg, st, args[1:])
```

and to the usage string, after the `chat` line:

```
  spore serve                  run the daemon (HTTP API + web UI + scheduler)
  spore serve --status         report whether a daemon is running
  spore serve --stop           stop a running daemon
```

- [ ] **Step 8: Exercise it by hand**

```bash
make build
./spore serve --status          # not running
./spore serve &                 # listening on http://127.0.0.1:7777
sleep 1
./spore serve --status          # running (pid N)
curl -s http://127.0.0.1:7777/healthz
curl -s http://127.0.0.1:7777/api/sessions
./spore serve --stop
sleep 1
./spore serve --status          # not running
```
Expected: exactly those transitions, and the pidfile gone at the end. Open `http://127.0.0.1:7777/` in a browser while it runs and confirm the UI loads, lists sessions and accepts a message.

- [ ] **Step 9: Commit**

```bash
make vet && make test
git add internal/daemon cmd/spore
git commit -m "feat(cli): spore serve with a pidfile, --status and --stop"
```

---

### Task 9: `chat` and `once` as thin clients

The spec's reason for this is that only one code path stays warm: whatever the web UI exercises is what the CLI exercises. The decision recorded in spec section 8 is that a CLI finding nothing listening starts a **detached** daemon that outlives the invocation, so scheduled jobs keep firing and a suspended approval survives the terminal closing.

**Files:**
- Create: `cmd/spore/client.go`, `cmd/spore/autostart.go`, `cmd/spore/autostart_test.go`
- Modify: `cmd/spore/chat.go`, `cmd/spore/once.go`, `cmd/spore/main.go`

**Interfaces:**
- Consumes: `daemon.WireEvent` and its type constants (Task 2), the HTTP API (Tasks 3–5), `daemon.ReadPidFile`/`PidAlive` (Task 8), `terminalApprover` (Plan 2).
- Produces:
  - `main.newClient(addr string) *client`
  - `(*client).health(ctx) error`, `createSession(ctx, title) (string, error)`, `send(ctx, sessionID, text) error`, `resolve(ctx, sessionID string, pendingID int64, ans policy.Answer) error`
  - `(*client).stream(ctx context.Context, sessionID string, fn func(daemon.WireEvent) error) error`
  - `main.ensureDaemon(ctx context.Context, cfg *config.Config) (*client, error)`
  - `main.waitForHealth(ctx context.Context, c *client, timeout time.Duration) error`

- [ ] **Step 1: Write the client**

Create `cmd/spore/client.go`:

```go
package main

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

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// client talks to the daemon over the same HTTP API the web UI uses. Keeping
// the CLI on that one path is the point: a bug in the API is a bug both
// clients hit, so neither drifts into being the only tested one.
type client struct {
	base string
	// short is for request/response calls; stream deliberately uses a client
	// with no timeout, because an SSE connection is meant to stay open.
	short  *http.Client
	stream *http.Client
}

func newClient(addr string) *client {
	return &client{
		base:   "http://" + addr,
		short:  &http.Client{Timeout: 30 * time.Second},
		stream: &http.Client{},
	}
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.short.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if out != nil {
		return json.Unmarshal(payload, out)
	}
	return nil
}

func (c *client) health(ctx context.Context) error {
	return c.do(ctx, "GET", "/healthz", nil, nil)
}

func (c *client) createSession(ctx context.Context, title string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/api/sessions", map[string]string{"title": title}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *client) send(ctx context.Context, sessionID, text string) error {
	return c.do(ctx, "POST", "/api/sessions/"+sessionID+"/messages",
		map[string]string{"text": text}, nil)
}

func (c *client) resolve(ctx context.Context, sessionID string, pendingID int64, ans policy.Answer) error {
	return c.do(ctx, "POST",
		fmt.Sprintf("/api/sessions/%s/approvals/%d", sessionID, pendingID),
		map[string]any{"allow": ans.Allow, "scope": string(ans.Scope)}, nil)
}

// streamFrom reads the session's server-sent events until ctx is cancelled,
// the connection drops, or fn returns an error. It closes `connected` once
// the stream is actually open, which is what lets a caller post a message
// knowing that no event published in the meantime can be missed — attaching
// in a goroutine and posting immediately would race.
func (c *client) streamFrom(ctx context.Context, sessionID string, connected chan<- struct{}, fn func(daemon.WireEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/sessions/"+sessionID+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("attach to session %s: %s", sessionID, res.Status)
	}
	if connected != nil {
		close(connected)
	}

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue // blank separators and ": ping" heartbeats
		}
		var ev daemon.WireEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue // a malformed frame is not worth dropping the session for
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (c *client) stream(ctx context.Context, sessionID string, fn func(daemon.WireEvent) error) error {
	return c.streamFrom(ctx, sessionID, nil, fn)
}
```

- [ ] **Step 2: Write the failing auto-start test**

Create `cmd/spore/autostart_test.go`:

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

func TestEnsureDaemonUsesAnAlreadyRunningOne(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			hits++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Daemon.Addr = strings.TrimPrefix(ts.URL, "http://")

	c, err := ensureDaemon(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if c == nil {
		t.Fatal("ensureDaemon returned no client")
	}
	if hits == 0 {
		t.Error("ensureDaemon never probed /healthz")
	}
	// Nothing was spawned, so no pidfile should have appeared.
	if _, err := daemonPid(cfg); err == nil {
		t.Error("ensureDaemon wrote a pidfile for a daemon it did not start")
	}
}

func TestWaitForHealthGivesUp(t *testing.T) {
	// Port 1 on loopback refuses instantly, so this exercises the give-up
	// path without waiting on a real timeout.
	c := newClient("127.0.0.1:1")
	start := time.Now()
	err := waitForHealth(context.Background(), c, 300*time.Millisecond)
	if err == nil {
		t.Fatal("waitForHealth reported a healthy daemon on a closed port")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waitForHealth took %v; it should give up after its timeout", elapsed)
	}
}

func TestWaitForHealthReturnsAsSoonAsItIsUp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	c := newClient(strings.TrimPrefix(ts.URL, "http://"))
	if err := waitForHealth(context.Background(), c, 5*time.Second); err != nil {
		t.Fatalf("waitForHealth: %v", err)
	}
}
```

- [ ] **Step 3: Run it and watch it fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run 'TestEnsureDaemon|TestWaitForHealth' -v`
Expected: FAIL — `ensureDaemon` undefined.

- [ ] **Step 4: Write auto-start**

Create `cmd/spore/autostart.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

// startupTimeout bounds how long a CLI waits for a daemon it just spawned.
const startupTimeout = 15 * time.Second

// daemonPid reports the pid of a live daemon, or an error when none is
// running. A stale pidfile from a crashed daemon is an error, not a pid.
func daemonPid(cfg *config.Config) (int, error) {
	pid, err := daemon.ReadPidFile(cfg.PidPath())
	if err != nil {
		return 0, err
	}
	if !daemon.PidAlive(pid) {
		return 0, fmt.Errorf("pidfile names pid %d, which is not running", pid)
	}
	return pid, nil
}

// ensureDaemon returns a client for a running daemon, starting one if
// nothing answers. The daemon it starts is DETACHED and outlives this
// process: scheduled jobs must keep firing and an approval suspended
// mid-turn must still be answerable after the terminal closes.
func ensureDaemon(ctx context.Context, cfg *config.Config) (*client, error) {
	c := newClient(cfg.Daemon.Addr)

	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	err := c.health(probe)
	cancel()
	if err == nil {
		return c, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the spore binary to start a daemon: %w", err)
	}
	logPath := filepath.Join(cfg.DataDir, "daemon.log")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe, "-config", cfg.Path, "serve", "--detach")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Stdin = nil
	detach(cmd) // put it in its own process group; see proc_*.go
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start the daemon: %w", err)
	}
	// Release rather than Wait: this process must be able to exit while the
	// daemon keeps running.
	if err := cmd.Process.Release(); err != nil {
		return nil, fmt.Errorf("release the daemon process: %w", err)
	}

	fmt.Fprintf(os.Stderr, "spore: started a daemon on %s (log: %s)\n", cfg.Daemon.Addr, logPath)
	if err := waitForHealth(ctx, c, startupTimeout); err != nil {
		return nil, fmt.Errorf("%w\n%s", err, tailFile(logPath, 2048))
	}
	return c, nil
}

// waitForHealth polls until the daemon answers or the timeout passes.
func waitForHealth(ctx context.Context, c *client, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		err := c.health(probe)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no daemon answered on %s within %s: %w", c.base, timeout, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// tailFile returns the last n bytes of a file, for putting a failed daemon's
// own error message in front of the user instead of "it did not start".
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	if info.Size() > n {
		if _, err := f.Seek(-n, 2); err != nil {
			return ""
		}
	}
	var b strings.Builder
	buf := make([]byte, n)
	read, _ := f.Read(buf)
	b.Write(buf[:read])
	return b.String()
}
```

Create `cmd/spore/proc_unix.go`:

```go
//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own process group so a ctrl-c in the
// terminal that started it does not take it down with the CLI.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
```

Create `cmd/spore/proc_windows.go`:

```go
//go:build windows

package main

import "os/exec"

// detach is a no-op on Windows; the daemon is not a supported deployment
// target there (spec section 9), and the CLI still works against one started
// by hand.
func detach(cmd *exec.Cmd) {}
```

- [ ] **Step 5: Run the auto-start tests**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run 'TestEnsureDaemon|TestWaitForHealth' -v`
Expected: PASS.

- [ ] **Step 6: Rewrite `once`**

Replace `cmd/spore/once.go` entirely:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// printEvent renders one wire event on the terminal. It is the CLI's whole
// display layer, shared by once and chat.
func printEvent(ev daemon.WireEvent, showCost bool) {
	switch ev.Type {
	case daemon.WireText:
		fmt.Print(ev.Text)
	case daemon.WireToolCall:
		fmt.Printf("\n  → %s %s\n", ev.Tool, ev.Args)
	case daemon.WireToolResult:
		mark := "←"
		if ev.IsError {
			mark = "✗"
		}
		fmt.Printf("  %s %d bytes\n", mark, len(ev.Content))
	case daemon.WireTurnDone:
		cost := ""
		if showCost {
			cost = fmt.Sprintf(" · $%.4f", ev.CostUSD)
		}
		fmt.Printf("\n\n[%s · %d in / %d out%s]\n", ev.Model, ev.TokensIn, ev.TokensOut, cost)
	case daemon.WireError:
		fmt.Fprintf(os.Stderr, "\nturn failed: %s\n", ev.Error)
	}
}

// errTurnFinished ends a stream cleanly once the turn it was watching is
// over. stream returns it, and callers treat it as success.
var errTurnFinished = fmt.Errorf("turn finished")

// approve renders an approval on the terminal and posts the answer back.
// Errors are reported and swallowed: the guard denies on its own timeout, so
// a failure to answer degrades to a denial rather than a hung turn.
func approve(ctx context.Context, c *client, ap terminalApprover, sessionID string, ev daemon.WireEvent) {
	ans, err := ap.Ask(ctx, policy.Ask{
		SessionID: sessionID, Tool: ev.Tool, Args: []byte(ev.Args),
		Rule: ev.Rule, PendingID: ev.PendingID, Pattern: ev.Pattern,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\napproval not answered: %v\n", err)
		return
	}
	if err := c.resolve(ctx, sessionID, ev.PendingID, ans); err != nil {
		fmt.Fprintf(os.Stderr, "\ncould not send the answer: %v\n", err)
	}
}

func cmdOnce(ctx context.Context, cfg *config.Config, prompt string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	sessionID, err := c.createSession(ctx, prompt)
	if err != nil {
		return err
	}

	ap := terminalApprover{lines: stdinLines, out: os.Stdout}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Attach BEFORE posting the message, and wait for the connection rather
	// than for the goroutine to start: an event published between the two
	// would otherwise be missed, and for a one-shot turn that can be the
	// whole reply.
	connected := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		errc <- c.streamFrom(streamCtx, sessionID, connected, func(ev daemon.WireEvent) error {
			printEvent(ev, cfg.ShowCost)
			switch ev.Type {
			case daemon.WireApproval:
				approve(streamCtx, c, ap, sessionID, ev)
			case daemon.WireTurnDone, daemon.WireError:
				return errTurnFinished
			}
			return nil
		})
	}()
	select {
	case <-connected:
	case err := <-errc:
		return fmt.Errorf("attach to the session: %w", err)
	}

	if err := c.send(ctx, sessionID, prompt); err != nil {
		return err
	}
	if err := <-errc; err != nil && err != errTurnFinished {
		return err
	}
	return nil
}
```

- [ ] **Step 7: Rewrite `chat`**

Replace `cmd/spore/chat.go` entirely:

```go
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

func cmdChat(ctx context.Context, cfg *config.Config, sessionID string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	if sessionID == "" {
		if sessionID, err = c.createSession(ctx, "chat"); err != nil {
			return err
		}
	}
	fmt.Printf("session %s — ctrl-d to exit\n", sessionID)
	fmt.Printf("web UI: http://%s/#%s\n", cfg.Daemon.Addr, sessionID)

	ap := terminalApprover{lines: stdinLines, out: os.Stdout}
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// turnDone carries one signal per finished turn. The prompt loop waits on
	// it, which is also what keeps the approval prompt and the input prompt
	// from reading stdin at the same time: while a turn is running, the loop
	// is blocked here and the stream goroutine is the only reader.
	turnDone := make(chan struct{}, 1)
	connected := make(chan struct{})
	streamErr := make(chan error, 1)
	go func() {
		streamErr <- c.streamFrom(streamCtx, sessionID, connected, func(ev daemon.WireEvent) error {
			printEvent(ev, cfg.ShowCost)
			switch ev.Type {
			case daemon.WireApproval:
				approve(streamCtx, c, ap, sessionID, ev)
			case daemon.WireTurnDone, daemon.WireError:
				select {
				case turnDone <- struct{}{}:
				default:
				}
			}
			return nil
		})
	}()
	select {
	case <-connected:
	case err := <-streamErr:
		return fmt.Errorf("attach to the session: %w", err)
	}

	sc := stdinLines
	for {
		fmt.Print("\n> ")
		if !sc.Scan() {
			return sc.Err()
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := c.send(ctx, sessionID, line); err != nil {
			fmt.Fprintln(os.Stderr, "send failed:", err)
			continue
		}
		select {
		case <-turnDone:
		case err := <-streamErr:
			return fmt.Errorf("lost the event stream: %w", err)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
```

- [ ] **Step 8: Update main**

In `cmd/spore/main.go`, the `once` and `chat` cases no longer take a store:

```go
	case "once":
		if len(args) < 2 {
			return fmt.Errorf("once needs a prompt")
		}
		return cmdOnce(ctx, cfg, args[1])
	case "chat":
		id := ""
		if len(args) > 1 {
			id = args[1]
		}
		return cmdChat(ctx, cfg, id)
```

`main.go` still opens the store for `serve`, `session` and `policy`, so leave that alone. The `-config` flag is already parsed there and `cfg.Path` is already populated by `Load`, which is what `ensureDaemon` passes to the daemon it spawns.

- [ ] **Step 9: Exercise the whole loop by hand**

```bash
make build
./spore serve --stop || true
./spore once 'say hello and nothing else'
./spore serve --status      # running — the daemon outlived the CLI
./spore chat
```
Expected: `once` reports it started a daemon, prints the reply, and exits leaving the daemon up. `chat` attaches to the *same* daemon with no second start. Open the web UI and confirm the `chat` session appears there and that a message typed in the browser shows up in the terminal's stream. Then `./spore serve --stop`.

- [ ] **Step 10: Commit**

```bash
make vet && make test
git add cmd/spore
git commit -m "feat(cli): chat and once as thin clients that auto-start a detached daemon"
```

---

### Task 10: The end-to-end test and the documentation

The spec asks for one end-to-end test that boots the daemon with the fake provider and drives it over HTTP. This task builds it with the **real** guard, the **real** registry and **real** builtins behind it — which also closes the integration gap the Plan 2 final review identified and deferred to this plan: until now every layer has only ever been tested against a fake of its neighbour.

**Files:**
- Create: `internal/daemon/e2e_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything built in Tasks 1–9.
- Produces: no new production code.

- [ ] **Step 1: Write the end-to-end test**

Create `internal/daemon/e2e_test.go`:

```go
package daemon

import (
	"bufio"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/fs"
)

// newFullServer wires the real thing: real store, real registry, real fs
// builtins, real policy guard, real agent. Only the model is scripted. Every
// other test in this package fakes at least one neighbour; this one fakes
// none, which is the only way the seams between them get exercised.
func newFullServer(t *testing.T, turns ...provider.ScriptTurn) (*Server, *httptest.Server, string) {
	t.Helper()
	workspace := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.DefaultModel = "script/fake"
	cfg.DataDir = t.TempDir()
	cfg.Policy.Workspace = workspace
	cfg.Policy.Allow = []string{"fs_read", "fs_list"}
	cfg.Policy.Ask = []string{"fs_write"}
	cfg.Policy.Default = "deny"

	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}

	srv := New(Options{Store: st, Cfg: cfg})

	reg := tool.NewRegistry(cfg.Policy.MaxOutput)
	for _, tl := range fs.New(workspace, cfg.Policy.MaxOutput) {
		if err := reg.Register(tl); err != nil {
			t.Fatalf("register %s: %v", tl.Name(), err)
		}
	}
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	guard := policy.NewGuard(reg, engine, srv.Approver(), st, nil)
	srv.Attach(agent.New(st, preg, rt, cfg, guard), guard)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts, workspace
}

func attachStream(t *testing.T, ts *httptest.Server, sessionID string) *bufio.Reader {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/"+sessionID+"/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return bufio.NewReader(res.Body)
}

func newSession(t *testing.T, ts *httptest.Server, title string) string {
	t.Helper()
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": title})
	defer res.Body.Close()
	var s SessionJSON
	json.NewDecoder(res.Body).Decode(&s)
	if s.ID == "" {
		t.Fatal("no session created")
	}
	return s.ID
}

// An allowed tool call runs for real: the model asks to read a file, the
// guard allows it, the fs builtin reads it, and the content comes back.
func TestEndToEndAllowedToolCallReachesTheRealBuiltin(t *testing.T) {
	_, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
			Input: json.RawMessage(`{"path":"note.txt"}`),
		}}},
		provider.ScriptTurn{Text: "the note says hello"},
	)
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello from disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	id := newSession(t, ts, "e2e")
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read note.txt"})
	post.Body.Close()

	events := readSSE(t, r, 4) // tool_call, tool_result, text, turn_done
	if events[0].Type != WireToolCall || events[0].Tool != "fs_read" {
		t.Fatalf("first event = %+v, want the fs_read call", events[0])
	}
	if events[1].Type != WireToolResult {
		t.Fatalf("second event = %+v, want a tool result", events[1])
	}
	if events[1].IsError {
		t.Fatalf("the tool call failed: %s", events[1].Content)
	}
	if !strings.Contains(events[1].Content, "hello from disk") {
		t.Errorf("tool result = %q, want the file's real content", events[1].Content)
	}
	if events[2].Type != WireText || events[2].Text != "the note says hello" {
		t.Errorf("third event = %+v", events[2])
	}
	if events[3].Type != WireTurnDone {
		t.Errorf("fourth event = %+v, want turn_done", events[3])
	}
}

// A denied call must come back as a tool error the model can read, and must
// never reach the filesystem — deny is absolute and is never escalated to a
// human, so no approval event may appear.
func TestEndToEndDeniedCallNeverReachesTheBuiltin(t *testing.T) {
	_, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_read",
			Input: json.RawMessage(`{"path":"/etc/passwd"}`),
		}}},
		provider.ScriptTurn{Text: "I could not read that"},
	)
	_ = workspace

	id := newSession(t, ts, "denied")
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "read /etc/passwd"})
	post.Body.Close()

	events := readSSE(t, r, 4)
	for _, ev := range events {
		if ev.Type == WireApproval {
			t.Fatal("a denied call produced an approval prompt; deny must never be escalated to a human")
		}
	}
	if !events[1].IsError {
		t.Fatalf("the out-of-workspace read was not refused: %+v", events[1])
	}
	if !strings.Contains(events[1].Content, "denied by policy") {
		t.Errorf("refusal text = %q, want it to name the policy", events[1].Content)
	}
}

// The full approval round trip: a turn suspends, the approval arrives over
// SSE, a client answers over HTTP, and the turn resumes and completes.
func TestEndToEndApprovalSuspendsAndResumesTheTurn(t *testing.T) {
	_, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"out.txt","content":"written by the agent"}`),
		}}},
		provider.ScriptTurn{Text: "done"},
	)

	id := newSession(t, ts, "approve")
	r := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "write out.txt"})
	post.Body.Close()

	events := readSSE(t, r, 2) // tool_call, approval
	var approval WireEvent
	for _, ev := range events {
		if ev.Type == WireApproval {
			approval = ev
		}
	}
	if approval.PendingID == 0 {
		t.Fatalf("no approval arrived; events were %+v", events)
	}
	if approval.Tool != "fs_write" {
		t.Errorf("approval names %q, want fs_write", approval.Tool)
	}

	// Nothing has been written yet: the turn is suspended.
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was written before the approval was answered")
	}

	answer := postJSON(t, ts.URL+"/api/sessions/"+id+"/approvals/"+
		strconv.FormatInt(approval.PendingID, 10), map[string]any{"allow": true, "scope": "once"})
	answer.Body.Close()

	rest := readSSE(t, r, 4) // resolved, tool_result, text, turn_done
	sawDone := false
	for _, ev := range rest {
		if ev.Type == WireTurnDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Fatalf("the turn did not resume after approval; got %+v", rest)
	}
	body, err := os.ReadFile(filepath.Join(workspace, "out.txt"))
	if err != nil {
		t.Fatalf("the approved write never happened: %v", err)
	}
	if string(body) != "written by the agent" {
		t.Errorf("file content = %q", body)
	}
}

// A second client attaching mid-suspension is told what is waiting, so a
// browser opened after the fact can still answer.
func TestEndToEndSecondClientSeesThePendingApproval(t *testing.T) {
	_, ts, _ := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"late.txt","content":"x"}`),
		}}},
		provider.ScriptTurn{Text: "done"},
	)

	id := newSession(t, ts, "late")
	first := attachStream(t, ts, id)
	post := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "write late.txt"})
	post.Body.Close()
	readSSE(t, first, 2) // wait until the approval exists

	// A client attaching now must be told immediately, before any deltas.
	second := attachStream(t, ts, id)
	got := readSSE(t, second, 1)
	if got[0].Type != WireApproval {
		t.Fatalf("a late client's first event was %+v, want the pending approval", got[0])
	}

	listed, err := http.Get(ts.URL + "/api/sessions/" + id + "/approvals")
	if err != nil {
		t.Fatal(err)
	}
	defer listed.Body.Close()
	var pending []WireEvent
	json.NewDecoder(listed.Body).Decode(&pending)
	if len(pending) != 1 || pending[0].Tool != "fs_write" {
		t.Errorf("GET approvals = %+v, want the one pending fs_write", pending)
	}
}

// The scheduler's callback goes through the same path as a human's message:
// a fresh session, a real turn, a real transcript.
func TestEndToEndScheduledJobOpensAFreshSession(t *testing.T) {
	srv, ts, _ := newFullServer(t, provider.ScriptTurn{Text: "briefing complete"})

	sessionID, err := srv.StartJob(t.Context(), store.Job{
		ID: 1, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing", Enabled: true,
	})
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	if sessionID == "" {
		t.Fatal("StartJob returned no session id")
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		res, err := http.Get(ts.URL + "/api/sessions/" + sessionID)
		if err != nil {
			t.Fatal(err)
		}
		var tr TranscriptJSON
		json.NewDecoder(res.Body).Decode(&tr)
		res.Body.Close()
		if len(tr.Messages) >= 2 {
			if tr.Messages[0].Role != "user" || tr.Messages[0].Blocks[0].Text != "morning briefing" {
				t.Errorf("first message = %+v, want the job's prompt", tr.Messages[0])
			}
			if tr.Session.Title != "morning briefing" {
				t.Errorf("session title = %q, want the job's prompt", tr.Session.Title)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the scheduled turn never completed; transcript has %d messages", len(tr.Messages))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
```

Add `"net/http/httptest"` and `"strconv"` to this file's imports.

- [ ] **Step 2: Run the end-to-end suite**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestEndToEnd -race -v`
Expected: PASS. `TestEndToEndDeniedCallNeverReachesTheBuiltin` is the important one — it is the only test in the codebase that proves deny holds across the whole agent-loop → guard → registry → builtin path rather than against a fake.

- [ ] **Step 3: Run everything**

Run: `make vet && make test`
Expected: PASS across all 17 packages.

- [ ] **Step 4: Update the README**

In `README.md`, add a section after the existing tools/policy material:

```markdown
## Running as a daemon

    spore serve                  # HTTP API, web UI and scheduler on 127.0.0.1:7777
    spore serve --status         # is one running?
    spore serve --stop           # stop it

`spore chat` and `spore once` are thin clients against that API — the same
path the web UI uses. If nothing is listening they start a daemon themselves
and leave it running, so scheduled jobs keep firing and an approval you have
not answered yet survives closing the terminal. Its log is at
`~/.spore/daemon.log` and its pidfile at `~/.spore/spore.pid`.

The daemon binds loopback and has no authentication: spore serves one person
on one machine. A non-loopback `addr` is rejected at load.

    [daemon]
    addr = "127.0.0.1:7777"
    tick_seconds = 30

## Web UI

`http://127.0.0.1:7777/` — session list, transcript with collapsible tool
calls, inline approval buttons, and the model and cost for each turn. It is
served out of the binary; there is no build step and nothing to install.

## Scheduled jobs

A job is a prompt plus a schedule: a five-field cron expression (UTC) or an
RFC3339 instant for a one-off. Each firing starts a **new** session, so a
recurring job never grows one unbounded thread, and policy applies to it
exactly as it does to a turn you typed — a job that trips an `ask` rule
suspends and waits for you.

    curl -s localhost:7777/api/jobs \
      -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'

The model can manage jobs itself through `schedule_create`, `schedule_list`
and `schedule_cancel`, which are in the default `ask` list.

If the daemon was down when a job was due, it fires once on the next start.
Missed runs are never backfilled.
```

- [ ] **Step 5: Commit**

```bash
make vet && make test
git add internal/daemon README.md
git commit -m "test(daemon): end-to-end coverage through the real guard, registry and builtins"
```

- [ ] **Step 6: Final verification before review**

```bash
make vet
make test
make build
./spore serve --stop || true
./spore once 'what is 2+2? answer with just the number'
./spore serve --status
./spore serve --stop
```
Expected: vet clean, every package passing, the binary building, and the manual sequence behaving as Task 9 Step 9 described. Confirm `git status` is clean before opening the branch for review.

---

## Notes for the executor

**The construction cycle in `buildServer` is deliberate.** The guard needs an approver, the daemon owns the approver, and the daemon needs the guard. `New` → `Approver()` → build tools → `Attach` is the order that breaks it. Do not "simplify" it by giving the guard a nil approver and setting it later; a guard with no approver denies every `ask`, and the failure would only show up under a real approval.

**Two paths record an approval and only one may run.** `Broker.Answer` returning true means a suspended turn took the answer and `Guard.Run` will write the audit row. Returning false means no turn is waiting and `Guard.Resolve` must write it instead. If both ever run for one suspension, the approvals table gets two rows for one human decision. The `if s.broker.Answer(...) { return }` shape in `handleResolveApproval` is what keeps them exclusive.

**Never pass a request context to `agent.Run`.** It is the single easiest way to break spec invariant 2, and `TestTurnSurvivesTheClientThatStartedItDisconnecting` is the only thing standing between that mistake and a shipped regression.
