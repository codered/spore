# Per-Session Workspace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the workspace a property of a session — recorded at creation, carried on the turn context, and honoured by the filesystem tools, the shell, the prompt's environment section and the policy engine — instead of one value per daemon.

**Architecture:** `sessions.workspace` becomes the record of truth, written at creation and read once per turn by the daemon, which puts it on the context beside the trust profile (`policy.Session`). Everything downstream — `fs_*`, `shell_exec`, `Agent.Env`, and the `path outside workspace` predicate — reads it from the context and fails closed when it is absent, so no tool holds a workspace of its own any more. `policy.workspace` stops being a location and becomes only a ceiling that a new session's root is checked against at creation.

**Tech Stack:** Go 1.22+, SQLite (`mattn/go-sqlite3`, build tag `sqlite_fts5`), `net/http` daemon, Bubble Tea CLI, vanilla-JS web UI.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` — sections 5 (Sessions), 6 (Policy engine), 8 (Daemon, Scheduler, Bridges, CLI), 9 (Configuration), 11 (stage 6).

## Global Constraints

- Every build and test command carries the FTS5 tag: `go build -tags sqlite_fts5`, `go test -tags sqlite_fts5 ./...`. `make test` already does this; never run bare `go test`.
- The repository's comment style explains *why*, not *what*. Match it: every non-obvious decision in this plan has a rationale, and that rationale belongs in the code as a comment.
- Fail closed. Where a workspace cannot be determined, the call is refused — never silently substituted with `policy.workspace`, the home directory, or the process's cwd. `policy.SessionFrom` already sets the precedent by reporting the least-trusted profile when nothing is attached.
- Deny is absolute and evaluated first; nothing in this stage may add a path that lets an approval talk past a deny rule.
- `internal/mcp` keeps pinning server subprocesses to `policy.workspace` (spec §6): one server process is shared by every session, so it cannot hold a per-session directory. Containment for MCP calls comes from the policy engine evaluating path arguments against the *calling session's* workspace, which this plan delivers for free.
- Session directories spore allocates live at `<data_dir>/sessions/<session-id>` and are created on the session's first turn, never at creation.
- Existing sessions must keep working: a row written before the column existed is backfilled with the configured ceiling, so resuming one behaves exactly as it did.

---

### Task 1: The `workspace` column

**Files:**
- Modify: `internal/store/schema.go` (the `sessions` table)
- Modify: `internal/store/store.go` (`Open`, `Session`, `CreateSession`, `ListSessions`, `Session()`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `store.Session` gains `Workspace string`.
  - `func (s *Store) CreateSession(ctx context.Context, title, workspace string) (string, error)` — an empty `workspace` means "allocate one": the row records `<data_dir>/sessions/<id>`, where `<data_dir>` is the directory holding the database file. Nothing is created on disk.
  - `func (s *Store) SessionsDir() string` — `<data_dir>/sessions`.
  - `func (s *Store) SetSessionWorkspace(ctx context.Context, id, workspace string) error` — re-roots a session; returns an error naming the id when no row matched.
  - `func (s *Store) BackfillSessionWorkspaces(ctx context.Context, root string) (int64, error)` — fills empty workspaces with `root`, returns the number of rows changed.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go`. The file's existing helper is
`openTestStore(t) *Store`, which throws away the directory it opened in; the
tests below need it, so add a second helper beside it rather than changing
the one every other test uses:

```go
// newStore is openTestStore, plus the data directory the store was opened
// in: the tests below assert on paths the store derives from it.
func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}
```

```go
func TestCreateSessionRecordsWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "titled", "/projects/thing")
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := s.Session(ctx, id)
	if err != nil || !found {
		t.Fatalf("session %s: found=%v err=%v", id, found, err)
	}
	if got.Workspace != "/projects/thing" {
		t.Fatalf("workspace = %q, want /projects/thing", got.Workspace)
	}
}

// An empty workspace is the caller saying "I have no directory of my own".
// The store allocates one under its own data directory and records it, but
// must not create it: a session that is opened and never used leaves nothing
// on disk.
func TestCreateSessionAllocatesSessionDir(t *testing.T) {
	s, dir := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, "sessions", id)
	if got.Workspace != want {
		t.Fatalf("workspace = %q, want %q", got.Workspace, want)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatalf("session directory was created at creation time: stat err = %v", err)
	}
}

func TestListSessionsCarriesWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	if _, err := s.CreateSession(ctx, "a", "/ws/a"); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListSessions(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Workspace != "/ws/a" {
		t.Fatalf("list = %+v, want one row rooted at /ws/a", list)
	}
}

func TestSetSessionWorkspace(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "/ws/old")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionWorkspace(ctx, id, "/ws/new"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := s.Session(ctx, id)
	if got.Workspace != "/ws/new" {
		t.Fatalf("workspace = %q, want /ws/new", got.Workspace)
	}
	if err := s.SetSessionWorkspace(ctx, "nosuch", "/ws/new"); err == nil {
		t.Fatal("re-rooting an unknown session should error")
	}
}

// A database written before the column existed has rows with an empty
// workspace. They are backfilled with the configured ceiling so resuming one
// behaves exactly as it did.
func TestBackfillSessionWorkspaces(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	id, err := s.CreateSession(ctx, "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a pre-migration row.
	if _, err := s.DB().ExecContext(ctx, `UPDATE sessions SET workspace = '' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	n, err := s.BackfillSessionWorkspaces(ctx, "/home/user")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("backfilled %d rows, want 1", n)
	}
	got, _, _ := s.Session(ctx, id)
	if got.Workspace != "/home/user" {
		t.Fatalf("workspace = %q, want /home/user", got.Workspace)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'Workspace|SessionDir' -v`
Expected: FAIL — `CreateSession` takes 2 arguments, `Session.Workspace` undefined, `SetSessionWorkspace` undefined.

- [ ] **Step 3: Add the column and the migration**

In `internal/store/schema.go`, the `sessions` table becomes:

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  workspace  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

`CREATE TABLE IF NOT EXISTS` does nothing to a database that already has the table, so add an explicit migration in `internal/store/store.go` next to `migrateJobs`, following its shape (inspect with `PRAGMA table_info`, act only when the column is missing):

```go
// migrateSessions adds the per-session workspace column to a database written
// before stage 6. Unlike migrateJobs this preserves every row: sessions hold
// real transcripts. The column lands empty and is filled by
// BackfillSessionWorkspaces, which needs the configured ceiling and so cannot
// run here -- Open knows nothing about config.
func migrateSessions(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return fmt.Errorf("inspect sessions table: %w", err)
	}
	defer rows.Close()
	var columns int
	hasWorkspace := false
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns++
		if name == "workspace" {
			hasWorkspace = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// No table at all: the schema statement creates the right one.
	if columns == 0 || hasWorkspace {
		return nil
	}
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN workspace TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add sessions.workspace: %w", err)
	}
	return nil
}
```

Call it in `Open`, next to `migrateJobs` and before `db.Exec(schemaSQL)`:

```go
	if err := migrateSessions(db); err != nil {
		db.Close()
		return nil, err
	}
```

- [ ] **Step 4: Teach the Store its data directory and the session methods**

In `internal/store/store.go`:

```go
type Store struct {
	db *sql.DB
	// dataDir is the directory holding the database file. It is where a
	// session with no directory of its own is rooted, so the store can
	// allocate one without knowing anything about config.
	dataDir string
}
```

`Open` returns `&Store{db: db, dataDir: filepath.Dir(path)}`.

```go
// SessionsDir is where spore allocates a workspace for a session whose
// creator has no directory of its own -- a bridge, the web UI, the scheduler.
func (s *Store) SessionsDir() string { return filepath.Join(s.dataDir, "sessions") }

// CreateSession records a session rooted at workspace. An empty workspace
// means the creator has no directory of its own, and the session is rooted at
// SessionsDir()/<id>. The directory is NOT created here: a session that is
// opened and never used must leave nothing on disk.
func (s *Store) CreateSession(ctx context.Context, title, workspace string) (string, error) {
	id := newID()
	if workspace == "" {
		workspace = filepath.Join(s.SessionsDir(), id)
	}
	now := time.Now().UTC().Format(timeFormat)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		id, title, workspace, now, now)
	if err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	return id, nil
}

