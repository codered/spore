# spore — TUI views (k9s layer)

**Date:** 2026-09-22
**Status:** approved (brainstorming dialogue), pending written review
**Builds on:** `2026-09-22-tui-shell-design.md` (branch `tui-shell`, PR #41)
**Second of three TUI specs.** Session renaming and folders are spec 2b, written separately.

## 1. What this adds

The shell gave `spore chat` a sidebar and modal keys, but nothing on screen says
which keys exist, and there is nowhere to look at anything except a
conversation. A terminal developer who knows k9s expects two things this spec
adds:

- **A header** that always shows where you are and the keys that work there.
- **Resource views**: full-screen tables for skills, sub-agents, jobs, usage,
  MCP servers, policy and memory, opened by a hotkey or a `:` command, each
  with the actions that make sense on its rows.

Built as two plans under this one spec:

- **2a-1**: header, the resource framework, and the skills, agents, jobs and
  usage views, with stop-agent and cancel-job. Existing endpoints plus one
  aggregate.
- **2a-2**: the MCP, policy and memory views with reconnect, revoke and
  delete-fact. New daemon plumbing and the state-changing, security-relevant
  actions.

### Out of scope

- Renaming sessions and user-defined folders (spec 2b).
- Editing written policy rules from the TUI. `config.toml` stays the place a
  human writes policy.
- Creating jobs or facts from a view.
- Mouse interaction with tables.

## 2. Screen

```
┌ spore · a1b2 fix flaky · chat · ~/dev/spore     <n> new  <:> cmd  <?> help   │
│ sonnet-5 · ctx 38k · $0.12 · 1 blocked           <S> skills <A> agents <U> usage│
│                                                   <J> jobs <M> mcp <P> policy <R> memory
├─ skills(3) ───────────────────────────────────────────────────────────────────┤
│ … the chat screen (sidebar | transcript), or a resource table …               │
└ NORMAL │ <enter> detail  </> filter  <x> stop  <esc> back ─────────────────────┘
```

- **Header** (new, `header.go`): the left block is the selected session (short
  id, title, source, workspace) and its facts (model, context tokens, cost when
  `show_cost`, blocked count, `reconnecting…`). The right block is a grid of key
  hints: the global keys (`n`, `:`, `?`, `q`) and every view's hotkey. At 100
  columns and wider the header is three rows. Below 100 it is one row: the
  session on the left, `<?> help  <:> cmd` on the right.
- **Status bar** keeps only the mode badge and the hints for what is on screen:
  chat keys on the chat screen, the current view's keys in a view. The facts it
  showed in spec 1 move to the header.
- **Title rule**: the line between header and body names the body:
  `chat`, or `<view>(<row count>)`, plus `/<filter>` when one is active and
  `stale · <error>` when the last refresh failed.
- **Too small** stays 60×15; the header counts toward it.

## 3. The resource framework

### 3.1 Interface (`internal/tui/resource.go`)

```go
type Resource interface {
    Name() string       // "skills": the :command and the title
    Hotkey() string     // "S"
    Scoped() bool       // true: rows belong to the selected session
    Columns() []Column
    Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error)
    Detail(r Row) string // shown by enter / d
    Actions() []Action
}

type Column struct {
    Title string
    Min   int  // minimum width in cells
    Flex  bool // takes the remaining width; at most one per resource
    Right bool // right-align (numbers)
}

type Row struct {
    ID    string
    Cells []string
    Data  any // the typed value, for Detail and Actions
}

type Action struct {
    Key     string                  // "x"
    Label   string                  // "stop"
    Applies func(Row) bool          // nil: every row
    Confirm func(Row) string        // nil: no confirmation
    Run     func(ctx context.Context, v Views, r Row) error
}
```

### 3.2 The table (`internal/tui/table.go`)

One component runs every view:

- `j/k`, `g/G`, `ctrl+d/u` move; the cursor keeps its row ID across refreshes.
- `/` filters rows by substring over all cells, case-insensitive; `esc` in the
  filter clears it.
- `s` cycles the sort column, `S` reverses the order; the sorted column's title
  gets `↑`/`↓`.
- `enter` or `d` opens the detail pane (a scrollable viewport over
  `Detail(row)`); `esc` closes it.
- An action key runs the action on the selected row if `Applies`; if `Confirm`
  is set, the status bar asks `<prompt> y/n` and only `y` runs it. After an
  action the view refreshes immediately.
- Columns: each gets `Min`; the flex column takes the rest; cells are clipped
  in display cells (the `clip` fixed in spec 1).

### 3.3 Navigation

- A view opens by its hotkey (NORMAL mode on the chat screen) or by
  `:<name>` (with tab completion). `:chat` or `esc` returns to the chat screen.
- **`esc` is contextual, as in k9s.** In a view it means back (or closes the
  detail pane, or clears the filter, innermost first). On the chat screen it
  still stops the running turn. The status bar always shows what `esc` will do.
- A scoped view follows the selected session. Selecting another session (via
  `:chat`, then `j/k`) and reopening the view shows that session's rows.
- Hotkeys are uppercase so they never collide with the chat screen's
  lower-case keys: `S` skills, `A` agents, `U` usage, `J` jobs, `M` mcp,
  `P` policy, `R` memory ("recall").
- `alt+<hotkey>` works from INSERT like any other key (spec 1's Esc handling).

### 3.4 Refresh

While a view is open it refetches every 2 seconds, and immediately after an
action. `ctrl+r` refetches now. A fetch runs as a Bubble Tea command; a result
for a view that is no longer open is discarded. There is no polling while the
chat screen is showing.

### 3.5 Backend

`tui.Views` is a second interface beside `tui.Backend`, with one typed method
per view's data and per action. `cmd/spore`'s adapter implements both. Tests
use a fake.

## 4. Views

### 4.1 2a-1

| view | rows | actions | daemon |
|---|---|---|---|
| **skills** (scoped) | NAME · LOADED · TOKENS · DESCRIPTION; load errors as `!` rows with the message | detail: the skill body | `GET /api/sessions/{id}/skills` (exists) |
| **agents** (scoped) | ID · STATE · AGE · COST · PROMPT | `x` stops a running sub-agent, confirming `stop sub-agent <id>?`; `enter` selects that child session and returns to chat | `GET /api/sessions/{id}/agents`, `DELETE /api/sessions/{id}/agents/{child}` (exist) |
| **jobs** | ID · KIND · SCHEDULE · NEXT RUN · LAST RUN · PROMPT | `x` cancels, confirming `cancel job <id>?`; `enter` selects the job's last session | `GET /api/jobs`, `DELETE /api/jobs/{id}` (exist) |
| **usage** | a header block for the selected session (turns, tokens in/out, cache read/write and hit %, cost), then DAY · MODEL · TURNS · IN · OUT · CACHE % · COST over the last 30 days | none | **new** `GET /api/usage?session=<id>` |

**`GET /api/usage`** sums the `messages` table: one aggregate for the named
session, and rows grouped by UTC day and model over the last 30 days across all
sessions. Cache hit % is `cache_read / (tokens_in + cache_read + cache_write)`,
matching `/usage` since prompt caching shipped. Only assistant messages carry
usage, so the query sums them.

### 4.2 2a-2

| view | rows | actions | daemon |
|---|---|---|---|
| **mcp** | SERVER · TRANSPORT · STATE · TOOLS · LAST ERROR; `enter` opens TOOL · DECISION · RULE for that server | `r` reconnects the selected server | **new** `GET /api/mcp`, `POST /api/mcp/{server}/reconnect` |
| **policy** | PROFILE · DECISION · RULE · SOURCE, in evaluation order per profile, the default decision last | `x` on a **learned** rule revokes it, confirming `revoke "<rule>"?`; other rows are read-only and say `edit config.toml` | **new** `GET /api/policy`, `DELETE /api/policy/learned` |
| **memory** | NAME · TYPE · DESCRIPTION; `/` filters; `:memory <query>` runs a recall search and lists hits | `x` deletes a fact, confirming `delete fact "<name>"?`; detail: the fact body | **new** `GET /api/memory[?q=]`, `DELETE /api/memory/{name}` |

**MCP.** `GET /api/mcp` returns `mcp.Host.Status()` per server, and for each tool
the decision and rule the policy engine gives a call to it from the local
profile with empty arguments. A tool whose decision depends on its arguments
shows the decision for empty arguments, marked `depends on args`.
`POST …/reconnect` calls a new `Host.Redial(name)` that closes the named
server's client and dials it again, leaving the others alone. The daemon must
now keep a reference to the host (today only `serve.go` holds it).

**Policy.** `GET /api/policy` lists, per profile in evaluation order, each rule
with its source: `baseline` (the deny rules `config.Load` adds), `config`
(`allow`/`ask`/`deny` lines a human wrote), or `learned` (the spore-managed
block). `DELETE /api/policy/learned` with `{decision, rule}` calls a new
`config.UnlearnRule(path, decision, rule)`, which removes exactly that line from
the managed block under the same lock `LearnRule` uses. A rule not present in
the managed block is a 404. It never edits a line outside the block.

**The engine becomes swappable. This fixes a present bug.** Today `p`
("always allow this pattern") writes the rule to the config file, but the
running `policy.Engine` is built once at startup and never sees it: the rule
takes effect only after a daemon restart. The guard gains `SetEngine(*Engine)`,
holding the engine behind an `atomic.Pointer`. After a successful `LearnRule`
or `UnlearnRule`, the daemon reloads the policy section of the config, builds a
new engine and swaps it in. A call evaluated before the swap keeps the decision
it got; every call after it uses the new rules. If the rebuild fails, the old
engine stays and the endpoint reports the error.

**Memory.** `GET /api/memory` lists facts from the memory cache (`memory.Load`
over `MemoryDir`). With `?q=` it runs the recall query the model's recall tool
uses and returns hits (kind, name, snippet, score). `DELETE /api/memory/{name}`
removes the fact file, reloads the fact cache, and deletes the fact from the
recall index through the existing delete path (tombstone + mirror), so its
vector goes too.

### 4.3 Who may call the state-changing endpoints

Revoke, delete-fact, reconnect, stop-agent and cancel-job are HTTP endpoints on
the local daemon: the same trust as `spore chat` and the web UI today. The
Discord bridge never calls them and exposes no command that does. Each writes
an audit record (the `approvals` table's pattern for revoke; a log line with
the session-less actor `tui` for the rest).

## 5. Errors

| failure | behaviour |
|---|---|
| a fetch fails | the table keeps its last rows; the title shows `stale · <error>`; the next tick retries |
| the endpoint returns 404 for the route itself (an older daemon) | the view shows `this daemon is older than the TUI — restart it` in place of rows |
| an action fails | status-bar error; the row stays; the confirmation closes |
| reconnect fails | the server's STATE and LAST ERROR show it on the next refresh |
| revoke of a rule not in the learned block | 404 → `only rules added with p can be revoked here` |
| engine rebuild fails after learn or unlearn | the config write stands, the old engine stays, the endpoint returns the error; the next successful rebuild (or restart) applies it |
| fact file deleted, recall delete fails | success is reported; the tombstone feed retries the vector delete (the #31 path) |

## 6. Testing

**`internal/tui`**
- Table: movement keeps the cursor on the same row ID across a refresh that
  reorders rows; filter; sort; detail open/close; confirmation (`x` then `n`
  does nothing, `y` runs); stale-on-error; the older-daemon message.
- Each resource: column set and row mapping from a fixture response, including
  a skills load error and a usage block with zero cache.
- Navigation: hotkeys open views only in NORMAL; `alt+<hotkey>` from INSERT
  opens the view; `esc` closes the innermost layer; `esc` on the chat screen
  still stops a turn.
- Golden screens: the header at 80, 100 and 160 columns; every view at 100; a
  stale view; the detail pane.
- Real data shape: the agents and sidebar paths run against sessions migrated
  as `unknown`.

**`internal/daemon`, `internal/config`, `internal/policy`, `internal/mcp`**
- Usage totals against hand-computed fixtures, including cache hit % and the
  30-day window edge.
- `GET /api/mcp` against a fake host; `Redial` re-dials only the named server.
- `UnlearnRule` removes only the named learned line, refuses a written rule,
  and leaves the file byte-identical otherwise.
- **The engine swap:** after `p`, the very next call to a matching tool is
  allowed without restarting the daemon (this test fails on today's code);
  after revoke, the next call asks again.
- Deleting a fact removes the file and writes a recall tombstone.
- Each state-changing endpoint is unreachable from the Discord bridge's command
  set (asserted against the bridge's command table).

**End to end**
- The real model against an in-process daemon: open `S`, filter, `esc` back to
  chat, open `J`, cancel a job, open `P`, revoke a learned rule, then a call
  that matched it asks again.

**Manual gate (in each plan, before its PR)**
- Under tmux: `Esc` followed quickly by a hotkey opens the view.
- A view keeps refreshing while a Discord turn runs.
- Revoke a real learned rule and confirm the next call asks again.
