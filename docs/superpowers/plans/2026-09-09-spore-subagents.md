# Sub-agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a spore session launch another agent — blocking on its answer with `agent_run`, or detaching it with `agent_spawn` — and let a human see and cancel what is running.

**Architecture:** A new `internal/subagent.Supervisor` is the single owner of live children; `internal/tool/subagent` wraps it as three tools, and the daemon reads the same object for `GET /api/sessions/{id}/agents`. A child is a session row with `parent_id`, so it inherits persistence, compaction and cost accounting. It inherits the parent's trust profile and workspace; its approvals route to the root of the chain through an upward-only ancestor walk placed in front of the existing atomic `ClaimPendingCall`.

**Tech Stack:** Go, SQLite (`mattn/go-sqlite3`, build tag `sqlite_fts5`), `net/http` `ServeMux` with method+path patterns, Bubble Tea for the TUI.

**Spec:** `docs/superpowers/specs/2026-09-09-spore-subagents-design.md`

## Global Constraints

- Build and test with the FTS tag: `go build -tags sqlite_fts5 ./...` and `go test -tags sqlite_fts5 ./...`. A run without the tag does not compile the store.
- Tool names must match `\A[a-zA-Z0-9_-]{1,64}\z` (intersection of the Anthropic and OpenAI rules), enforced by `tool.Registry.Register`.
- Policy tests build `config.PolicyConfig` **explicitly**, never from `config.Default()` or `config.Load`. `config.Load` adds a baseline deny set that silently satisfies assertions meant to be testing the rules under test.
- `deny` is absolute and never escalates to a human — for children exactly as for parents.
- New tools report `ReadOnly() == false`. A child mutates, so it must never join the loop's parallel read-only batch.
- Every LLM call names a `router` call site. The set is closed and validated by `router.ValidSite`.
- Comments explain *why*, matching the density of surrounding code. Do not narrate what the code plainly does.
- Commit after each task with a `type(scope): subject` subject line.

---

### Task 1: Session parentage

**Files:**
- Modify: `internal/store/schema.go` (the `sessions` CREATE TABLE, ~line 9)
- Modify: `internal/store/store.go` (`Session` struct ~line 28, `migrateSessions` ~line 64, `CreateSession` ~line 153, `ListSessions` ~line 168)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `store.Session.ParentID string`; `(*Store).CreateChildSession(ctx context.Context, title, workspace, parentID string) (string, error)`; `(*Store).ListSessions(ctx context.Context, limit int, includeChildren bool) ([]Session, error)`; `(*Store).SessionAncestors(ctx context.Context, id string) ([]string, error)` returning ids from the immediate parent up to the root, root last.

- [ ] **Step 1: Write the failing tests**

Add to `internal/store/store_test.go`:

```go
func TestCreateChildSessionRecordsParent(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	parent, err := s.CreateSession(ctx, "parent", "/tmp/ws")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	child, err := s.CreateChildSession(ctx, "child", "/tmp/ws", parent)
	if err != nil {
		t.Fatalf("CreateChildSession: %v", err)
	}

	got, ok, err := s.Session(ctx, child)
	if err != nil || !ok {
		t.Fatalf("Session(%s): ok=%v err=%v", child, ok, err)
	}
	if got.ParentID != parent {
		t.Errorf("ParentID = %q, want %q", got.ParentID, parent)
	}
	top, ok, err := s.Session(ctx, parent)
	if err != nil || !ok {
		t.Fatalf("Session(%s): ok=%v err=%v", parent, ok, err)
	}
	if top.ParentID != "" {
		t.Errorf("top-level ParentID = %q, want empty", top.ParentID)
	}
}

func TestListSessionsHidesChildrenUnlessAsked(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	parent, err := s.CreateSession(ctx, "parent", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChildSession(ctx, "child", "/tmp/ws", parent); err != nil {
		t.Fatal(err)
	}

	top, err := s.ListSessions(ctx, 50, false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(top) != 1 || top[0].ID != parent {
		t.Fatalf("default listing = %+v, want only the parent", top)
	}

	all, err := s.ListSessions(ctx, 50, true)
	if err != nil {
		t.Fatalf("ListSessions(all): %v", err)
	}
	if len(all) != 2 {
		t.Errorf("listing with children = %d rows, want 2", len(all))
	}
}

func TestSessionAncestorsWalksToRoot(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	root, err := s.CreateSession(ctx, "root", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	mid, err := s.CreateChildSession(ctx, "mid", "/tmp/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := s.CreateChildSession(ctx, "leaf", "/tmp/ws", mid)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.SessionAncestors(ctx, leaf)
	if err != nil {
		t.Fatalf("SessionAncestors: %v", err)
	}
	if len(got) != 2 || got[0] != mid || got[1] != root {
		t.Errorf("ancestors = %v, want [%s %s]", got, mid, root)
	}

	none, err := s.SessionAncestors(ctx, root)
	if err != nil {
		t.Fatalf("SessionAncestors(root): %v", err)
	}
	if len(none) != 0 {
		t.Errorf("root ancestors = %v, want none", none)
	}
}

func TestMigrateSessionsAddsParentID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")

	// A database written before this change: sessions with a workspace but
	// no parent_id.
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE sessions (
	  id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '',
	  workspace TEXT NOT NULL DEFAULT '',
	  created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO sessions (id, title, workspace, created_at, updated_at)
		 VALUES ('keepme', 'old', '/tmp/ws', '2026-01-01T00:00:00.000000000Z', '2026-01-01T00:00:00.000000000Z')`,
	); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-migration database: %v", err)
	}
	defer s.Close()

	got, ok, err := s.Session(context.Background(), "keepme")
	if err != nil || !ok {
		t.Fatalf("row did not survive the migration: ok=%v err=%v", ok, err)
	}
	if got.ParentID != "" {
		t.Errorf("migrated ParentID = %q, want empty", got.ParentID)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'ChildSession|ListSessionsHides|Ancestors|MigrateSessionsAddsParent' -v`
Expected: FAIL — compile error, `s.CreateChildSession undefined` and `ListSessions` called with 3 arguments.

- [ ] **Step 3: Add the column to the schema**

In `internal/store/schema.go`, the `sessions` table becomes:

```sql
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  title      TEXT NOT NULL DEFAULT '',
  workspace  TEXT NOT NULL DEFAULT '',
  parent_id  TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_id);
```

- [ ] **Step 4: Extend the migration**

`migrateSessions` currently detects one missing column. Generalise it to detect both, keeping its existing "no table at all" early return:

```go
func migrateSessions(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		return fmt.Errorf("inspect sessions table: %w", err)
	}
	defer rows.Close()
	var columns int
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		columns++
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// No table at all: the schema statement creates the right one.
	if columns == 0 {
		return nil
	}
	if !have["workspace"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN workspace TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add sessions.workspace: %w", err)
		}
	}
	if !have["parent_id"] {
		if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN parent_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add sessions.parent_id: %w", err)
		}
	}
	return nil
}
```

- [ ] **Step 5: Add the field, the constructor and the walk**

In `internal/store/store.go`, add to `Session`:

```go
	// ParentID is the session that launched this one as a sub-agent, or ""
	// for a top-level session. Fixed at creation and never rewritten, which
	// is what lets the approval ancestor walk trust it without a lock.
	ParentID string
```

Then:

```go
// CreateChildSession creates a sub-agent's session. The workspace is passed
// in rather than defaulted, because a child inherits its parent's root: two
// agents working the same task must see the same files.
func (s *Store) CreateChildSession(ctx context.Context, title, workspace, parentID string) (string, error) {
	if parentID == "" {
		return "", fmt.Errorf("create child session: parent id is required")
	}
	id := newID()
	if workspace == "" {
		workspace = filepath.Join(s.SessionsDir(), id)
	}
	now := nowString()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, title, workspace, parent_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, title, workspace, parentID, now, now)
	if err != nil {
		return "", fmt.Errorf("create child session: %w", err)
	}
	return id, nil
}