// SetSessionWorkspace re-roots a session. The root is fixed at creation for
// every ordinary path; this exists for the CLI's deliberate "--workspace on a
// resume" exception, and the caller is responsible for checking the new root
// against the ceiling first.
func (s *Store) SetSessionWorkspace(ctx context.Context, id, workspace string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET workspace = ?, updated_at = ? WHERE id = ?`,
		workspace, nowString(), id)
	if err != nil {
		return fmt.Errorf("re-root session: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("no session %s", id)
	}
	return nil
}

// BackfillSessionWorkspaces roots every session written before the column
// existed at the configured ceiling, so resuming one behaves exactly as it
// did when the workspace was a single daemon-wide value.
func (s *Store) BackfillSessionWorkspaces(ctx context.Context, root string) (int64, error) {
	if root == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET workspace = ? WHERE workspace = ''`, root)
	if err != nil {
		return 0, fmt.Errorf("backfill session workspaces: %w", err)
	}
	return res.RowsAffected()
}
```

Add `workspace` to both read paths. In `ListSessions`:

```go
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, title, workspace, created_at, updated_at FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit)
	...
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Workspace, &created, &updated); err != nil {
```

and in `Session`:

```go
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, workspace, created_at, updated_at FROM sessions WHERE id = ?`, id).
		Scan(&sess.ID, &sess.Title, &sess.Workspace, &created, &updated)
```

with the struct field:

```go
type Session struct {
	ID    string
	Title string
	// Workspace is the directory this session is rooted at: the working
	// directory for its filesystem tools, its shell calls and the
	// environment section of its prompt. Fixed at creation.
	Workspace string
	CreatedAt time.Time
	UpdatedAt time.Time
}
```

- [ ] **Step 5: Fix every `CreateSession` call site to compile**

The mechanical pass — real behaviour arrives in later tasks. Pass `""` at each:
`internal/daemon/sessions.go:68`, `internal/daemon/jobs.go:88`, `internal/bridge/discord/bridge.go:235`, `internal/bridge/discord/bridge.go:276`, plus every test that calls it.

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./...`
Expected: clean.

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS, whole suite.

- [ ] **Step 7: Commit**

```bash
git add internal/store internal/daemon internal/bridge
git commit -m "store: record each session's workspace

Adds sessions.workspace with an idempotent ALTER migration, allocation of
<data_dir>/sessions/<id> for a creator with no directory of its own, a
re-root path for the CLI's --workspace, and a backfill for rows written
before the column existed.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 2: Where a new session is allowed to be rooted

**Files:**
- Create: `internal/workspace/root.go`
- Create: `internal/workspace/root_test.go`
- Modify: `internal/config/config.go` (`ProfilePolicy.Workspace`, its expansion and its ceiling check in `Load`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `policy.Inside(workspace, p string) bool` and `policy.Resolve(workspace, p string) (string, error)` from `internal/policy/path.go` (both already exist).
- Produces:
  - `config.ProfilePolicy` gains `Workspace string \`toml:"workspace"\``, `~`-expanded at load and rejected at load when it lies outside `policy.workspace`.
  - ```go
    type Request struct {
        Requested  string // what the creator asked for; "" means "allocate one"
        Ceiling    string // policy.workspace
        RemoteRoot string // policy.profile.remote.workspace; "" confines to the session directory
        Remote     bool   // the creator is on the remote trust profile
    }
    func Root(req Request) (string, error)
    ```
    `Root` returns the workspace to record, or `""` meaning "let the store allocate a session directory". It returns an error when a requested root lies outside the ceiling or is not absolute.
  - `func Allocated(sessionsDir, ws string) bool` — whether `ws` is a directory spore allocated, which is what decides whether the daemon may create it.

- [ ] **Step 1: Write the failing tests**

`internal/workspace/root_test.go`:

```go
package workspace

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootAllocatesWhenNothingRequested(t *testing.T) {
	got, err := Root(Request{Ceiling: "/home/user"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("root = %q, want \"\" (allocate a session directory)", got)
	}
}

func TestRootAcceptsADirectoryInsideTheCeiling(t *testing.T) {
	ceiling := t.TempDir()
	inside := filepath.Join(ceiling, "project")
	got, err := Root(Request{Requested: inside, Ceiling: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	if got != inside {
		t.Fatalf("root = %q, want %q", got, inside)
	}
}

// The ceiling is the whole point: a session rooted outside it is refused at
// creation rather than quietly moved.
func TestRootRefusesOutsideTheCeiling(t *testing.T) {
	ceiling := t.TempDir()
	other := t.TempDir()
	_, err := Root(Request{Requested: other, Ceiling: ceiling})
	if err == nil {
		t.Fatal("a root outside the ceiling must be refused")
	}
	if !strings.Contains(err.Error(), ceiling) {
		t.Fatalf("error %q should name the ceiling %q", err, ceiling)
	}
}

func TestRootRefusesARelativeRequest(t *testing.T) {
	if _, err := Root(Request{Requested: "project", Ceiling: t.TempDir()}); err == nil {
		t.Fatal("a relative workspace must be refused: there is no cwd to resolve it against")
	}
}

// A remote creator cannot name a directory at all. Left alone it is confined
// to its own session directory, which holds nothing but its own transcript.
func TestRemoteIgnoresTheRequestAndIsConfined(t *testing.T) {
	ceiling := t.TempDir()
	got, err := Root(Request{Requested: filepath.Join(ceiling, "secrets"), Ceiling: ceiling, Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("root = %q, want \"\" (a remote session gets its own directory)", got)
	}
}

// Setting [policy.remote] workspace is the operator saying a bridge user is
// meant to work on something real.
func TestRemoteRootIsUsedWhenConfigured(t *testing.T) {
	ceiling := t.TempDir()
	shared := filepath.Join(ceiling, "shared")
	got, err := Root(Request{Ceiling: ceiling, RemoteRoot: shared, Remote: true})
	if err != nil {
		t.Fatal(err)
	}
	if got != shared {
		t.Fatalf("root = %q, want %q", got, shared)
	}
}

func TestAllocated(t *testing.T) {
	sessions := "/data/sessions"
	if !Allocated(sessions, "/data/sessions/abc123") {
		t.Fatal("a directory under the sessions directory is spore's own")
	}
	if Allocated(sessions, "/home/user/project") {
		t.Fatal("a user directory is not spore's to create")
	}
}
```

Add to `internal/config/config_test.go`:

```go
func TestRemoteProfileWorkspaceMustBeInsideTheCeiling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[policy]\nworkspace = \"" + filepath.Join(dir, "ceiling") + "\"\n\n" +
		"[policy.profile.remote]\nworkspace = \"" + filepath.Join(dir, "elsewhere") + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("a remote workspace outside the ceiling must fail at load, not at the first tool call")
	}
}

func TestProfileWorkspaceExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "[policy]\nworkspace = \"~\"\n\n[policy.profile.remote]\nworkspace = \"~/shared\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Policy.Profiles["remote"].Workspace; got != filepath.Join(home, "shared") {
		t.Fatalf("remote workspace = %q, want %q", got, filepath.Join(home, "shared"))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/workspace/ ./internal/config/ -run 'Root|Allocated|ProfileWorkspace|RemoteProfile' -v`
Expected: FAIL — `Root` undefined, `ProfilePolicy.Workspace` undefined.

- [ ] **Step 3: Write `internal/workspace/root.go`**

```go
package workspace

import (
	"fmt"
	"path/filepath"

	"github.com/codered/spore/internal/policy"
)

// Request is everything needed to decide where a new session is rooted.
type Request struct {
	// Requested is the directory the creator asked for. Empty means the
	// creator has none -- a bridge, the web UI, the scheduler -- and the
	// session gets one of its own.
	Requested string
	// Ceiling is policy.workspace: not a location, but the bound every
	// session's root must lie within.
	Ceiling string
	// RemoteRoot is [policy.remote] workspace. Empty keeps a remote session
	// in its own directory, which holds nothing but its own transcript.
	RemoteRoot string
	// Remote reports that the creator is on the remote trust profile.
	Remote bool
}

// Root decides the workspace to record for a new session. It returns "" when
// the store should allocate a session directory, and an error when the
// requested root is not one this daemon is allowed to hand out -- refused at
// creation rather than quietly moved, so a creator learns immediately that it
// asked for something outside the ceiling.
//
// A session directory spore allocates is never checked against the ceiling:
// spore allocated it, and a ceiling naming a project directory would
// otherwise reject spore's own storage. That is why the allocation path
// returns before any containment check rather than after one.
func Root(req Request) (string, error) {
	// A remote creator does not get to name a directory: the request arrives
	// from an untrusted party, and the operator's answer to "where may a
	// bridge user work" is [policy.remote] workspace, checked at config load.
	if req.Remote {
		return req.RemoteRoot, nil
	}
	if req.Requested == "" {
		return "", nil
	}
	if !filepath.IsAbs(req.Requested) {
		return "", fmt.Errorf("workspace %q must be an absolute path: the daemon has no working directory to resolve it against", req.Requested)
	}
	abs, err := policy.Resolve(req.Requested, req.Requested)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", req.Requested, err)
	}
	if !policy.Inside(req.Ceiling, abs) {
		return "", fmt.Errorf("workspace %s is outside the configured ceiling %s (policy.workspace)", abs, req.Ceiling)
	}
	return abs, nil
}

// Allocated reports whether ws is a session directory spore allocated. It is
// what decides whether the daemon may create the directory on the session's
// first turn: spore makes its own storage, and never makes a directory a
// human named.
func Allocated(sessionsDir, ws string) bool {
	return policy.Inside(sessionsDir, ws)
}
```

Note for the implementer: `policy.Resolve(req.Requested, req.Requested)` resolves symlinks on an already-absolute path; the first argument is only consulted for relative paths, which the guard above has already excluded.

- [ ] **Step 4: Add the config field and its checks**

In `internal/config/config.go`, extend `ProfilePolicy`:

```go
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
```

In `Load`, immediately after `policy.Workspace` is defaulted and `~`-expanded (around `config.go:433-438`), and after the `cfg.Policy.Profiles == nil` guard, add:

```go
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
```

`internal/config` must not import `internal/policy` (policy imports config; the cycle is real). Add the containment check locally in `config.go`, next to `expandHome`:

```go
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
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/workspace/ ./internal/config/ -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/workspace internal/config
git commit -m "workspace: decide and bound where a session may be rooted

policy.workspace becomes a ceiling: workspace.Root refuses a requested root
outside it at creation, a remote creator never names its own directory, and
[policy.profile.remote] workspace is expanded and bounded at config load.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 3: The workspace travels on the turn context

**Files:**
- Modify: `internal/policy/guard.go` (`WithSession`, `SessionFrom`, `Guard.Run`)
- Modify: `internal/policy/engine.go` (`Evaluate`)
- Modify: `internal/daemon/sessions.go:167` (the one production caller)
- Modify: `cmd/spore/policy.go` (`cmdPolicyCheck`)
- Test: `internal/policy/guard_test.go`, `internal/policy/engine_test.go`

**Interfaces:**
- Consumes: `store.Session.Workspace` (Task 1).
- Produces:
  - ```go
    // Session is what a turn runs under: who it belongs to, how far it is
    // trusted, and where it is rooted.
    type Session struct {
        ID        string
        Profile   Profile
        Workspace string
    }
    func WithSession(ctx context.Context, s Session) context.Context
    func SessionFrom(ctx context.Context) Session // Profile is ProfileRemote when nothing is attached
    func WorkspaceFrom(ctx context.Context) string
    ```
  - `func (e *Engine) Evaluate(s Session, c Call) Result` — evaluates `path outside workspace` against `s.Workspace`, falling back to the configured ceiling only when the session names none (which is the `spore policy check` path, not a turn).

- [ ] **Step 1: Write the failing tests**

Add to `internal/policy/engine_test.go`:

```go
// One engine serves every session, so the bound is the calling session's own
// workspace and not the ceiling: a daemon serving a session in /ws/a and one
// in /ws/b applies the right bound to each.
func TestEvaluateUsesTheCallingSessionsWorkspace(t *testing.T) {
	cfg := config.PolicyConfig{
		Workspace:       "/ws",
		Default:         "allow",
		ApprovalTimeout: "1m",
		Deny:            []string{"fs_*(path outside workspace)"},
	}
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	call := Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"/ws/a/notes.md"}`)}

	inA := e.Evaluate(Session{ID: "s1", Profile: ProfileLocal, Workspace: "/ws/a"}, call)
	if inA.Decision != DecisionAllow {
		t.Fatalf("inside its own workspace: %+v, want allow", inA)
	}
	inB := e.Evaluate(Session{ID: "s2", Profile: ProfileLocal, Workspace: "/ws/b"}, call)
	if inB.Decision != DecisionDeny {
		t.Fatalf("a sibling session's path: %+v, want deny", inB)
	}
}

// spore policy check has no session. Falling back to the ceiling keeps it
// answering the question it always answered.
func TestEvaluateFallsBackToTheCeilingWithNoSessionWorkspace(t *testing.T) {
	cfg := config.PolicyConfig{
		Workspace:       "/ws",
		Default:         "allow",
		ApprovalTimeout: "1m",
		Deny:            []string{"fs_*(path outside workspace)"},
	}
	e, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := e.Evaluate(Session{Profile: ProfileLocal},
		Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"/ws/anything.md"}`)})
	if res.Decision != DecisionAllow {
		t.Fatalf("%+v, want allow: with no session the ceiling is the bound", res)
	}
}
```

Add to `internal/policy/guard_test.go`:

```go
func TestSessionFromCarriesWorkspace(t *testing.T) {
	ctx := WithSession(context.Background(), Session{ID: "s1", Profile: ProfileLocal, Workspace: "/ws/a"})
	got := SessionFrom(ctx)
	if got.ID != "s1" || got.Profile != ProfileLocal || got.Workspace != "/ws/a" {
		t.Fatalf("session = %+v", got)
	}
	if WorkspaceFrom(ctx) != "/ws/a" {
		t.Fatalf("WorkspaceFrom = %q", WorkspaceFrom(ctx))
	}
}

// Nothing attached still fails toward the strictest ruleset, and names no
// directory at all rather than a default one.
func TestSessionFromEmptyContext(t *testing.T) {
	got := SessionFrom(context.Background())
	if got.Profile != ProfileRemote {
		t.Fatalf("profile = %q, want %q", got.Profile, ProfileRemote)
	}
	if got.Workspace != "" {
		t.Fatalf("workspace = %q, want empty", got.Workspace)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run 'Session|Evaluate' -v`
Expected: FAIL — `Session` undefined, `Evaluate` takes a `Profile`.

- [ ] **Step 3: Replace the context carrier**

In `internal/policy/guard.go`, replace the `sessionInfo`/`WithSession`/`SessionFrom` block with:

```go
type sessionKey struct{}

// Session is what a turn runs under: which session it belongs to, how far its
// client is trusted, and the directory it is rooted at. One guard and one
// engine serve every session in the daemon, so all three travel on the
// context rather than being held by anything.
type Session struct {
	ID        string
	Profile   Profile
	Workspace string
}

// WithSession attaches the session a turn is running under.
func WithSession(ctx context.Context, s Session) context.Context {
	return context.WithValue(ctx, sessionKey{}, s)
}

// SessionFrom returns the session on the context. When nothing is attached it
// reports the LEAST trusted profile, not the most: a caller that forgot
// WithSession must fail toward the strictest ruleset, never toward the one
// that allows the most. The workspace is left empty for the same reason --
// naming a default directory here would hand an unattributed call a place to
// work, and the tools refuse a call with no workspace instead.
func SessionFrom(ctx context.Context) Session {
	s, ok := ctx.Value(sessionKey{}).(Session)
	if !ok || s.Profile == "" {
		s.Profile = ProfileRemote
	}
	return s
}

// WorkspaceFrom is the shorthand the filesystem tools and the shell use. An
// empty string means no session is attached, and every caller treats that as
// a refusal.
func WorkspaceFrom(ctx context.Context) string { return SessionFrom(ctx).Workspace }
```

In `Guard.Run`, the head becomes:

```go
	sess := SessionFrom(ctx)
	// Checked BEFORE evaluation, not on the ask branch: a call with no session
	// cannot be audited, attributed, or routed to a human, so it must not run
	// at all -- not even a tool policy would allow outright. Leaving this until
	// the ask branch would let an allowed call through unattributed.
	if sess.ID == "" {
		sporetrace.RecordPolicy(ctx, "deny", "policy.no-session")
		return denied(call.ID, "refusing %s: no session on the context, so the call cannot be audited", call.Name)
	}

	c := Call{Tool: call.Name, Args: call.Input}
	res := g.engine.Evaluate(sess, c)
```

Then replace the remaining `sessionID` uses in that function with `sess.ID` and `profile` with `sess.Profile` (the `AddPendingCall` write and the `Ask`/`RecordApproval`/`SessionDecision` calls).

- [ ] **Step 4: Evaluate against the session's workspace**

In `internal/policy/engine.go`:

```go
// Evaluate resolves one call for one session. Deny rules are checked first
// and win outright; then allow and ask rules in configured order; then the
// profile default. Path predicates are evaluated against the CALLING
// session's workspace, so one daemon serving a local session in a project and
// a bridge session in its own directory applies the right bound to each.
func (e *Engine) Evaluate(s Session, c Call) Result {
	env := e.env
	if s.Workspace != "" {
		env.Workspace = s.Workspace
	}
	// ... the existing malformed-arguments gate, unchanged ...
	rs, ok := e.profiles[s.Profile]
	if !ok {
		rs = e.base
	}
	for _, r := range rs.deny {
		if r.Match(c, env) {
			return Result{Decision: DecisionDeny, Rule: r.Raw}
		}
	}
	for _, r := range rs.allowAndAsk {
		if r.Match(c, env) {
			return Result{Decision: r.Decision, Rule: r.Raw}
		}
	}
	return Result{Decision: rs.fallback, Rule: "policy.default"}
}
```

Keep `e.env` and `Engine.Workspace()` as they are: the ceiling is still what `spore policy check` evaluates against when no session names a directory.

- [ ] **Step 5: Fix the two production call sites**

`internal/daemon/sessions.go:167` — for now (Task 6 supplies the real workspace):

```go
	ctx := policy.WithSession(s.base, policy.Session{ID: sessionID, Profile: profile})
```

`cmd/spore/policy.go` — wherever it calls `Evaluate(policy.Profile(profile), call)`:

```go
	res := engine.Evaluate(policy.Session{ID: "policy-check", Profile: policy.Profile(profile), Workspace: workspaceFlag}, call)
```

where `workspaceFlag` is `""` for now; the `-workspace` flag lands in Task 7. Update the test files under `internal/policy` and `internal/daemon` that call the old signatures.

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/policy internal/daemon cmd/spore
git commit -m "policy: carry the workspace on the turn context

WithSession takes a policy.Session (id, profile, workspace) and Evaluate
resolves 'path outside workspace' against the calling session's own root,
falling back to the ceiling only where there is no session at all.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 4: The filesystem tools and the shell read the context

**Files:**
- Modify: `internal/tool/fs/fs.go` (`New`, `base`, `resolve`, `rel`, and every `Call` that uses them)
- Modify: `internal/tool/shell/shell.go` (`New`, `execTool`, `Call`)
- Modify: `cmd/spore/wire.go:45-47`
- Test: `internal/tool/fs/fs_test.go`, `internal/tool/shell/shell_test.go`

**Interfaces:**
- Consumes: `policy.WorkspaceFrom(ctx) string` (Task 3).
- Produces:
  - `func fs.New(maxBytes int) []tool.Tool` — no workspace argument. Each call resolves paths against the context's workspace.
  - `func shell.New(defaultTimeout time.Duration, maxOutput int) tool.Tool` — no workspace argument; `cmd.Dir` is the context's workspace.
  - Both return an error mentioning "no workspace" when the context carries none.

- [ ] **Step 1: Write the failing tests**

Rework the helper at the top of `internal/tool/fs/fs_test.go` and add the new cases:

```go
func tools(t *testing.T) (map[string]tool.Tool, string) {
	t.Helper()
	ws := t.TempDir()
	m := map[string]tool.Tool{}
	for _, tl := range New(1 << 20) {
		m[tl.Name()] = tl
	}
	return m, ws
}

// ctxFor is what every test in this file calls instead of context.Background:
// a filesystem tool has no workspace of its own any more, so a call without a
// session on the context is not a call it can run.
func ctxFor(ws string) context.Context {
	return policy.WithSession(context.Background(),
		policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: ws})
}
```

`run` and `runErr` gain a workspace parameter and use `ctxFor(ws)`; update their call sites in this file mechanically.

```go
// Two sessions, two roots, one set of tools: the same relative path must
// resolve differently for each.
func TestRelativePathFollowsTheSessionWorkspace(t *testing.T) {
	m, a := tools(t)
	b := t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "note.txt"), []byte("from a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "note.txt"), []byte("from b"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotA := run(t, m["fs_read"], a, map[string]any{"path": "note.txt"})
	gotB := run(t, m["fs_read"], b, map[string]any{"path": "note.txt"})
	if !strings.Contains(gotA, "from a") || !strings.Contains(gotB, "from b") {
		t.Fatalf("a = %q, b = %q: each session must read its own file", gotA, gotB)
	}
}

// Fail closed. A tool reached without a session cannot know where to work,
// and must not fall back to a configured default.
func TestFSRefusesWithoutASessionWorkspace(t *testing.T) {
	m, _ := tools(t)
	raw, _ := json.Marshal(map[string]any{"path": "note.txt"})
	if _, err := m["fs_read"].Call(context.Background(), raw); err == nil {
		t.Fatal("a call with no workspace on the context must be refused")
	}
}
```

In `internal/tool/shell/shell_test.go`, the equivalent pair:

```go
func TestShellRunsInTheSessionWorkspace(t *testing.T) {
	ws := t.TempDir()
	tl := New(5*time.Second, 1<<16)
	ctx := policy.WithSession(context.Background(),
		policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: ws})
	out, err := tl.Call(ctx, json.RawMessage(`{"command":"pwd"}`))
	if err != nil {
		t.Fatal(err)
	}
	// macOS resolves /var to /private/var, so compare resolved paths.
	want, _ := filepath.EvalSymlinks(ws)
	got, _ := filepath.EvalSymlinks(strings.TrimSpace(firstLine(out)))
	if got != want {
		t.Fatalf("pwd = %q, want %q", got, want)
	}
}