// SessionAncestors returns the chain above id, immediate parent first and the
// root last. A top-level session has none. The walk is bounded by
// maxAncestorWalk so a parent_id cycle -- which nothing writes, but which a
// hand-edited database could hold -- cannot spin here forever.
func (s *Store) SessionAncestors(ctx context.Context, id string) ([]string, error) {
	var out []string
	seen := map[string]bool{id: true}
	for i := 0; i < maxAncestorWalk; i++ {
		var parent string
		err := s.db.QueryRowContext(ctx, `SELECT parent_id FROM sessions WHERE id = ?`, id).Scan(&parent)
		if err == sql.ErrNoRows {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read parent of %s: %w", id, err)
		}
		if parent == "" {
			return out, nil
		}
		if seen[parent] {
			return nil, fmt.Errorf("session %s: parent_id cycle", id)
		}
		seen[parent] = true
		out = append(out, parent)
		id = parent
	}
	return nil, fmt.Errorf("session ancestry deeper than %d", maxAncestorWalk)
}
```

Add the constant near `timeFormat`:

```go
// maxAncestorWalk bounds SessionAncestors. It is far above any depth the
// subagent config permits; it exists to make a corrupt row terminate.
const maxAncestorWalk = 64
```

`CreateSession` keeps its signature and inserts `parent_id` as `''` — top-level creation is untouched.

- [ ] **Step 6: Filter the listing**

```go
// ListSessions returns the most recently updated sessions. Sub-agent
// sessions are hidden unless includeChildren: a fan-out leaves one row per
// child, and `session list` is a human's view of their own conversations.
func (s *Store) ListSessions(ctx context.Context, limit int, includeChildren bool) ([]Session, error) {
	q := `SELECT id, title, workspace, parent_id, created_at, updated_at FROM sessions
	      WHERE parent_id = '' ORDER BY updated_at DESC LIMIT ?`
	if includeChildren {
		q = `SELECT id, title, workspace, parent_id, created_at, updated_at FROM sessions
		     ORDER BY updated_at DESC LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var sess Session
		var created, updated string
		if err := rows.Scan(&sess.ID, &sess.Title, &sess.Workspace, &sess.ParentID, &created, &updated); err != nil {
			return nil, err
		}
		sess.CreatedAt, _ = time.Parse(timeFormat, created)
		sess.UpdatedAt, _ = time.Parse(timeFormat, updated)
		out = append(out, sess)
	}
	return out, rows.Err()
}
```

Add `parent_id` to the `SELECT` and `Scan` in `Session` (the single-row reader) the same way.

- [ ] **Step 7: Fix every caller of ListSessions**

Run: `go build -tags sqlite_fts5 ./... 2>&1 | head -20`

Pass `false` at each call site (the human-facing listings: `cmd/spore` session list, `internal/daemon` handleListSessions). Add an `--all` flag to the `session list` CLI command that passes `true`, and print a `parent` column only when it is set.

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/store/ ./internal/daemon/ ./cmd/... -v 2>&1 | tail -20`
Expected: PASS, including the four new tests.

- [ ] **Step 9: Commit**

```bash
git add internal/store cmd/spore internal/daemon
git commit -m "feat(store): record session parentage

Sub-agent sessions are sessions with a parent_id, so they inherit
persistence, compaction and cost accounting. ListSessions hides them
unless asked, because a fan-out would otherwise fill a human's session
list with transcripts they never opened."
```

---

### Task 2: The sub-agent run table

**Files:**
- Modify: `internal/store/schema.go`
- Create: `internal/store/subagents.go`
- Test: `internal/store/subagents_test.go`

**Interfaces:**
- Consumes: `CreateChildSession` from Task 1.
- Produces: `store.SubagentRun` struct; `(*Store).StartSubagentRun(ctx, run SubagentRun) error`; `(*Store).FinishSubagentRun(ctx, sessionID, state, result, errText string) error`; `(*Store).SubagentRun(ctx, sessionID string) (SubagentRun, bool, error)`; `(*Store).SubagentRunsByParent(ctx, parentID string) ([]SubagentRun, error)`; `(*Store).InterruptRunningSubagents(ctx context.Context) (int64, error)`; `(*Store).TreeCost(ctx, rootID string) (float64, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/subagents_test.go`:

```go
package store

import (
	"context"
	"testing"
)

func newRunTree(t *testing.T, s *Store) (parent, child string) {
	t.Helper()
	ctx := context.Background()
	parent, err := s.CreateSession(ctx, "parent", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	child, err = s.CreateChildSession(ctx, "child", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	return parent, child
}

func TestSubagentRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)

	err := s.StartSubagentRun(ctx, SubagentRun{
		SessionID: child, ParentID: parent, Prompt: "audit the policy tests", Depth: 1,
	})
	if err != nil {
		t.Fatalf("StartSubagentRun: %v", err)
	}

	got, ok, err := s.SubagentRun(ctx, child)
	if err != nil || !ok {
		t.Fatalf("SubagentRun: ok=%v err=%v", ok, err)
	}
	if got.State != "running" {
		t.Errorf("State = %q, want running", got.State)
	}
	if got.Prompt != "audit the policy tests" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if !got.EndedAt.IsZero() {
		t.Errorf("EndedAt = %v, want zero while running", got.EndedAt)
	}

	if err := s.FinishSubagentRun(ctx, child, "done", "no findings", ""); err != nil {
		t.Fatalf("FinishSubagentRun: %v", err)
	}
	got, _, err = s.SubagentRun(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "done" || got.Result != "no findings" {
		t.Errorf("after finish: state=%q result=%q", got.State, got.Result)
	}
	if got.EndedAt.IsZero() {
		t.Error("EndedAt is still zero after finishing")
	}
}

func TestInterruptRunningSubagentsMarksOrphans(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)

	if err := s.StartSubagentRun(ctx, SubagentRun{SessionID: child, ParentID: parent, Prompt: "p", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	other, err := s.CreateChildSession(ctx, "other", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartSubagentRun(ctx, SubagentRun{SessionID: other, ParentID: parent, Prompt: "q", Depth: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishSubagentRun(ctx, other, "done", "ok", ""); err != nil {
		t.Fatal(err)
	}

	n, err := s.InterruptRunningSubagents(ctx)
	if err != nil {
		t.Fatalf("InterruptRunningSubagents: %v", err)
	}
	if n != 1 {
		t.Errorf("interrupted %d rows, want 1", n)
	}
	got, _, _ := s.SubagentRun(ctx, child)
	if got.State != "interrupted" {
		t.Errorf("orphan state = %q, want interrupted", got.State)
	}
	// A finished run must not be rewritten by the sweep.
	fin, _, _ := s.SubagentRun(ctx, other)
	if fin.State != "done" {
		t.Errorf("finished run was rewritten to %q", fin.State)
	}
}

func TestTreeCostSumsDescendants(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	parent, child := newRunTree(t, s)
	grand, err := s.CreateChildSession(ctx, "grand", "/tmp/ws", child)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := s.CreateSession(ctx, "unrelated", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		session string
		cost    float64
	}{{parent, 0.01}, {child, 0.02}, {grand, 0.04}, {outside, 9.00}} {
		if _, err := s.AppendMessage(ctx, Message{
			SessionID: tc.session, Role: "assistant",
			BlocksJSON: []byte(`[{"type":"text","text":"x"}]`), CostUSD: tc.cost,
		}); err != nil {
			t.Fatalf("AppendMessage(%s): %v", tc.session, err)
		}
	}

	got, err := s.TreeCost(ctx, parent)
	if err != nil {
		t.Fatalf("TreeCost: %v", err)
	}
	if want := 0.07; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("TreeCost = %v, want %v (an unrelated session must not count)", got, want)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run Subagent -v`
Expected: FAIL — `undefined: SubagentRun`.

- [ ] **Step 3: Add the table**

In `internal/store/schema.go`:

```sql
-- subagent_runs is run bookkeeping for a sub-agent session: what it was
-- asked, whether it is still going, and what it returned. It is separate
-- from sessions because this is sub-agent-only state, where sessions.parent_id
-- is read by the approval walk and the session listing.
CREATE TABLE IF NOT EXISTS subagent_runs (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  parent_id  TEXT NOT NULL,
  prompt     TEXT NOT NULL,
  depth      INTEGER NOT NULL DEFAULT 1,
  state      TEXT NOT NULL,
  result     TEXT NOT NULL DEFAULT '',
  error      TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_subagent_runs_parent ON subagent_runs(parent_id, started_at);
```

- [ ] **Step 4: Write the accessors**

Create `internal/store/subagents.go`:

```go
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Sub-agent run states. A run is running until exactly one of the other
// three replaces it.
const (
	RunRunning     = "running"
	RunDone        = "done"
	RunFailed      = "failed"
	RunInterrupted = "interrupted"
)

// SubagentRun is one sub-agent's bookkeeping row.
type SubagentRun struct {
	SessionID string
	ParentID  string
	Prompt    string
	Depth     int
	State     string
	Result    string
	Error     string
	StartedAt time.Time
	EndedAt   time.Time // zero while running
}

func (s *Store) StartSubagentRun(ctx context.Context, r SubagentRun) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subagent_runs (session_id, parent_id, prompt, depth, state, started_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.SessionID, r.ParentID, r.Prompt, r.Depth, RunRunning, nowString())
	if err != nil {
		return fmt.Errorf("start subagent run %s: %w", r.SessionID, err)
	}
	return nil
}

// FinishSubagentRun records a terminal state. It only ever moves a running
// row: a cancel racing a natural completion must not overwrite the result
// the child actually produced.
func (s *Store) FinishSubagentRun(ctx context.Context, sessionID, state, result, errText string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subagent_runs SET state = ?, result = ?, error = ?, ended_at = ?
		 WHERE session_id = ? AND state = ?`,
		state, result, errText, nowString(), sessionID, RunRunning)
	if err != nil {
		return fmt.Errorf("finish subagent run %s: %w", sessionID, err)
	}
	return nil
}

func scanRun(sc interface{ Scan(...any) error }) (SubagentRun, error) {
	var r SubagentRun
	var started, ended string
	if err := sc.Scan(&r.SessionID, &r.ParentID, &r.Prompt, &r.Depth,
		&r.State, &r.Result, &r.Error, &started, &ended); err != nil {
		return SubagentRun{}, err
	}
	r.StartedAt, _ = time.Parse(timeFormat, started)
	if ended != "" {
		r.EndedAt, _ = time.Parse(timeFormat, ended)
	}
	return r, nil
}

const runColumns = `session_id, parent_id, prompt, depth, state, result, error, started_at, ended_at`

func (s *Store) SubagentRun(ctx context.Context, sessionID string) (SubagentRun, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM subagent_runs WHERE session_id = ?`, sessionID)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return SubagentRun{}, false, nil
	}
	if err != nil {
		return SubagentRun{}, false, fmt.Errorf("read subagent run %s: %w", sessionID, err)
	}
	return r, true, nil
}

func (s *Store) SubagentRunsByParent(ctx context.Context, parentID string) ([]SubagentRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+runColumns+` FROM subagent_runs WHERE parent_id = ? ORDER BY started_at`, parentID)
	if err != nil {
		return nil, fmt.Errorf("list subagent runs: %w", err)
	}
	defer rows.Close()
	var out []SubagentRun
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InterruptRunningSubagents marks every still-running row interrupted. The
// goroutines that were driving them died with the previous process, so a
// running row at startup describes work that stopped without saying so.
func (s *Store) InterruptRunningSubagents(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE subagent_runs SET state = ?, error = ?, ended_at = ? WHERE state = ?`,
		RunInterrupted, "the process running this sub-agent exited", nowString(), RunRunning)
	if err != nil {
		return 0, fmt.Errorf("interrupt orphaned subagent runs: %w", err)
	}
	return res.RowsAffected()
}

// TreeCost sums what a session and every descendant have spent. The walk is
// a recursive CTE rather than repeated queries because the ceiling is checked
// before every child turn.
func (s *Store) TreeCost(ctx context.Context, rootID string) (float64, error) {
	var total sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		WITH RECURSIVE tree(id) AS (
		  SELECT id FROM sessions WHERE id = ?
		  UNION
		  SELECT s.id FROM sessions s JOIN tree t ON s.parent_id = t.id
		)
		SELECT SUM(m.cost_usd) FROM messages m JOIN tree ON m.session_id = tree.id`,
		rootID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("tree cost for %s: %w", rootID, err)
	}
	return total.Float64, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run Subagent -v`
Expected: PASS (3 tests).

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): add the subagent_runs table

Run bookkeeping lives beside sessions rather than on them: state,
prompt and result are sub-agent-only, where parent_id is read by the
approval walk and the session listing. TreeCost sums the tree from
messages.cost_usd, so the budget ceiling needs no new accounting."
```

---

### Task 3: A routed call site for child turns

**Files:**
- Modify: `internal/router/router.go:14-27`
- Modify: `internal/agent/agent.go:181-287` (`Run`, `loop`)
- Test: `internal/router/router_test.go`, `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `router.SiteSubagent = "subagent"`; `(*Agent).RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error)`. `Run` keeps its signature and delegates with `router.SiteChat`.

- [ ] **Step 1: Write the failing tests**

In `internal/router/router_test.go`:

```go
func TestSubagentIsAValidSite(t *testing.T) {
	if !ValidSite(SiteSubagent) {
		t.Error("ValidSite(SiteSubagent) = false, want true")
	}
}

func TestRouteSubagentToItsOwnModel(t *testing.T) {
	r, err := New([]config.Route{{When: "subagent", Model: "small"}}, "big")
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Model(SiteSubagent); got != "small" {
		t.Errorf("Model(subagent) = %q, want small", got)
	}
	if got := r.Model(SiteChat); got != "big" {
		t.Errorf("Model(chat) = %q, want big -- the subagent rule must not match chat", got)
	}
}
```

In `internal/agent/agent_test.go`, extend whichever existing test drives a turn against a fake provider so it asserts the persisted `call_site`. Follow the existing fake-provider helper in that file; the assertion to add is:

```go
func TestRunSiteRecordsTheCallSite(t *testing.T) {
	// Build the same Agent the existing turn tests build, then:
	ch, err := a.RunSite(ctx, sessionID, "hello", router.SiteSubagent)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	rows, err := st.Messages(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var sites []string
	for _, r := range rows {
		if r.Role == "assistant" {
			sites = append(sites, r.CallSite)
		}
	}
	if len(sites) == 0 || sites[0] != router.SiteSubagent {
		t.Errorf("assistant call_site = %v, want the subagent site", sites)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/router/ ./internal/agent/ -run 'Subagent|RunSite' -v`
Expected: FAIL — `undefined: SiteSubagent`, `a.RunSite undefined`.

- [ ] **Step 3: Add the site**

In `internal/router/router.go`:

```go
const (
	SiteChat       = "chat"
	SiteCompaction = "compaction"
	SiteTitle      = "title"
	SiteClassify   = "classify"
	// SiteSubagent is a sub-agent's own turns. It is a site of its own so
	// delegated work can be routed to a cheaper model in configuration
	// alone, with no model selection in the sub-agent path.
	SiteSubagent = "subagent"
)

func ValidSite(s string) bool {
	switch s {
	case SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteSubagent:
		return true
	}
	return false
}
```

- [ ] **Step 4: Parameterise the loop's site**

In `internal/agent/agent.go`, `loop` currently names `router.SiteChat` twice (the `Model` lookup and the `appendMessage` call). Thread a site through:

```go
// Run executes a turn as the session's own chat.
func (a *Agent) Run(ctx context.Context, sessionID, input string) (<-chan Event, error) {
	return a.RunSite(ctx, sessionID, input, router.SiteChat)
}

// RunSite is Run with an explicit call site. A sub-agent's turns name
// router.SiteSubagent, so they route -- and are costed in `/usage` -- as the
// delegated work they are rather than as the parent's chat.
func (a *Agent) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	if !router.ValidSite(site) {
		return nil, fmt.Errorf("unknown call site %q", site)
	}
	pctx, cancelPersist := persistCtx(ctx)
	err := a.appendMessage(pctx, sessionID, provider.RoleUser,
		[]provider.Block{{Type: provider.BlockText, Text: input}}, "", "", provider.Usage{}, 0)
	cancelPersist()
	if err != nil {
		return nil, fmt.Errorf("persist user message: %w", err)
	}

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		ctx, turn := sporetrace.StartTurn(ctx, sessionID, "core")
		defer turn.End()
		if err := a.loop(ctx, sessionID, site, out); err != nil {
			turn.RecordError(err)
			out <- Event{Type: EvError, Err: err}
		}
	}()
	return out, nil
}
```

In `loop`, change the signature to `func (a *Agent) loop(ctx context.Context, sessionID, site string, out chan<- Event) error` and replace both `router.SiteChat` uses with `site`. Leave `MaybeCompact` alone — compaction keeps its own site.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/router/ ./internal/agent/ -v 2>&1 | tail -15`
Expected: PASS. Existing turn tests still pass, because `Run` delegates with the site they already assert.

- [ ] **Step 6: Commit**

```bash
git add internal/router internal/agent
git commit -m "feat(agent): give a turn an explicit call site

RunSite is Run with the site named. A sub-agent's turns can then route
to a cheaper model through a [[route]] rule alone, and their cost is
attributed to delegated work rather than to the parent's chat."
```

---

### Task 4: Sub-agent configuration

**Files:**
- Modify: `internal/config/config.go` (the `Config` struct ~line 36, `Default()` ~line 400, validation ~line 529)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `config.SubagentConfig{MaxDepth int; MaxCostUSD float64; MaxConcurrent int}`; `Config.Subagents SubagentConfig` with TOML key `subagents`. Defaults: `MaxDepth: 2`, `MaxCostUSD: 1.00`, `MaxConcurrent: 4`.

- [ ] **Step 1: Write the failing tests**

In `internal/config/config_test.go`:

```go
func TestSubagentDefaults(t *testing.T) {
	c := Default()
	if c.Subagents.MaxDepth != 2 {
		t.Errorf("MaxDepth = %d, want 2", c.Subagents.MaxDepth)
	}
	if c.Subagents.MaxCostUSD != 1.00 {
		t.Errorf("MaxCostUSD = %v, want 1.00", c.Subagents.MaxCostUSD)
	}
	if c.Subagents.MaxConcurrent != 4 {
		t.Errorf("MaxConcurrent = %d, want 4", c.Subagents.MaxConcurrent)
	}
}

func TestSubagentZeroValuesFallBackToDefaults(t *testing.T) {
	// A config file with a [subagents] block that sets only one key must not
	// leave the others at zero: a zero depth would disable sub-agents and a
	// zero ceiling would refuse every spawn, both silently.
	c := Default()
	c.Subagents = SubagentConfig{MaxDepth: 3}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if c.Subagents.MaxDepth != 3 {
		t.Errorf("MaxDepth = %d, want the configured 3", c.Subagents.MaxDepth)
	}
	if c.Subagents.MaxCostUSD != 1.00 || c.Subagents.MaxConcurrent != 4 {
		t.Errorf("unset keys did not fall back: %+v", c.Subagents)
	}
}

func TestSubagentNegativeValuesRejected(t *testing.T) {
	c := Default()
	c.Subagents.MaxDepth = -1
	if err := c.Validate(); err == nil {
		t.Error("Validate accepted a negative max_depth")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run Subagent -v`
Expected: FAIL — `undefined: SubagentConfig`.

- [ ] **Step 3: Add the config block**

```go
// SubagentConfig bounds a tree of agents. maxIterations bounds one agent's
// round trips; none of it bounds a parent that keeps spawning.
type SubagentConfig struct {
	// MaxDepth is how deep the tree may go. The default 2 means a top-level
	// session spawns children and those children may not spawn.
	MaxDepth int `toml:"max_depth"`
	// MaxCostUSD is the ceiling for a whole tree: the root and every
	// descendant, summed. Depth alone does not see a wide flat fan-out.
	MaxCostUSD float64 `toml:"max_cost_usd"`
	// MaxConcurrent is how many children may run at once under one root. An
	// unbounded spawn batch reaches provider rate limits long before it
	// reaches the cost ceiling.
	MaxConcurrent int `toml:"max_concurrent"`
}
```

Add to `Config`: `Subagents SubagentConfig \`toml:"subagents"\``.

In `Default()`: `Subagents: SubagentConfig{MaxDepth: 2, MaxCostUSD: 1.00, MaxConcurrent: 4}`.

In `(*Config).Validate` (`internal/config/config.go:586`), alongside the existing per-block normalisation:

```go
	if c.Subagents.MaxDepth < 0 || c.Subagents.MaxCostUSD < 0 || c.Subagents.MaxConcurrent < 0 {
		return fmt.Errorf("subagents: max_depth, max_cost_usd and max_concurrent must not be negative")
	}
	// Zero means "not set in the file", not "disabled": a partially written
	// [subagents] block must not silently refuse every spawn.
	if c.Subagents.MaxDepth == 0 {
		c.Subagents.MaxDepth = 2
	}
	if c.Subagents.MaxCostUSD == 0 {
		c.Subagents.MaxCostUSD = 1.00
	}
	if c.Subagents.MaxConcurrent == 0 {
		c.Subagents.MaxConcurrent = 4
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ -v 2>&1 | tail -10`
Expected: PASS.

- [ ] **Step 5: Document the block**

Add a `[subagents]` section to the configuration table in `README.md`, next to `[skills]`, with the three keys, their defaults and one line each on what they bound.

- [ ] **Step 6: Commit**

```bash
git add internal/config README.md
git commit -m "feat(config): add the [subagents] limits

Depth, tree cost and concurrency. A zero in any of the three means the
key was absent from the file rather than deliberately disabled, so it
falls back to the default instead of refusing every spawn."
```

---

### Task 5: The supervisor and `agent_run`

**Files:**
- Create: `internal/subagent/supervisor.go`
- Create: `internal/subagent/supervisor_test.go`
- Create: `internal/tool/subagent/tools.go`
- Create: `internal/tool/subagent/tools_test.go`
- Modify: `cmd/spore/wire.go:45-71` (`buildTools`), `cmd/spore/wire.go:160-171` (`buildAgent`)

**Interfaces:**
- Consumes: `store.CreateChildSession`, `store.SessionAncestors`, `store.TreeCost`, `store.StartSubagentRun`, `store.FinishSubagentRun` (Tasks 1-2); `agent.RunSite`, `router.SiteSubagent` (Task 3); `config.SubagentConfig` (Task 4).
- Produces: `subagent.Supervisor` with `New(st *store.Store, cfg config.SubagentConfig) *Supervisor`, `Attach(a *agent.Agent)`, `Run(ctx, parentID, prompt string) (Status, error)`, `List(ctx, parentID string) ([]Status, error)`; `subagent.Status` as defined in the spec; `subagenttool.New(sup *subagent.Supervisor) []tool.Tool` returning the `agent_run` tool (`agent_spawn` and `agent_result` join it in Task 7).

- [ ] **Step 1: Write the failing supervisor tests**

Create `internal/subagent/supervisor_test.go`. The supervisor needs an agent, so the test uses a stub through a narrow interface rather than a real provider:

```go
package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/spore.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// stubRunner answers every child turn with a fixed reply, and records the
// sessions it was asked to run so the tests can assert on parentage.
type stubRunner struct {
	reply string
	ran   []string
}

func (s *stubRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	s.ran = append(s.ran, sessionID)
	ch := make(chan Event, 2)
	ch <- Event{Text: s.reply}
	close(ch)
	return ch, nil
}

func testSupervisor(t *testing.T, cfg config.SubagentConfig, r *stubRunner) (*Supervisor, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	sup := New(st, cfg)
	sup.AttachRunner(r)
	return sup, st
}

func parentSession(t *testing.T, st *store.Store) string {
	t.Helper()
	id, err := st.CreateSession(context.Background(), "parent", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ctxFor(sessionID string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{
		ID: sessionID, Profile: policy.ProfileLocal, Workspace: "/tmp/ws",
	})
}

func TestRunReturnsTheChildsAnswer(t *testing.T) {
	r := &stubRunner{reply: "no findings"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	got, err := sup.Run(ctxFor(parent), parent, "audit the policy tests")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Result != "no findings" {
		t.Errorf("Result = %q, want the child's reply", got.Result)
	}
	if got.State != store.RunDone {
		t.Errorf("State = %q, want done", got.State)
	}

	sess, ok, err := st.Session(context.Background(), got.ID)
	if err != nil || !ok {
		t.Fatalf("child session missing: ok=%v err=%v", ok, err)
	}
	if sess.ParentID != parent {
		t.Errorf("child ParentID = %q, want %q", sess.ParentID, parent)
	}
	if sess.Workspace != "/tmp/ws" {
		t.Errorf("child Workspace = %q, want the parent's", sess.Workspace)
	}
}

func TestRunRefusesBeyondMaxDepth(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	child, err := sup.Run(ctxFor(parent), parent, "first level")
	if err != nil {
		t.Fatalf("depth 1 must be allowed: %v", err)
	}
	if _, err := sup.Run(ctxFor(child.ID), child.ID, "second level"); err == nil {
		t.Fatal("Run at depth 2 was allowed, want a refusal")
	} else if !strings.Contains(err.Error(), "depth") {
		t.Errorf("error = %v, want it to name the depth limit", err)
	}
}

func TestRunRefusesOverTheTreeCostCeiling(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 0.05, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)

	// Spend the ceiling in the parent before any child exists.
	if _, err := st.AppendMessage(context.Background(), store.Message{
		SessionID: parent, Role: "assistant",
		BlocksJSON: []byte(`[{"type":"text","text":"x"}]`), CostUSD: 0.06,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := sup.Run(ctxFor(parent), parent, "too expensive"); err == nil {
		t.Fatal("Run over the ceiling was allowed, want a refusal")
	} else if !strings.Contains(err.Error(), "cost") {
		t.Errorf("error = %v, want it to name the cost ceiling", err)
	}
}

func TestListReportsChildrenOfOneParent(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)
	other := parentSession(t, st)

	if _, err := sup.Run(ctxFor(parent), parent, "mine"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Run(ctxFor(other), other, "theirs"); err != nil {
		t.Fatal(err)
	}

	got, err := sup.List(context.Background(), parent)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Prompt != "mine" {
		t.Errorf("List = %+v, want only this parent's child", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/subagent/ -v`
Expected: FAIL — no such package.

- [ ] **Step 3: Write the supervisor**

Create `internal/subagent/supervisor.go`:

```go
// Package subagent runs one agent on behalf of another. It is the single
// owner of what is running: the tools launch through it, the daemon endpoint
// reads it, and the startup sweep reconciles it with the store.
package subagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// Event is the part of agent.Event the supervisor consumes. Depending on the
// narrow shape rather than on *agent.Agent is what lets the tests drive a
// supervisor without a provider.
type Event = agent.Event

// Runner is the agent seen from here. agent.Agent satisfies it.
type Runner interface {
	RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error)
}

// Status is one child, live or finished. It is what agent_result returns to
// the model and what the endpoint renders.
type Status struct {
	ID      string    `json:"id"`
	Prompt  string    `json:"prompt"`
	State   string    `json:"state"`
	Result  string    `json:"result,omitempty"`
	Error   string    `json:"error,omitempty"`
	Depth   int       `json:"depth"`
	CostUSD float64   `json:"cost_usd"`
	Started time.Time `json:"started_at"`
	Ended   time.Time `json:"ended_at,omitempty"`
}

type child struct {
	cancel context.CancelFunc
	prompt string
	depth  int
	start  time.Time
}

type Supervisor struct {
	mu      sync.Mutex
	running map[string]*child
	runner  Runner
	store   *store.Store
	cfg     config.SubagentConfig
}

func New(st *store.Store, cfg config.SubagentConfig) *Supervisor {
	return &Supervisor{running: map[string]*child{}, store: st, cfg: cfg}
}

// Attach supplies the agent. buildAgent constructs the registry before the
// Agent, so the supervisor is built empty, the tools are registered against
// it, and the agent arrives last.
func (s *Supervisor) Attach(a *agent.Agent) { s.AttachRunner(a) }

// AttachRunner is Attach against the narrow interface, which is what the
// tests use.
func (s *Supervisor) AttachRunner(r Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = r
}

// admit decides whether one more child may start under this parent, and
// returns the depth it would run at. Every refusal is an ordinary error: the
// caller turns it into a tool error the model can read and route around.
func (s *Supervisor) admit(ctx context.Context, parentID string) (int, error) {
	ancestors, err := s.store.SessionAncestors(ctx, parentID)
	if err != nil {
		return 0, err
	}
	depth := len(ancestors) + 1
	if depth > s.cfg.MaxDepth {
		return 0, fmt.Errorf("refusing to launch: this would be depth %d and the configured max_depth is %d. Do the work in this agent instead", depth, s.cfg.MaxDepth)
	}

	root := parentID
	if len(ancestors) > 0 {
		root = ancestors[len(ancestors)-1]
	}
	spent, err := s.store.TreeCost(ctx, root)
	if err != nil {
		return 0, err
	}
	if spent >= s.cfg.MaxCostUSD {
		return 0, fmt.Errorf("refusing to launch: this agent tree has spent $%.4f of its $%.2f max_cost_usd. Do the work in this agent instead", spent, s.cfg.MaxCostUSD)
	}

	s.mu.Lock()
	live := len(s.running)
	s.mu.Unlock()
	if live >= s.cfg.MaxConcurrent {
		return 0, fmt.Errorf("refusing to launch: %d sub-agents are already running and max_concurrent is %d. Wait for one to finish", live, s.cfg.MaxConcurrent)
	}
	return depth, nil
}

// start creates the child session and its run row, and returns the context
// the child turn runs under. The child inherits the parent's profile and
// workspace: inheriting the profile is what stops a sub-agent reaching
// further than whoever launched it, and inheriting the workspace keeps its
// filesystem calls inside the same ceiling.
func (s *Supervisor) start(ctx context.Context, parentID, prompt string, depth int) (string, context.Context, error) {
	parent := policy.SessionFrom(ctx)
	childID, err := s.store.CreateChildSession(ctx, prompt, parent.Workspace, parentID)
	if err != nil {
		return "", nil, err
	}
	err = s.store.StartSubagentRun(ctx, store.SubagentRun{
		SessionID: childID, ParentID: parentID, Prompt: prompt, Depth: depth,
	})
	if err != nil {
		return "", nil, err
	}
	childCtx := policy.WithSession(ctx, policy.Session{
		ID: childID, Profile: parent.Profile, Workspace: parent.Workspace,
	})
	return childID, childCtx, nil
}

// drain consumes a child's turn to completion and returns its final text.
func drain(ch <-chan Event) (string, error) {
	var text string
	var turnErr error
	for ev := range ch {
		switch {
		case ev.Err != nil:
			turnErr = ev.Err
		case ev.Text != "":
			text += ev.Text
		}
	}
	return text, turnErr
}

// Run launches a child and blocks until it settles.
func (s *Supervisor) Run(ctx context.Context, parentID, prompt string) (Status, error) {
	depth, err := s.admit(ctx, parentID)
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	runner := s.runner
	s.mu.Unlock()
	if runner == nil {
		return Status{}, fmt.Errorf("sub-agents are not available: no agent is attached")
	}

	childID, childCtx, err := s.start(ctx, parentID, prompt, depth)
	if err != nil {
		return Status{}, err
	}
	childCtx, cancel := context.WithCancel(childCtx)
	s.track(childID, &child{cancel: cancel, prompt: prompt, depth: depth, start: time.Now().UTC()})
	defer s.untrack(childID)
	defer cancel()

	ch, err := runner.RunSite(childCtx, childID, prompt, router.SiteSubagent)
	if err != nil {
		s.finish(ctx, childID, store.RunFailed, "", err.Error())
		return Status{}, err
	}
	text, turnErr := drain(ch)
	if turnErr != nil {
		s.finish(ctx, childID, store.RunFailed, text, turnErr.Error())
		return Status{}, turnErr
	}
	s.finish(ctx, childID, store.RunDone, text, "")
	return s.Result(ctx, childID)
}

func (s *Supervisor) track(id string, c *child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[id] = c
}

func (s *Supervisor) untrack(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

// finish records the terminal state. It uses a context detached from the
// child's own, because a cancelled child must still record that it stopped.
func (s *Supervisor) finish(ctx context.Context, childID, state, result, errText string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.FinishSubagentRun(writeCtx, childID, state, result, errText); err != nil {
		// The transcript is the record; a lost bookkeeping row costs the
		// listing accuracy, never the work.
		_ = err
	}
}

// Result reads one child's status, live or finished.
func (s *Supervisor) Result(ctx context.Context, childID string) (Status, error) {
	run, ok, err := s.store.SubagentRun(ctx, childID)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, fmt.Errorf("no sub-agent %s", childID)
	}
	return s.statusOf(ctx, run)
}

func (s *Supervisor) statusOf(ctx context.Context, run store.SubagentRun) (Status, error) {
	cost, err := s.store.TreeCost(ctx, run.SessionID)
	if err != nil {
		return Status{}, err
	}
	return Status{
		ID: run.SessionID, Prompt: run.Prompt, State: run.State,
		Result: run.Result, Error: run.Error, Depth: run.Depth,
		CostUSD: cost, Started: run.StartedAt, Ended: run.EndedAt,
	}, nil
}

// List reports one parent's children, running and finished.
func (s *Supervisor) List(ctx context.Context, parentID string) ([]Status, error) {
	runs, err := s.store.SubagentRunsByParent(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(runs))
	for _, r := range runs {
		st, err := s.statusOf(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}
```

- [ ] **Step 4: Run the supervisor tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/subagent/ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Write the failing tool test**

Create `internal/tool/subagent/tools_test.go`:

```go
package subagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentRunToolIsNotReadOnly(t *testing.T) {
	for _, tl := range New(nil) {
		if tl.ReadOnly() {
			t.Errorf("%s reports ReadOnly, but a child mutates and must not join a parallel batch", tl.Name())
		}
	}
}

func TestAgentRunRequiresAPrompt(t *testing.T) {
	tools := New(nil)
	var run tool.Tool
	for _, tl := range tools {
		if tl.Name() == "agent_run" {
			run = tl
		}
	}
	if run == nil {
		t.Fatal("New did not return agent_run")
	}
	if _, err := run.Call(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("agent_run accepted an empty prompt")
	} else if !strings.Contains(err.Error(), "prompt") {
		t.Errorf("error = %v, want it to name the missing prompt", err)
	}
}
```

Add the `tool` import. Note the supervisor is `nil` here on purpose: both tests must fail before reaching it.

- [ ] **Step 6: Write the tool**

Create `internal/tool/subagent/tools.go`:

```go
// Package subagent exposes the sub-agent supervisor as tools. It holds no
// state: internal/subagent is the single owner of what is running.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/subagent"
	"github.com/codered/spore/internal/tool"
)

// New returns the sub-agent tools. Every one reports ReadOnly() == false: a
// child mutates, so it must never be dispatched inside the loop's parallel
// read-only batch.
func New(sup *subagent.Supervisor) []tool.Tool {
	return []tool.Tool{&runTool{sup: sup}}
}

type runTool struct{ sup *subagent.Supervisor }

func (t *runTool) Name() string { return "agent_run" }

func (t *runTool) Description() string {
	return "Run a sub-agent on a self-contained task and wait for its answer. " +
		"Use this to keep a long, noisy investigation out of your own context: " +
		"the sub-agent reads what it needs and you get only its conclusion. " +
		"Give it one complete instruction -- it cannot see this conversation."
}

func (t *runTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "prompt": {
	      "type": "string",
	      "description": "The complete, self-contained task for the sub-agent."
	    }
	  },
	  "required": ["prompt"]
	}`)
}

func (t *runTool) ReadOnly() bool { return false }

func (t *runTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_run: %w", err)
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("agent_run: prompt is required")
	}
	sess := policy.SessionFrom(ctx)
	if sess.ID == "" {
		return "", fmt.Errorf("agent_run: no session on the context")
	}
	st, err := t.sup.Run(ctx, sess.ID, in.Prompt)
	if err != nil {
		return "", err
	}
	if st.Result == "" {
		return fmt.Sprintf("sub-agent %s finished without a reply", st.ID), nil
	}
	return st.Result, nil
}
```

- [ ] **Step 7: Run the tool tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/tool/subagent/ -v`
Expected: PASS (2 tests).

- [ ] **Step 8: Wire it into buildAgent**

In `cmd/spore/wire.go`, `buildTools` gains a `sup *subagent.Supervisor` parameter and registers the tools:

```go
	tools = append(tools, skill.New(cfg, skillsCache)...)
	tools = append(tools, subagenttool.New(sup)...)
```

with the import aliased `subagenttool "github.com/codered/spore/internal/tool/subagent"`, and in `buildAgent`, around the existing construction:

```go
	// The supervisor is built before the tools that launch through it and
	// receives the agent after: buildAgent constructs the registry first, so
	// the cycle is closed by Attach rather than by construction order.
	sup := subagent.New(st, cfg.Subagents)
	tools, host, err := buildTools(cfg, st, facts, recallBackend, skillsCache, sup, approver)
	if err != nil {
		return nil, nil, nil, err
	}
	a := agent.New(st, reg, rt, cfg, tools)
	a.Facts = facts
	a.Skills = skillsCache
	a.Env = workspace.NewDescribers().Describe
	sup.Attach(a)
```

`buildAgent` returns the supervisor too, so the daemon can read it in Task 8. Update its signature to `(*agent.Agent, *subagent.Supervisor, *mcphost.Host, *mirror.Mirror, error)` and fix the call site at `cmd/spore/wire.go:180`.

- [ ] **Step 9: Verify the whole build and suite**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./... 2>&1 | tail -25`
Expected: build clean, vet clean, all tests PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/subagent internal/tool/subagent cmd/spore
git commit -m "feat(subagent): add the supervisor and agent_run

One supervisor owns what is running; the tool is a thin client of it,
which is what lets the daemon endpoint and the startup sweep read the
same object. A child inherits the parent's profile and workspace and
gets its own session id, so it can reach no further than its launcher
while staying individually auditable. Depth, tree cost and concurrency
are checked before launch and refuse as tool errors the model can route
around."
```

---

### Task 6: Approvals across the parent chain

**Files:**
- Modify: `internal/policy/guard.go:317-368` (`Pending`, `Resolve`)
- Test: `internal/policy/subagent_test.go` (new file)

**Interfaces:**
- Consumes: `store.SessionAncestors` (Task 1).
- Produces: `(*Guard).PendingTree(ctx context.Context, sessionID string) ([]store.PendingCall, error)`; `Resolve` accepting an ancestor of the pending row's session; `rootOf(ctx, store, sessionID) (string, error)` used for remembered decisions.

- [ ] **Step 1: Write the failing tests**

Create `internal/policy/subagent_test.go`. Build the engine from an explicit `config.PolicyConfig`, never `config.Default()`:

```go
package policy

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// askEverything is a ruleset written out in full. Building it from
// config.Default() would add the baseline deny set, which silently satisfies
// the assertions these tests exist to make.
func askEverything(t *testing.T, workspace string) *Engine {
	t.Helper()
	e, err := NewEngine(config.PolicyConfig{
		Default:   "ask",
		Workspace: workspace,
		Allow:     []string{},
		Ask:       []string{"*"},
		Deny:      []string{},
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	return e
}

func treeStore(t *testing.T) (*store.Store, string, string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/spore.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, err := st.CreateSession(ctx, "root", "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	childA, err := st.CreateChildSession(ctx, "a", "/tmp/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	childB, err := st.CreateChildSession(ctx, "b", "/tmp/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	return st, root, childA, childB
}

func pendingIn(t *testing.T, st *store.Store, sessionID string) int64 {
	t.Helper()
	id, err := st.AddPendingCall(context.Background(), store.PendingCall{
		SessionID: sessionID, ToolUseID: "tu-" + sessionID, Tool: "shell_exec",
		Profile: string(ProfileLocal), Rule: "*", ArgsJSON: []byte(`{"cmd":"ls"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPendingTreeIncludesDescendants(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	pendingIn(t, st, root)
	pendingIn(t, st, childA)

	own, err := g.Pending(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 1 {
		t.Errorf("Pending = %d rows, want only the root's own", len(own))
	}

	all, err := g.PendingTree(ctx, root)
	if err != nil {
		t.Fatalf("PendingTree: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("PendingTree = %d rows, want the root's and the child's", len(all))
	}
}

func TestParentMayAnswerAChildsApproval(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	if err := g.Resolve(ctx, root, id, Answer{Allow: true, Scope: ScopeOnce}); err != nil {
		t.Errorf("the parent could not answer its child's approval: %v", err)
	}
}

func TestChildMayNotAnswerItsOwnApproval(t *testing.T) {
	ctx := context.Background()
	st, _, childA, _ := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	err := g.Resolve(ctx, childA, id, Answer{Allow: true, Scope: ScopeOnce})
	if err == nil {
		t.Fatal("a sub-agent answered its own approval")
	}
	if !strings.Contains(err.Error(), "own approval") {
		t.Errorf("error = %v, want it to say a sub-agent cannot answer its own approval", err)
	}
}

func TestSiblingMayNotAnswerAnothersApproval(t *testing.T) {
	ctx := context.Background()
	st, _, childA, childB := treeStore(t)
	g := NewGuard(&recordingRunner{}, askEverything(t, "/tmp/ws"), &scriptedApprover{}, st, nil)

	id := pendingIn(t, st, childA)
	if err := g.Resolve(ctx, childB, id, Answer{Allow: true, Scope: ScopeOnce}); err == nil {
		t.Fatal("a sibling answered another sub-agent's approval")
	}
}

func TestRememberedDecisionsAreRootScoped(t *testing.T) {
	ctx := context.Background()
	st, root, childA, _ := treeStore(t)

	// The human allowed shell_exec for the session earlier.
	if err := st.RecordApproval(ctx, root, "shell_exec", []byte(`{}`), string(DecisionAllow), string(ScopeSession)); err != nil {
		t.Fatal(err)
	}

	got, ok, err := rootScopedDecision(ctx, st, childA, "shell_exec")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != string(DecisionAllow) {
		t.Errorf("child decision = (%q, %v), want the root's allow", got, ok)
	}
	_ = root
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run 'PendingTree|Answer|Sibling|RootScoped' -v`
Expected: FAIL — `g.PendingTree undefined`, `rootScopedDecision undefined`.

- [ ] **Step 3: Add the ancestry helpers**

In `internal/policy/guard.go`:

```go
// rootOf returns the top of a session's parent chain, which is the session a
// human is attached to. A top-level session is its own root.
func rootOf(ctx context.Context, st *store.Store, sessionID string) (string, error) {
	ancestors, err := st.SessionAncestors(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if len(ancestors) == 0 {
		return sessionID, nil
	}
	return ancestors[len(ancestors)-1], nil
}

// isAncestor reports whether answerer sits above target in the parent chain.
// The walk is upward only, which is what stops a sub-agent answering its own
// approval or a sibling's.
func isAncestor(ctx context.Context, st *store.Store, answerer, target string) (bool, error) {
	ancestors, err := st.SessionAncestors(ctx, target)
	if err != nil {
		return false, err
	}
	for _, a := range ancestors {
		if a == answerer {
			return true, nil
		}
	}
	return false, nil
}

// rootScopedDecision looks up a remembered "always this session" answer under
// the root of the chain, so one answer covers a whole agent tree. Per-child
// scoping was rejected: a fan-out would ask once per child for the same tool,
// and a detached child has nobody attached to answer at all, so its ask would
// run out the approval timeout and deny -- turning background work into
// silent failure. The containment for the wider scope is the trust profile
// the whole tree shares, and the baseline deny set no approval overrides.
func rootScopedDecision(ctx context.Context, st *store.Store, sessionID, tool string) (string, bool, error) {
	root, err := rootOf(ctx, st, sessionID)
	if err != nil {
		return "", false, err
	}
	return st.SessionDecision(ctx, root, tool)
}
```

- [ ] **Step 4: Use the root scope in `Run`**

In `Guard.Run`, replace the remembered-decision lookup:

```go
	if remembered, ok, err := rootScopedDecision(ctx, g.store, sess.ID, call.Name); err == nil && ok {
```

leaving the two branches under it unchanged.

- [ ] **Step 5: Add `PendingTree`**

```go
// PendingTree returns a session's own pending approvals together with those
// of its descendants. A child's ask must reach the clients a human actually
// has attached, and those are attached to the root of the chain, not to the
// child.
func (g *Guard) PendingTree(ctx context.Context, sessionID string) ([]store.PendingCall, error) {
	return g.store.PendingCallsTree(ctx, sessionID)
}
```

and in `internal/store/store.go`, beside `PendingCalls`:

```go
// PendingCallsTree is PendingCalls over a session and every descendant.
func (s *Store) PendingCallsTree(ctx context.Context, sessionID string) ([]PendingCall, error) {
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE tree(id) AS (
		  SELECT id FROM sessions WHERE id = ?
		  UNION
		  SELECT s.id FROM sessions s JOIN tree t ON s.parent_id = t.id
		)
		SELECT p.id, p.session_id, p.tool_use_id, p.tool, p.args, p.profile, p.rule, p.created_at
		FROM pending_calls p JOIN tree ON p.session_id = tree.id
		WHERE p.state = 'pending' ORDER BY p.id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read pending calls for tree: %w", err)
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
```

- [ ] **Step 6: Authorise an ancestor in `Resolve`**

Insert at the top of `Resolve`, before the scope correction. The `ClaimPendingCall` SQL is **not** loosened; authorization happens in front of it and the claim then uses the pending row's own session id:

```go
	// A child's approval belongs to the child's session, but the human
	// answering it is attached to an ancestor. Authorise the answerer here
	// and then claim with the row's own session id: the claim stays exactly
	// as atomic against a double answer as it was, and parentage is
	// immutable once written, so there is no time-of-check gap.
	owner := sessionID
	if p, found, err := g.store.PendingCallByID(ctx, pendingID); err == nil && found && p.SessionID != sessionID {
		ok, err := isAncestor(ctx, g.store, sessionID, p.SessionID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("session %s may not answer approvals for session %s", sessionID, p.SessionID)
		}
		owner = p.SessionID
	} else if found && p.SessionID == sessionID {
		// A sub-agent must never approve its own call: that is the lever a
		// prompt injection reaches for, and the whole point of routing the
		// ask to a human above it.
		if ancestors, err := g.store.SessionAncestors(ctx, sessionID); err == nil && len(ancestors) > 0 {
			return fmt.Errorf("a sub-agent may not answer its own approval")
		}
	}
```

Then replace `sessionID` with `owner` in the `ClaimPendingCall` call, and leave everything else in `Resolve` untouched.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -v 2>&1 | tail -20`
Expected: PASS, including the five new tests and every pre-existing guard, resume and rule test.

- [ ] **Step 8: Surface child asks on the parent's clients**

In `internal/daemon`, `handleListApprovals` calls `Pending`. Change it to `PendingTree` so a child's ask reaches the human, and include the originating session in the JSON so the client can label it:

```go
	// A child's ask carries its own session id. The client shows it so the
	// human can see they are answering for a sub-agent, not for the
	// conversation in front of them.
	Origin string `json:"origin_session,omitempty"`
```

Set `Origin` only when the pending row's session differs from the requested one. In the TUI's approval prompt, prefix the line with `sub-agent <short-id>:` when `Origin` is set.

- [ ] **Step 9: Verify the whole suite**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./... 2>&1 | tail -25`
Expected: build clean, vet clean, all PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/policy internal/store internal/daemon cmd/spore
git commit -m "feat(policy): route a sub-agent's approvals to its human

A child's ask belongs to the child's session; the human answering it is
attached to an ancestor. Authorisation happens in front of
ClaimPendingCall rather than by loosening its SQL, so the claim stays
atomic against double answers, and parentage is immutable so there is no
time-of-check gap. The walk is upward only: a sub-agent can answer
neither its own approval nor a sibling's. Remembered session decisions
are looked up at the root, so one answer covers a tree."
```

---

### Task 7: `agent_spawn`, `agent_result` and the startup sweep

**Files:**
- Modify: `internal/subagent/supervisor.go`
- Modify: `internal/subagent/supervisor_test.go`
- Modify: `internal/tool/subagent/tools.go`
- Modify: `cmd/spore/serve.go` (the daemon's startup path)

**Interfaces:**
- Consumes: everything from Tasks 1-6.
- Produces: `(*Supervisor).Spawn(ctx, parentID, prompt string) (string, error)`; `(*Supervisor).Cancel(ctx, childID string) error`; `(*Supervisor).SweepOrphans(ctx context.Context) (int64, error)`; `(*Supervisor).AllowDetached(bool)`; the `agent_spawn` and `agent_result` tools from `New`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/subagent/supervisor_test.go`:

```go
func TestSpawnReturnsBeforeTheChildFinishes(t *testing.T) {
	release := make(chan struct{})
	r := &blockingRunner{release: release, reply: "eventually"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, nil)
	sup.AttachRunner(r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	id, err := sup.Spawn(ctxFor(parent), parent, "background work")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	got, err := sup.Result(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.RunRunning {
		t.Errorf("State = %q immediately after Spawn, want running", got.State)
	}

	close(release)
	waitForState(t, sup, id, store.RunDone)
}

func TestSpawnSurvivesTheParentTurnEnding(t *testing.T) {
	release := make(chan struct{})
	r := &blockingRunner{release: release, reply: "done later"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, nil)
	sup.AttachRunner(r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	// The parent's turn context is cancelled the moment Spawn returns, which
	// is what happens when the turn that called agent_spawn ends.
	turnCtx, cancelTurn := context.WithCancel(ctxFor(parent))
	id, err := sup.Spawn(turnCtx, parent, "background work")
	if err != nil {
		t.Fatal(err)
	}
	cancelTurn()

	close(release)
	waitForState(t, sup, id, store.RunDone)
	got, _ := sup.Result(context.Background(), id)
	if got.Result != "done later" {
		t.Errorf("Result = %q, want the child's reply -- the child died with its parent's turn", got.Result)
	}
}

func TestSpawnRefusedWhenDetachedIsNotAllowed(t *testing.T) {
	r := &stubRunner{reply: "ok"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, r)
	parent := parentSession(t, st)
	// AllowDetached defaults to false: a one-shot CLI process has nothing to
	// collect a detached result.
	if _, err := sup.Spawn(ctxFor(parent), parent, "background"); err == nil {
		t.Fatal("Spawn was allowed outside the daemon")
	} else if !strings.Contains(err.Error(), "agent_run") {
		t.Errorf("error = %v, want it to point at agent_run", err)
	}
}

func TestCancelStopsAChildAndRecordsIt(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	r := &blockingRunner{release: release, reply: "never"}
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, nil)
	sup.AttachRunner(r)
	sup.AllowDetached(true)
	parent := parentSession(t, st)

	id, err := sup.Spawn(ctxFor(parent), parent, "long job")
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Cancel(context.Background(), id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitForState(t, sup, id, store.RunInterrupted)
}

func TestSweepOrphansMarksRunsFromAPreviousProcess(t *testing.T) {
	ctx := context.Background()
	sup, st := testSupervisor(t, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4}, &stubRunner{})
	parent := parentSession(t, st)
	orphan, err := st.CreateChildSession(ctx, "orphan", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartSubagentRun(ctx, store.SubagentRun{
		SessionID: orphan, ParentID: parent, Prompt: "from a dead process", Depth: 1,
	}); err != nil {
		t.Fatal(err)
	}

	n, err := sup.SweepOrphans(ctx)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d, want 1", n)
	}
	got, _ := sup.Result(ctx, orphan)
	if got.State != store.RunInterrupted {
		t.Errorf("orphan state = %q, want interrupted", got.State)
	}
}
```

Add the helpers to the same file:

```go
// blockingRunner holds its turn open until release is closed, or until the
// child's context is cancelled.
type blockingRunner struct {
	release chan struct{}
	reply   string
}

func (b *blockingRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error) {
	ch := make(chan Event, 2)
	go func() {
		defer close(ch)
		select {
		case <-b.release:
			ch <- Event{Text: b.reply}
		case <-ctx.Done():
			ch <- Event{Err: ctx.Err()}
		}
	}()
	return ch, nil
}

func waitForState(t *testing.T, sup *Supervisor, id, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := sup.Result(context.Background(), id)
		if err == nil && got.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	got, _ := sup.Result(context.Background(), id)
	t.Fatalf("state = %q after 2s, want %q", got.State, want)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/subagent/ -run 'Spawn|Cancel|Sweep' -v`
Expected: FAIL — `sup.Spawn undefined`.

- [ ] **Step 3: Add detachment to the supervisor**

In `internal/subagent/supervisor.go`, add the field and its setter:

```go
	// detached reports whether agent_spawn is available. Only the daemon sets
	// it: a one-shot process has nothing to collect a detached result, and
	// letting one write `running` rows would also make the daemon's startup
	// sweep unable to tell a live run from an orphan.
	detached bool
```

```go
func (s *Supervisor) AllowDetached(ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detached = ok
}
```

Then:

```go
// Spawn launches a child that outlives the caller's turn and returns its id.
func (s *Supervisor) Spawn(ctx context.Context, parentID, prompt string) (string, error) {
	s.mu.Lock()
	allowed, runner := s.detached, s.runner
	s.mu.Unlock()
	if !allowed {
		return "", fmt.Errorf("agent_spawn needs the spore daemon; nothing here would collect the result. Use agent_run instead and wait for the answer")
	}
	if runner == nil {
		return "", fmt.Errorf("sub-agents are not available: no agent is attached")
	}
	depth, err := s.admit(ctx, parentID)
	if err != nil {
		return "", err
	}
	childID, childCtx, err := s.start(ctx, parentID, prompt, depth)
	if err != nil {
		return "", err
	}

	// The child must not inherit the parent turn's context: that context is
	// cancelled the moment the turn ends, which is exactly what detached work
	// is defined not to be bound by.
	bg, cancel := context.WithCancel(context.WithoutCancel(childCtx))
	s.track(childID, &child{cancel: cancel, prompt: prompt, depth: depth, start: time.Now().UTC()})

	go func() {
		defer cancel()
		defer s.untrack(childID)
		ch, err := runner.RunSite(bg, childID, prompt, router.SiteSubagent)
		if err != nil {
			s.finish(context.Background(), childID, store.RunFailed, "", err.Error())
			return
		}
		text, turnErr := drain(ch)
		switch {
		case turnErr != nil && bg.Err() != nil:
			s.finish(context.Background(), childID, store.RunInterrupted, text, "cancelled")
		case turnErr != nil:
			s.finish(context.Background(), childID, store.RunFailed, text, turnErr.Error())
		default:
			s.finish(context.Background(), childID, store.RunDone, text, "")
		}
	}()
	return childID, nil
}

// NOTE added during execution of tasks 1-6: the cost ceiling has a TOCTOU that
// the concurrency cap no longer has. Supervisor.admit reads TreeCost with no
// lock held, while tryTrack counts and reserves max_concurrent atomically under
// s.mu. That is harmless in tasks 1-6 because agent_run reports
// ReadOnly() == false, so the agent loop serialises it and one agent can never
// have two launches in flight. agent_spawn introduces exactly the true
// background concurrency that makes it reachable, so fold the cost check into
// the same lock tryTrack already holds as part of THIS task.

// NOTE added during execution of tasks 1-6: FinishSubagentRun returns only an
// error, so a cancel cannot tell "I moved this row to terminal" from "a
// natural completion beat me to it". Nothing in tasks 1-6 consumes that
// distinction, so it was left alone there. Cancel is its first real consumer:
// give FinishSubagentRun a (moved bool, err error) return as part of THIS
// task, the way ClaimPendingCall already reports who won the race, and have
// Cancel report accurately rather than assuming its cancel took effect.

// Cancel stops a running child. Cancelling is a human action, so it is not a
// tool: it arrives from the daemon endpoint or the CLI.
func (s *Supervisor) Cancel(ctx context.Context, childID string) error {
	s.mu.Lock()
	c, live := s.running[childID]
	s.mu.Unlock()
	if !live {
		return fmt.Errorf("sub-agent %s is not running", childID)
	}
	c.cancel()
	return nil
}

// SweepOrphans marks runs left running by a previous process. The daemon
// calls it once at startup, and it is the daemon's alone: agent_spawn is
// daemon-only, so no other live process can own a running row.
func (s *Supervisor) SweepOrphans(ctx context.Context) (int64, error) {
	return s.store.InterruptRunningSubagents(ctx)
}
```

`context.WithoutCancel` keeps the policy session value on the context while dropping the parent's cancellation, which is exactly what the child needs: same trust, independent lifetime.

- [ ] **Step 4: Run the supervisor tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/subagent/ -v`
Expected: PASS (9 tests).

- [ ] **Step 5: Add the two tools**

In `internal/tool/subagent/tools.go`, extend `New`:

```go
func New(sup *subagent.Supervisor) []tool.Tool {
	return []tool.Tool{&runTool{sup: sup}, &spawnTool{sup: sup}, &resultTool{sup: sup}}
}

type spawnTool struct{ sup *subagent.Supervisor }

func (t *spawnTool) Name() string { return "agent_spawn" }

func (t *spawnTool) Description() string {
	return "Start a sub-agent on a self-contained task and return immediately " +
		"with its id, without waiting. Use this for work that should continue " +
		"while you carry on -- collect the answer later with agent_result. " +
		"Give it one complete instruction: it cannot see this conversation."
}

func (t *spawnTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "prompt": {
	      "type": "string",
	      "description": "The complete, self-contained task for the sub-agent."
	    }
	  },
	  "required": ["prompt"]
	}`)
}

func (t *spawnTool) ReadOnly() bool { return false }

func (t *spawnTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_spawn: %w", err)
	}
	if in.Prompt == "" {
		return "", fmt.Errorf("agent_spawn: prompt is required")
	}
	sess := policy.SessionFrom(ctx)
	if sess.ID == "" {
		return "", fmt.Errorf("agent_spawn: no session on the context")
	}
	id, err := t.sup.Spawn(ctx, sess.ID, in.Prompt)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("started sub-agent %s; collect it with agent_result", id), nil
}

type resultTool struct{ sup *subagent.Supervisor }

func (t *resultTool) Name() string { return "agent_result" }

func (t *resultTool) Description() string {
	return "Read a sub-agent's state and, once it has finished, its answer. " +
		"Call it with the id agent_spawn returned. A sub-agent that is still " +
		"running reports running and has no answer yet."
}

func (t *resultTool) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "id": {"type": "string", "description": "The sub-agent id agent_spawn returned."}
	  },
	  "required": ["id"]
	}`)
}

// ReadOnly is false even though this only reads: it must not join a parallel
// batch alongside the spawns whose ids it reports on.
func (t *resultTool) ReadOnly() bool { return false }

func (t *resultTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("agent_result: %w", err)
	}
	if in.ID == "" {
		return "", fmt.Errorf("agent_result: id is required")
	}
	st, err := t.sup.Result(ctx, in.ID)
	if err != nil {
		return "", err
	}
	switch st.State {
	case "running":
		return fmt.Sprintf("sub-agent %s is still running (%s so far)", st.ID, time.Since(st.Started).Round(time.Second)), nil
	case "done":
		if st.Result == "" {
			return fmt.Sprintf("sub-agent %s finished without a reply", st.ID), nil
		}
		return st.Result, nil
	default:
		return fmt.Sprintf("sub-agent %s %s: %s", st.ID, st.State, st.Error), nil
	}
}
```

- [ ] **Step 6: Sweep and enable detachment in the daemon**

In `cmd/spore/serve.go`, after the supervisor exists and before the server starts serving:

```go
	// agent_spawn is available only here: a one-shot process has nothing to
	// collect a detached result, and this is what makes the sweep below
	// unambiguous -- no other live process owns a running row.
	sup.AllowDetached(true)
	if n, err := sup.SweepOrphans(ctx); err != nil {
		slog.Default().Warn("could not reconcile sub-agent runs from a previous process", "error", err)
	} else if n > 0 {
		slog.Default().Info("marked sub-agent runs from a previous process interrupted", "count", n)
	}
```

- [ ] **Step 7: Verify the whole suite**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./... 2>&1 | tail -25`
Expected: build clean, vet clean, all PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/subagent internal/tool/subagent cmd/spore
git commit -m "feat(subagent): add agent_spawn, agent_result and the sweep

A detached child runs on a context stripped of the parent turn's
cancellation but keeping its policy session: same trust, independent
lifetime. Spawn is daemon-only, which is also what makes the startup
sweep safe -- with no other live process owning a running row, a
running row at startup is an orphan and nothing else."
```

---

### Task 8: The `/agents` endpoint

**Files:**
- Create: `internal/daemon/agents.go`
- Create: `internal/daemon/agents_test.go`
- Modify: `internal/daemon/server.go:85-103` (routes, `Server` struct)
- Modify: `cmd/spore/serve.go` (pass the supervisor to the server)

**Interfaces:**
- Consumes: `subagent.Supervisor.List`, `.Cancel`, `.Status` (Tasks 5, 7).
- Produces: `GET /api/sessions/{id}/agents` returning `{"agents": [...]}`; `DELETE /api/sessions/{id}/agents/{child}` returning 204; `Server.subagents *subagent.Supervisor`.

- [ ] **Step 1: Write the failing test**

Create `internal/daemon/agents_test.go`, following the existing daemon test helpers (read `internal/daemon/skills_test.go` for how a test server is built and adapt):

```go
package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAgentsEndpointListsChildren(t *testing.T) {
	// Build the test server the way the skills tests do, with a supervisor
	// attached, then create a parent session and one finished child run.
	srv, st := newTestServer(t)
	parent := createTestSession(t, st)
	child := createTestChild(t, st, parent, "audit the tests", "done", "no findings")

	req := httptest.NewRequest("GET", "/api/sessions/"+parent+"/agents", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Agents []struct {
			ID     string `json:"id"`
			Prompt string `json:"prompt"`
			State  string `json:"state"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(got.Agents))
	}
	if got.Agents[0].ID != child || got.Agents[0].State != "done" {
		t.Errorf("agent = %+v, want the finished child %s", got.Agents[0], child)
	}
}

func TestCancelUnknownAgentIsNotFound(t *testing.T) {
	srv, st := newTestServer(t)
	parent := createTestSession(t, st)

	req := httptest.NewRequest("DELETE", "/api/sessions/"+parent+"/agents/nope", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
```

Write `createTestChild` in the same file using `store.CreateChildSession`, `StartSubagentRun` and `FinishSubagentRun`. Adapt `newTestServer`/`createTestSession` to whatever the existing daemon tests actually name.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run Agents -v`
Expected: FAIL — 404 from an unregistered route.

- [ ] **Step 3: Write the handlers**

Create `internal/daemon/agents.go`:

```go
package daemon

import (
	"net/http"

	"github.com/codered/spore/internal/subagent"
)

// AgentsJSON is the /agents response. Live rows come from the supervisor,
// finished ones from the store, and both render the same shape.
type AgentsJSON struct {
	Agents []subagent.Status `json:"agents"`
}

// handleAgents lists the sub-agents a session has launched: what each was
// asked, whether it is still going, how long it has run and what it has cost.
// It polls rather than streaming -- a child's turn deltas are deliberately
// kept off the parent's stream, since keeping that noise out of the parent is
// the point of delegating in the first place.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if s.subagents == nil {
		writeJSON(w, http.StatusOK, AgentsJSON{Agents: []subagent.Status{}})
		return
	}
	list, err := s.subagents.List(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list sub-agents: %v", err)
		return
	}
	if list == nil {
		list = []subagent.Status{}
	}
	writeJSON(w, http.StatusOK, AgentsJSON{Agents: list})
}

// handleCancelAgent stops one running sub-agent. Cancelling is a human
// action, which is why it is an endpoint and not a tool the model can call.
func (s *Server) handleCancelAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	childID := r.PathValue("child")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	if s.subagents == nil {
		writeError(w, http.StatusNotFound, "no sub-agent %s", childID)
		return
	}
	if err := s.subagents.Cancel(r.Context(), childID); err != nil {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 4: Register the routes and the field**

In `internal/daemon/server.go`, add `subagents *subagent.Supervisor` to `Server` and a setter or constructor parameter matching how `agent` is already supplied. Then:

```go
	mux.HandleFunc("GET /api/sessions/{id}/agents", s.handleAgents)
	mux.HandleFunc("DELETE /api/sessions/{id}/agents/{child}", s.handleCancelAgent)
```

In `cmd/spore/serve.go`, pass the supervisor built in `buildAgent` to the server the same way the agent is passed.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon cmd/spore
git commit -m "feat(daemon): serve and cancel sub-agents

GET lists what a session launched; DELETE stops one. Both read the same
supervisor the tools launch through. The listing polls rather than
streaming: a child's deltas are kept off the parent's stream, because
keeping that noise out of the parent is the point of delegating."
```

---

### Task 9: The `/agents` command

**Files:**
- Modify: `cmd/spore/client.go:150-156` (beside `listSkills`)
- Modify: `cmd/spore/chat.go:72-88` (`slashHandler`), `cmd/spore/chat.go:126-137` (`runPlainSlash`)
- Modify: `cmd/spore/tui.go` (a `handleAgents` beside `handleSkills`, and the slash-command hint list)
- Test: `cmd/spore/chat_test.go` or wherever `formatSkills` is tested

**Interfaces:**
- Consumes: `GET /api/sessions/{id}/agents` (Task 8).
- Produces: `client.listAgents(ctx, sessionID) (agentListJSON, error)`; `formatAgents(agentListJSON) string`; `/agents` handled in both the TUI and the plain loop.

- [ ] **Step 1: Write the failing test**

In the same test file that covers `formatSkills`:

```go
func TestFormatAgentsShowsStateAndCost(t *testing.T) {
	got := formatAgents(agentListJSON{Agents: []subagent.Status{
		{ID: "abc123", Prompt: "audit the policy tests", State: "running", CostUSD: 0.021, Started: time.Now()},
		{ID: "def456", Prompt: "summarise the backlog", State: "done", CostUSD: 0.004, Started: time.Now()},
	}})
	for _, want := range []string{"abc123", "audit the policy tests", "running", "done", "0.02"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatAgents output is missing %q:\n%s", want, got)
		}
	}
}

func TestFormatAgentsWhenThereAreNone(t *testing.T) {
	got := formatAgents(agentListJSON{})
	if !strings.Contains(got, "no sub-agents") {
		t.Errorf("empty listing = %q, want it to say there are none", got)
	}
}

func TestPlainSlashHandlesAgents(t *testing.T) {
	// runPlainSlash reports whether it consumed the line. /agents must be
	// consumed in the non-TTY loop too: a command that works only in the TUI
	// was one of the defects found after the last command shipped.
	if handled, _ := runPlainSlash(context.Background(), nil, "sess", "/agents", io.Discard); !handled {
		t.Error("runPlainSlash did not consume /agents")
	}
}
```

The third test passes a nil client, so it will reach a nil dereference unless `runPlainSlash` is structured to return `handled=true` alongside the error. Match `runPlainSlash`'s existing shape: it returns `(true, err)` for a command it owns.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run 'Agents' -v`
Expected: FAIL — `undefined: formatAgents`.

- [ ] **Step 3: Add the client call**

In `cmd/spore/client.go`, beside `listSkills`:

```go
func (c *client) listAgents(ctx context.Context, sessionID string) (agentListJSON, error) {
	var out agentListJSON
	if err := c.do(ctx, "GET", "/api/sessions/"+sessionID+"/agents", nil, &out); err != nil {
		return agentListJSON{}, err
	}
	return out, nil
}
```

with `type agentListJSON = daemon.AgentsJSON` beside the existing `skillListJSON` declaration.

- [ ] **Step 4: Add the formatter**

Beside `formatSkills`:

```go
// formatAgents renders the /agents listing. It is shared by the TUI and the
// plain loop so both surfaces say the same thing.
func formatAgents(list agentListJSON) string {
	if len(list.Agents) == 0 {
		return "  no sub-agents in this session\n"
	}
	var b strings.Builder
	for _, a := range list.Agents {
		elapsed := time.Since(a.Started).Round(time.Second)
		if !a.Ended.IsZero() {
			elapsed = a.Ended.Sub(a.Started).Round(time.Second)
		}
		fmt.Fprintf(&b, "  %s  %-9s  %6s  $%.4f  %s\n",
			a.ID, a.State, elapsed, a.CostUSD, a.Prompt)
	}
	return b.String()
}
```

- [ ] **Step 5: Handle the command on both surfaces**

In `cmd/spore/tui.go`, beside `handleSkills`:

```go
func (m *chatUI) handleAgents(ctx context.Context, c *client, sessionID string) tea.Cmd {
	return tea.Sequence(
		m.flush(styMuted.Render("  · loading sub-agents…")),
		func() tea.Msg {
			list, err := c.listAgents(ctx, sessionID)
			if err != nil {
				return slashErrMsg{err}
			}
			return slashAgentsMsg{list: list}
		},
	)
}
```

This mirrors `handleSkills` at `cmd/spore/tui.go:700`, so it needs the same
two pieces that command has: a `slashAgentsMsg{list agentListJSON}` type
beside `slashSkillsMsg`, and a case for it in the model's `Update` switch that
flushes `formatAgents(msg.list)`. `slashErrMsg` is reused unchanged.

In `chat.go`'s `slashHandler`, add `case "agents": return ui.handleAgents(streamCtx, c, sessionID)`, and update the comment above it to list `/agents`.

In `runPlainSlash`, generalise the single-command check:

```go
func runPlainSlash(ctx context.Context, c *client, sessionID, text string, out io.Writer) (bool, error) {
	switch text {
	case "/skills":
		list, err := c.listSkills(ctx, sessionID)
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprint(out, formatSkills(list))
		return true, err
	case "/agents":
		list, err := c.listAgents(ctx, sessionID)
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprint(out, formatAgents(list))
		return true, err
	}
	return false, nil
}
```

Add `/agents` to the slash-command hint list the TUI shows.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -v 2>&1 | tail -15`
Expected: PASS.

- [ ] **Step 7: Verify the whole suite and the binary**

Run:
```bash
go build -tags sqlite_fts5 ./... && \
go vet -tags sqlite_fts5 ./... && \
go test -tags sqlite_fts5 ./... 2>&1 | tail -30
```
Expected: build clean, vet clean, every package PASS.

- [ ] **Step 8: Document the feature**

Add a short `## Sub-agents` section to `README.md`: the three tools and when the model should reach for each, the `[subagents]` limits, `/agents`, and the two rules a reader will otherwise be surprised by — a sub-agent inherits its launcher's trust profile, and `agent_spawn` needs the daemon.

- [ ] **Step 9: Commit**

```bash
git add cmd/spore README.md
git commit -m "feat(cli): add the /agents command

A client-side intercept plus the one daemon endpoint, the pattern the
other chat commands use. It is handled in the plain loop as well as the
TUI: a command that worked only under a terminal was one of the defects
found after the last batch of commands shipped."
```

---

## Final verification

- [ ] `go build -tags sqlite_fts5 ./...` — clean
- [ ] `go vet -tags sqlite_fts5 ./...` — clean
- [ ] `go test -tags sqlite_fts5 ./...` — every package passes
- [ ] `make test` — passes (it compiles neither container suite)
- [ ] Manual: start `spore serve`, open `spore chat`, ask for a sub-agent to be run, confirm `/agents` lists it and that `session list` does **not** show the child while `session list --all` does.
- [ ] Manual: with a child running, kill and restart the daemon; confirm `/agents` reports the child `interrupted` rather than `running`.

## Amend the design spec

- [ ] Remove `- Sub-agents / agent teams. Deferred until the single-agent loop is solid.` from the non-goals list in `docs/superpowers/specs/2026-08-29-spore-design.md` section 1, and note the amendment in that document's header the way the earlier amendments are noted.
- [ ] Add `internal/subagent` to the architecture listing in section 2.
- [ ] Add the stage to section 11, pointing at this plan and the sub-agent spec.
- [ ] Update `docs/backlog.md`: rewrite the **Sub-agents** entry as closed, recording what the four open questions were answered with — two tools rather than one flagged tool; a child is a session with `parent_id`; approvals inherit the profile and route to the root through an upward-only ancestor walk, with remembered decisions root-scoped; depth, tree cost and concurrency bound the tree; and `/agents` polls rather than streaming.