func TestShellRefusesWithoutASessionWorkspace(t *testing.T) {
	tl := New(5*time.Second, 1<<16)
	if _, err := tl.Call(context.Background(), json.RawMessage(`{"command":"pwd"}`)); err == nil {
		t.Fatal("a shell call with no workspace on the context must be refused")
	}
}
```

If `firstLine` does not exist in that package, add it:

```go
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/tool/... -v 2>&1 | tail -20`
Expected: FAIL — `New` takes 2 arguments (fs) / 3 arguments (shell).

- [ ] **Step 3: Rewrite the fs base**

In `internal/tool/fs/fs.go`:

```go
// New builds the six filesystem tools. They hold no workspace: one daemon
// serves sessions rooted in different directories, so the root arrives on the
// context of each call and a call carrying none is refused.
func New(maxBytes int) []tool.Tool {
	b := base{maxBytes: maxBytes}
	return []tool.Tool{
		readTool{b}, writeTool{b}, editTool{b},
		listTool{b}, globTool{b}, grepTool{b},
	}
}

type base struct{ maxBytes int }

// errNoWorkspace is the fail-closed answer to a call that arrived with no
// session. The guard already refuses an unattributed call, so this is
// defence in depth for a tool reached another way -- never a reason to
// substitute a default directory.
var errNoWorkspace = errors.New("no session workspace on the context, so there is nowhere to resolve this path against")

func (b base) ws(ctx context.Context) (string, error) {
	ws := policy.WorkspaceFrom(ctx)
	if ws == "" {
		return "", errNoWorkspace
	}
	return ws, nil
}

func (b base) resolve(ctx context.Context, p string) (string, error) {
	ws, err := b.ws(ctx)
	if err != nil {
		return "", err
	}
	if p == "" {
		p = "."
	}
	return policy.Resolve(ws, p)
}

// rel renders a path for the model relative to the session's workspace when
// possible, so transcripts stay short and stable.
func (b base) rel(ctx context.Context, p string) string {
	ws := policy.WorkspaceFrom(ctx)
	if ws == "" {
		return p
	}
	if r, err := filepath.Rel(ws, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}
```

Then thread `ctx` through every `b.resolve(...)` and `b.rel(...)` call in the six tools' `Call` methods (each already receives `ctx`). Where a tool defaults a path to the workspace root — `fs_list`, `fs_glob`, `fs_grep` — `resolve(ctx, "")` already yields it.

- [ ] **Step 4: Rewrite the shell tool**

In `internal/tool/shell/shell.go`:

```go
type execTool struct {
	defaultTimeout time.Duration
	maxOutput      int
}

// New builds shell_exec. Like the filesystem tools it holds no workspace: the
// session's root arrives on the context of each call.
func New(defaultTimeout time.Duration, maxOutput int) tool.Tool {
	if defaultTimeout <= 0 {
		defaultTimeout = 120 * time.Second
	}
	if maxOutput <= 0 {
		maxOutput = 30_000
	}
	return &execTool{defaultTimeout: defaultTimeout, maxOutput: maxOutput}
}
```

In `Call`, before building the command:

```go
	ws := policy.WorkspaceFrom(ctx)
	if ws == "" {
		return "", errors.New("no session workspace on the context, so there is nowhere to run this command")
	}
```

and `cmd.Dir = ws`. Add the `internal/policy` import.

- [ ] **Step 5: Update the wiring**

`cmd/spore/wire.go`:

```go
	tools := fs.New(cfg.Policy.MaxOutput)
	tools = append(tools, shell.New(
		time.Duration(cfg.Shell.TimeoutSeconds)*time.Second, cfg.Policy.MaxOutput))
```

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS. Tests elsewhere that drive `fs`/`shell` through the guard already attach a session; any that call the tools directly with `context.Background()` need `policy.WithSession` — fix them in place.

- [ ] **Step 7: Commit**

```bash
git add internal/tool cmd/spore/wire.go
git commit -m "tools: resolve paths against the calling session's workspace

fs_* and shell_exec no longer hold a workspace. Each call reads the root
from the context and refuses when none is attached, rather than falling
back to a daemon-wide default.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 5: The prompt's environment section describes the session's directory

**Files:**
- Create: `internal/workspace/describers.go`
- Modify: `internal/agent/agent.go` (`Agent.Env`, `Snapshot`)
- Modify: `cmd/spore/wire.go:163`
- Test: `internal/workspace/describers_test.go`, `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `policy.WorkspaceFrom(ctx)` (Task 3), the existing `workspace.Describer` and `workspace.Describe(root string) string`.
- Produces:
  - ```go
    type Describers struct{ ... }
    func NewDescribers() *Describers
    func (d *Describers) Describe(root string) string
    ```
    One cached describer per root, so N sessions in N directories each keep their own TTL cache instead of fighting over one.
  - `Agent.Env` becomes `func(root string) string`. `Snapshot` calls it with the context's workspace and skips the environment section entirely when there is none.

- [ ] **Step 1: Write the failing tests**

`internal/workspace/describers_test.go`:

```go
package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One describer per root: two sessions in two directories must each see their
// own files, not whichever was described first.
func TestDescribersAreKeyedByRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "alpha.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "beta.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewDescribers()
	gotA, gotB := d.Describe(a), d.Describe(b)
	if !strings.Contains(gotA, "alpha.txt") || strings.Contains(gotA, "beta.txt") {
		t.Fatalf("describe(a) = %q", gotA)
	}
	if !strings.Contains(gotB, "beta.txt") || strings.Contains(gotB, "alpha.txt") {
		t.Fatalf("describe(b) = %q", gotB)
	}
}

func TestDescribersEmptyRoot(t *testing.T) {
	if got := NewDescribers().Describe(""); got != "" {
		t.Fatalf("describe(\"\") = %q, want empty", got)
	}
}
```

Add to `internal/agent/agent_test.go`. The file's `harness(t, script, tools)`
returns `(*Agent, *store.Store)`; use it rather than `newTestAgent`, which
throws the store away:

```go
func TestSnapshotDescribesTheSessionsWorkspace(t *testing.T) {
	a, st := harness(t, provider.NewScript(), nil)
	a.Env = func(root string) string { return "root=" + root }
	ctx := policy.WithSession(context.Background(),
		policy.Session{ID: "s1", Profile: policy.ProfileLocal, Workspace: "/ws/a"})
	id, err := st.CreateSession(ctx, "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Environment != "root=/ws/a" {
		t.Fatalf("environment = %q, want root=/ws/a", snap.Environment)
	}
}

// No session on the context means no environment section, rather than a
// description of a directory this turn is not working in.
func TestSnapshotHasNoEnvironmentWithoutAWorkspace(t *testing.T) {
	a, st := harness(t, provider.NewScript(), nil)
	a.Env = func(root string) string { return "root=" + root }
	id, err := st.CreateSession(context.Background(), "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Environment != "root=" {
		t.Fatalf("environment = %q, want the describer called with an empty root", snap.Environment)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/workspace/ ./internal/agent/ -run 'Describers|SnapshotDescribes' -v`
Expected: FAIL — `NewDescribers` undefined; `Env` takes no argument.

- [ ] **Step 3: Write the describer cache**

`internal/workspace/describers.go`:

```go
package workspace

import "sync"

// Describers holds one Describer per root. One daemon serves sessions in
// different directories, and each needs its own cached environment section:
// a single describer would rebuild on every turn as sessions alternate, and
// worse, could hand one session the other's file listing.
type Describers struct {
	mu    sync.Mutex
	byRoot map[string]*Describer
}

func NewDescribers() *Describers {
	return &Describers{byRoot: map[string]*Describer{}}
}

// Describe renders the environment section for one root. An empty root means
// the caller has no session workspace, and gets no environment section rather
// than a description of somewhere it is not working.
func (d *Describers) Describe(root string) string {
	if root == "" {
		return ""
	}
	d.mu.Lock()
	dd, ok := d.byRoot[root]
	if !ok {
		dd = NewDescriber(root)
		d.byRoot[root] = dd
	}
	d.mu.Unlock()
	return dd.Describe()
}
```

- [ ] **Step 4: Take the root from the context in the agent**

`internal/agent/agent.go`:

```go
	// Env renders the environment section of the system prompt for one root:
	// the session's working directory and its files. Nil means no environment
	// section, which is what a test that builds an Agent with New gets.
	Env func(root string) string
```

and in `Snapshot`:

```go
	if a.Env != nil {
		// The root comes from the turn context, not from the agent: one agent
		// serves every session, and each is rooted somewhere of its own.
		snap.Environment = a.Env(policy.WorkspaceFrom(ctx))
	}
```

Add the `internal/policy` import to `internal/agent/agent.go`. (No cycle: `internal/policy` declares its `Runner` interface locally and never imports `internal/agent`.)

- [ ] **Step 5: Update the wiring**

`cmd/spore/wire.go`:

```go
	a.Env = workspace.NewDescribers().Describe
```

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/workspace internal/agent cmd/spore/wire.go
git commit -m "agent: describe the session's own directory in the prompt

The environment section is rendered per root, with one cached describer per
directory, so two sessions in two projects each see their own files.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 6: The daemon records, exposes and honours the root

**Files:**
- Modify: `internal/daemon/sessions.go` (`SessionJSON`, `handleCreateSession`, `handlePatchSession`, `startTurn`)
- Modify: `internal/daemon/server.go` (route registration, a `CreateSession` method for the bridge)
- Modify: `internal/daemon/jobs.go` (`StartJob`)
- Modify: `internal/bridge/discord/bridge.go` (both `CreateSession` calls) and its `Options`
- Modify: `cmd/spore/wire.go` (`buildBridge` passes the remote root)
- Test: `internal/daemon/sessions_test.go`, `internal/daemon/api_test.go`, `internal/bridge/discord/bridge_test.go`

**Interfaces:**
- Consumes: `store.CreateSession/SetSessionWorkspace/SessionsDir` (Task 1), `workspace.Root/Allocated` (Task 2), `policy.Session` (Task 3).
- Produces:
  - `SessionJSON` gains `Workspace string \`json:"workspace"\``.
  - `POST /api/sessions` accepts `{"title": "...", "workspace": "/abs/path"}`; a workspace outside the ceiling is **400**, never a fallback. Omitted means a session directory.
  - `PATCH /api/sessions/{id}` accepts `{"workspace": "/abs/path"}` and re-roots the session; same 400 rule; responds with the updated `SessionJSON`.
  - `func (s *Server) CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error)` — the one place a session's root is decided, used by the HTTP handler, the scheduler and the bridge.
  - `discord.Options` gains `Sessions Sessions` where `type Sessions interface { CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error) }`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/daemon/sessions_test.go`. The package's harness is
`newTestServer(t, turns ...provider.ScriptTurn) (*Server, *httptest.Server)`
and its `postJSON(t, url, body) *http.Response` (both in `api_test.go`); the
tests below use those, reach the store through `srv.Store()`, and set the
ceiling on `srv.cfg` directly — same package, real config, no mock.

Two helpers this file needs; add them next to `postJSON`:

```go
func patchJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("PATCH", url, strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s: %v", url, err)
	}
	return res
}

func decodeSession(t *testing.T, res *http.Response, want int) SessionJSON {
	t.Helper()
	defer res.Body.Close()
	if res.StatusCode != want {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("status = %d, want %d: %s", res.StatusCode, want, body)
	}
	var out SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
```

```go
func TestCreateSessionRecordsTheRequestedWorkspace(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	inside := filepath.Join(srv.cfg.Policy.Workspace, "project")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	out := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{"workspace": inside}), http.StatusCreated)
	if out.Workspace != inside {
		t.Fatalf("workspace = %q, want %q", out.Workspace, inside)
	}
}

// The ceiling refuses at creation rather than quietly rooting the session
// somewhere else: a client that asked for the wrong place must be told.
func TestCreateSessionRefusesOutsideTheCeiling(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"workspace": t.TempDir()})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

func TestCreateSessionWithoutAWorkspaceGetsASessionDirectory(t *testing.T) {
	srv, ts := newTestServer(t)
	out := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	if want := filepath.Join(srv.Store().SessionsDir(), out.ID); out.Workspace != want {
		t.Fatalf("workspace = %q, want %q", out.Workspace, want)
	}
}

func TestPatchSessionReRoots(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)

	moved := filepath.Join(srv.cfg.Policy.Workspace, "elsewhere")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	out := decodeSession(t, patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]string{"workspace": moved}), http.StatusOK)
	if out.Workspace != moved {
		t.Fatalf("workspace = %q, want %q", out.Workspace, moved)
	}

	res := patchJSON(t, ts.URL+"/api/sessions/"+created.ID,
		map[string]string{"workspace": t.TempDir()})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("re-rooting outside the ceiling: status = %d, want 400", res.StatusCode)
	}
}

// spore allocated the directory, so spore creates it -- on the first turn,
// not at creation.
func TestFirstTurnCreatesAnAllocatedSessionDirectory(t *testing.T) {
	srv, ts := newTestServer(t, provider.ScriptTurn{Text: "hello"})
	created := decodeSession(t, postJSON(t, ts.URL+"/api/sessions",
		map[string]string{}), http.StatusCreated)
	if _, err := os.Stat(created.Workspace); !os.IsNotExist(err) {
		t.Fatalf("directory exists before the first turn: %v", err)
	}

	res := postJSON(t, ts.URL+"/api/sessions/"+created.ID+"/messages",
		map[string]string{"text": "hi"})
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("post message: status = %d", res.StatusCode)
	}
	// The turn runs on the server's context, so poll the hub the way
	// TestSecondTurnIsRejectedWhileOneIsRunning does.
	for i := 0; i < 200 && srv.hub.Running(created.ID); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := os.Stat(created.Workspace); err != nil {
		t.Fatalf("first turn did not create the session directory: %v", err)
	}
}

// A remote creator is confined to its own directory, whatever it asks for.
func TestRemoteSessionIsConfined(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.cfg.Policy.Workspace = t.TempDir()
	id, err := srv.CreateSession(context.Background(), "", srv.cfg.Policy.Workspace, policy.ProfileRemote)
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := srv.Store().Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.Workspace, srv.Store().SessionsDir()) {
		t.Fatalf("remote session rooted at %q, want a session directory", sess.Workspace)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run 'Workspace|Patch|SessionDirectory|Confined' -v`
Expected: FAIL — `SessionJSON.Workspace` undefined, no `PATCH` route, no `Server.CreateSession`.

- [ ] **Step 3: One place decides a session's root**

In `internal/daemon/sessions.go`:

```go
type SessionJSON struct {
	ID string `json:"id"`
	Title string `json:"title"`
	// Workspace is where the session is rooted. Clients show it so a human
	// can see which directory a detached session is operating on.
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSessionJSON(s store.Session) SessionJSON {
	return SessionJSON{ID: s.ID, Title: s.Title, Workspace: s.Workspace,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

// CreateSession is the one place a session's root is decided: the HTTP
// handler, the scheduler and the bridge all come through here, so the ceiling
// is checked once rather than in three places that can drift.
func (s *Server) CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error) {
	root, err := workspace.Root(workspace.Request{
		Requested:  requested,
		Ceiling:    s.cfg.Policy.Workspace,
		RemoteRoot: s.cfg.Policy.Profiles[string(policy.ProfileRemote)].Workspace,
		Remote:     profile == policy.ProfileRemote,
	})
	if err != nil {
		return "", err
	}
	return s.store.CreateSession(ctx, title, root)
}
```

The handler, with the ceiling refusal as a 400:

```go
func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title string `json:"title"`
		// Workspace is optional. Omitting it is a creator saying it has no
		// directory of its own -- the web UI, a script -- and it gets a
		// session directory. Naming one outside the ceiling is an error, not
		// a fallback: a client that asked for the wrong place must be told.
		Workspace string `json:"workspace"`
	}
	// An empty body is fine -- a session with no title is legal.
	_ = json.NewDecoder(r.Body).Decode(&body)
	id, err := s.CreateSession(r.Context(), strings.TrimSpace(body.Title), strings.TrimSpace(body.Workspace), policy.ProfileLocal)
	if err != nil {
		writeError(w, http.StatusBadRequest, "create session: %v", err)
		return
	}
	sess, found, err := s.store.Session(r.Context(), id)
	if err != nil || !found {
		writeError(w, http.StatusInternalServerError, "read back session %s: %v", id, err)
		return
	}
	writeJSON(w, http.StatusCreated, toSessionJSON(sess))
}

// handlePatchSession re-roots a session. The root is fixed at creation
// everywhere else; this exists for the CLI's deliberate "--workspace on a
// resume", and it is bounded by the same ceiling as creation.
func (s *Server) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var body struct {
		Workspace string `json:"workspace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	root, err := workspace.Root(workspace.Request{
		Requested: strings.TrimSpace(body.Workspace),
		Ceiling:   s.cfg.Policy.Workspace,
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if root == "" {
		writeError(w, http.StatusBadRequest, "workspace is required")
		return
	}
	if err := s.store.SetSessionWorkspace(r.Context(), id, root); err != nil {
		writeError(w, http.StatusInternalServerError, "re-root session: %v", err)
		return
	}
	sess, _, err := s.store.Session(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read back session %s: %v", id, err)
		return
	}
	writeJSON(w, http.StatusOK, toSessionJSON(sess))
}
```

Register the route in `internal/daemon/server.go` beside the others:

```go
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
```

- [ ] **Step 4: Put the root on the turn context and create it when it is ours**

In `startTurn`, replacing the `policy.WithSession` line:

```go
	// The session's root is read once per turn and travels on the context:
	// the tools, the prompt's environment section and the policy engine all
	// take it from there, so there is one answer to "where is this session
	// working" and it comes from the row.
	sess, found, err := s.store.Session(s.base, sessionID)
	if err != nil {
		turnErr = fmt.Errorf("read session %s: %w", sessionID, err)
		return turnErr
	}
	if !found {
		return fmt.Errorf("no session %s", sessionID)
	}
	// A directory spore allocated is spore's to create, and it is created
	// here rather than at creation so a session that is opened and never used
	// leaves nothing on disk. A directory a human named is never created:
	// a typo must fail loudly, not silently make an empty directory.
	if workspace.Allocated(s.store.SessionsDir(), sess.Workspace) {
		if err := os.MkdirAll(sess.Workspace, 0o700); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	ctx := policy.WithSession(s.base, policy.Session{
		ID: sessionID, Profile: profile, Workspace: sess.Workspace,
	})
```

(Drop the `turnErr` line if the function has no such variable; return the error directly. The panic-recovery `defer` at the top of `startTurn` stays exactly where it is.)

- [ ] **Step 5: Scheduler and bridge**

`internal/daemon/jobs.go`, in `StartJob`:

```go
	// A job has no directory of its own, so it gets a session directory --
	// the same treatment as the web UI and the bridge.
	sessionID, err := s.CreateSession(ctx, title, "", policy.ProfileLocal)
```

`internal/bridge/discord/bridge.go`: add to `Options` and the `Bridge` struct

```go
// Sessions creates the sessions this bridge binds threads to. It is an
// interface for the same reason Turns is: the bridge is tested without a
// daemon.
type Sessions interface {
	CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error)
}
```

with `Sessions Sessions` in `Options` (required, checked in `New` alongside `Turns`), stored as `sessions`. Both `b.store.CreateSession(b.ctx, "")` calls become:

```go
	sessionID, err := b.sessions.CreateSession(b.ctx, "", "", policy.ProfileRemote)
```

The bridge names no directory and never could: `workspace.Root` ignores a remote creator's request, so a Discord session is confined to its own directory unless the operator set `[policy.profile.remote] workspace`.

In `cmd/spore/wire.go`, `buildBridge` passes `Sessions: srv` alongside `Turns: srv`.

- [ ] **Step 6: Run the tests**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/daemon internal/bridge cmd/spore/wire.go
git commit -m "daemon: root each session and carry it through the turn

Session creation takes an optional workspace and refuses one outside the
ceiling with a 400; PATCH /api/sessions/{id} re-roots one; the turn context
carries the row's workspace, and an allocated session directory is created
on the first turn. Jobs and the Discord bridge get directories of their own.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 7: The CLI sends its directory

**Files:**
- Modify: `cmd/spore/client.go` (`createSession`, new `setWorkspace`)
- Modify: `cmd/spore/chat.go`, `cmd/spore/once.go` (send cwd; honour `--workspace`)
- Modify: `cmd/spore/main.go` (flag parsing, usage text, store backfill)
- Modify: `cmd/spore/session.go` (`list` and `show` report the root)
- Modify: `cmd/spore/policy.go` (`-workspace` on `policy check`)
- Test: `cmd/spore/wire_test.go` (or a new `cmd/spore/workspace_test.go`)

**Interfaces:**
- Consumes: `POST /api/sessions` with `workspace`, `PATCH /api/sessions/{id}` (Task 6); `store.BackfillSessionWorkspaces` (Task 1).
- Produces:
  - `func (c *client) createSession(ctx context.Context, title, workspace string) (string, error)`
  - `func (c *client) setWorkspace(ctx context.Context, sessionID, workspace string) error`
  - `func cmdChat(ctx context.Context, cfg *config.Config, sessionID, workspaceFlag string) error`
  - `func cmdOnce(ctx context.Context, cfg *config.Config, prompt, workspaceFlag string) error`
  - `func sessionWorkspace(flag string) (string, error)` in `cmd/spore/main.go` — the flag when set, otherwise `os.Getwd()`, always absolute.

- [ ] **Step 1: Write the failing tests**

Create `cmd/spore/workspace_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionWorkspaceDefaultsToCwd(t *testing.T) {
	got, err := sessionWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if got != cwd {
		t.Fatalf("workspace = %q, want the working directory %q", got, cwd)
	}
}

func TestSessionWorkspaceMakesTheFlagAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir) // Go 1.24+; otherwise save and restore os.Chdir by hand
	got, err := sessionWorkspace("sub/dir")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "sub", "dir"); got != want {
		t.Fatalf("workspace = %q, want %q", got, want)
	}
}
```

Add to `cmd/spore/wire_test.go` a check that the flag reaches the request. If the file has no fake-daemon harness, use `httptest`:

```go
func TestCreateSessionSendsTheWorkspace(t *testing.T) {
	var got struct {
		Title     string `json:"title"`
		Workspace string `json:"workspace"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]string{"id": "s1"})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}
	if _, err := c.createSession(context.Background(), "chat", "/ws/a"); err != nil {
		t.Fatal(err)
	}
	if got.Workspace != "/ws/a" {
		t.Fatalf("workspace sent = %q, want /ws/a", got.Workspace)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run 'Workspace' -v`
Expected: FAIL — `sessionWorkspace` undefined; `createSession` takes 2 arguments.

- [ ] **Step 3: Client and the cwd helper**

`cmd/spore/client.go`:

```go
func (c *client) createSession(ctx context.Context, title, workspace string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/api/sessions",
		map[string]string{"title": title, "workspace": workspace}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// setWorkspace re-roots an existing session. It is the deliberate exception
// to "the root is fixed at creation": --workspace on a resume rewrites the
// row, because the human asking for it is the one who chose the original.
func (c *client) setWorkspace(ctx context.Context, sessionID, workspace string) error {
	return c.do(ctx, "PATCH", "/api/sessions/"+sessionID,
		map[string]string{"workspace": workspace}, nil)
}
```

`cmd/spore/main.go`:

```go
// sessionWorkspace is the directory a CLI-created session is rooted at: the
// --workspace flag when given, otherwise the directory spore was run in, so
// spore describes and operates on the directory you are standing in.
func sessionWorkspace(flag string) (string, error) {
	if flag == "" {
		return os.Getwd()
	}
	return filepath.Abs(flag)
}
```

- [ ] **Step 4: Wire the flag into `chat` and `once`**

`cmd/spore/main.go`, in `run`, parse `--workspace` for both commands before dispatch:

```go
	case "once":
		rest, ws, err := takeWorkspaceFlag(args[1:])
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("once needs a prompt")
		}
		return cmdOnce(ctx, cfg, rest[0], ws)
	case "chat":
		rest, ws, err := takeWorkspaceFlag(args[1:])
		if err != nil {
			return err
		}
		id := ""
		if len(rest) > 0 {
			id = rest[0]
		}
		return cmdChat(ctx, cfg, id, ws)
```

with, next to `sessionWorkspace`:

```go
// takeWorkspaceFlag pulls "--workspace <dir>" (or "-workspace <dir>") out of
// an argument list, wherever it appears, and returns the rest. spore's CLI
// parses by hand rather than with the flag package, because a prompt is a
// positional argument that may itself begin with a dash.
func takeWorkspaceFlag(args []string) (rest []string, workspace string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--workspace" && args[i] != "-workspace" {
			rest = append(rest, args[i])
			continue
		}
		if i+1 >= len(args) {
			return nil, "", fmt.Errorf("--workspace needs a directory")
		}
		workspace = args[i+1]
		i++
	}
	return rest, workspace, nil
}
```

`cmd/spore/once.go`:

```go
func cmdOnce(ctx context.Context, cfg *config.Config, prompt, workspaceFlag string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	ws, err := sessionWorkspace(workspaceFlag)
	if err != nil {
		return err
	}
	sessionID, err := c.createSession(ctx, prompt, ws)
	if err != nil {
		return err
	}
```

`cmd/spore/chat.go`:

```go
func cmdChat(ctx context.Context, cfg *config.Config, sessionID, workspaceFlag string) error {
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	if sessionID == "" {
		ws, err := sessionWorkspace(workspaceFlag)
		if err != nil {
			return err
		}
		if sessionID, err = c.createSession(ctx, "chat", ws); err != nil {
			return err
		}
	} else if workspaceFlag != "" {
		// Resuming does not move a session -- a transcript, its recall hits
		// and its file references stay coherent. --workspace is the one way
		// to say "move it anyway", and it rewrites the row.
		ws, err := sessionWorkspace(workspaceFlag)
		if err != nil {
			return err
		}
		if err := c.setWorkspace(ctx, sessionID, ws); err != nil {
			return err
		}
	}
```

Update the usage text in `main.go`:

```
  spore once <prompt>          run one turn in a fresh session and print the reply
  spore chat [session-id]      interactive session (resumes when given an id)

flags:
  -config <path>       config file (default ~/.spore/config.toml)
  --workspace <dir>    root a new session here instead of the current directory;
                       on a resume, re-root the session (once, chat)
```

- [ ] **Step 5: Backfill on open, and report the root**

In `cmd/spore/main.go`, both branches that open the store (`serve`, `session`) go through one helper:

```go
// openStore opens the database and backfills any session written before the
// workspace column existed, so resuming one behaves exactly as it did when
// the workspace was a single daemon-wide value.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	if n, err := st.BackfillSessionWorkspaces(ctx, cfg.Policy.Workspace); err != nil {
		// Not fatal: a session whose root is empty still lists and still
		// shows, and the daemon refuses only to run a turn for it.
		slog.Default().Warn("backfilling session workspaces failed", "error", err)
	} else if n > 0 {
		slog.Default().Info("rooted sessions written before stage 6", "count", n, "workspace", cfg.Policy.Workspace)
	}
	return st, nil
}
```

`cmd/spore/session.go`, `list` and `show`:

```go
		for _, s := range sessions {
			fmt.Printf("%s  %s  %s  %s\n", s.ID, s.UpdatedAt.Format("2006-01-02 15:04"), s.Workspace, s.Title)
		}
```

and at the top of `show`, before the messages (so a human reading a transcript knows which directory its file references mean):

```go
		sess, found, err := st.Session(ctx, args[1])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("no session %s", args[1])
		}
		fmt.Printf("session %s\nworkspace %s\n", sess.ID, sess.Workspace)
```

- [ ] **Step 6: `policy check -workspace`**

In `cmd/spore/main.go`'s `policy` branch, accept `-workspace <dir>` in the same loop that reads `-profile`, and pass it to `cmdPolicyCheck(cfg, profile, workspace, tool, jsonArgs)`. In `cmd/spore/policy.go`:

```go
// A session's workspace decides what "path outside workspace" means, so the
// check takes one. Without it the ceiling is used, which is the answer for
// "would this be allowed anywhere at all".
	res := engine.Evaluate(policy.Session{
		ID: "policy-check", Profile: policy.Profile(profile), Workspace: workspace,
	}, call)
```

and print the workspace the decision was made against alongside the decision.

- [ ] **Step 7: Run the tests and the binary**

Run: `go test -tags sqlite_fts5 ./... 2>&1 | tail -30`
Expected: PASS.

Run: `make build && ./spore -config /tmp/spore-stage6/config.toml session list`
Expected: builds; `session list` prints a workspace column (empty database prints nothing, which is fine).

- [ ] **Step 8: Commit**

```bash
git add cmd/spore
git commit -m "cli: send the working directory, and show where a session lives

chat and once root a new session at the directory they were run in;
--workspace overrides that and, on a resume, re-roots the session. session
list and show report the root, policy check takes one, and opening the
store backfills sessions written before the column existed.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

### Task 8: The web UI shows the root, and the docs stop lying

**Files:**
- Modify: `web/app.js` (session list and header render the workspace)
- Modify: `web/index.html` (a header element to render into), `web/style.css` (one rule)
- Modify: `README.md`
- Modify: `docs/backlog.md`
- Test: `internal/daemon/web_test.go` (the embedded-asset test already there; extend only if it asserts on content)

**Interfaces:**
- Consumes: `SessionJSON.workspace` (Task 6).
- Produces: no Go API.

- [ ] **Step 1: Render the workspace**

In `web/index.html`, inside the transcript header (next to wherever the session id is shown), add:

```html
  <span id="workspace" class="workspace" title="the directory this session is rooted at"></span>
```

In `web/app.js`, where the transcript is loaded (`const tr = await api("GET", "/api/sessions/" + id)`), after it resolves:

```js
    // Which directory a session operates on is not a detail: two sessions in
    // two projects look identical without it.
    el("workspace").textContent = (tr.session && tr.session.workspace) || "";
```

In the session-list renderer, set each link's tooltip so the nav stays short:

```js
    a.title = s.workspace || "";
```

In `web/style.css`:

```css
.workspace {
  color: var(--muted, #888);
  font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
  font-size: 0.85em;
}
```

The web UI creates sessions with no workspace (`{ title: "web" }`), which is correct: it has no directory of its own and gets a session directory. Leave that call as it is.

- [ ] **Step 2: Check the UI renders**

Run: `make build && ./spore serve --status; ./spore serve &` then open `http://127.0.0.1:7777/`, create a session, confirm the header shows a path under `~/.spore/sessions/`. Stop with `./spore serve --stop`.
Expected: the path appears; the console shows no errors.

- [ ] **Step 3: Update README and backlog**

In `README.md`, wherever `policy.workspace` is described, say what it now is:

```markdown
`[policy] workspace` is a **ceiling**, not a working directory. Each session
records the directory it is rooted at: `spore chat` and `spore once` send the
directory you ran them in, and a creator with no directory of its own — the
web UI, the scheduler, the Discord bridge — gets `~/.spore/sessions/<id>`,
created on that session's first turn. A session rooted outside the ceiling is
refused at creation. `--workspace <dir>` roots a new session elsewhere and,
on a resume, re-roots an existing one.
```

In `docs/backlog.md`, replace the "Nothing here is a commitment to an order" paragraph's last two sentences with:

```markdown
Nothing here is a commitment to an order. Stages 5a, 5b, 5c and 6 have all
shipped; the staged plan in section 11 of the design spec is complete, and
everything below is what has been asked for since.
```

- [ ] **Step 4: Full verification**

Run: `make fmtcheck && make vet && make test 2>&1 | tail -20`
Expected: all three clean.

- [ ] **Step 5: Commit**

```bash
git add web README.md docs/backlog.md
git commit -m "web, docs: show where a session is rooted

The transcript header and the session list report each session's workspace,
and the README and backlog describe policy.workspace as the ceiling it now
is rather than the directory spore works in.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MVzgRipyqpVEe8AKJTSFM2"
```

---

## Verification checklist

Before opening the PR, in a clean worktree at HEAD (not the dirty development tree):

```bash
git worktree add /tmp/spore-verify HEAD
cd /tmp/spore-verify && make fmtcheck && make vet && make test
```

All three must pass on their own output, not on a claim about it. Then, by hand:

1. `cd /some/project && spore once "list the files here"` — the reply describes that project, and `spore session list` shows it rooted there.
2. `spore chat` in a second directory while the first session still exists — `fs_list` in each returns that session's own files.
3. `spore once --workspace /etc "read hosts"` with the default ceiling of `~` — refused at creation, naming the ceiling.
4. A Discord message (if a bridge is configured) — the session is rooted under `~/.spore/sessions/`, and `fs_read` of a path in the ceiling is denied by `fs_*(path outside workspace)`.
5. An old database: `spore session list` against a copy of a pre-stage-6 `spore.db` shows every session rooted at the configured ceiling, and resuming one still works.
