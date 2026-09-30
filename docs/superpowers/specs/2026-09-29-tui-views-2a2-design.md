# spore — TUI views 2a-2: MCP, policy and memory

**Date:** 2026-09-29
**Status:** approved (brainstorming dialogue), pending written review
**Supersedes:** §4.2, §4.3 and the 2a-2 rows of §5–6 in
`2026-09-22-tui-views-design.md`. Everything else in that spec (the header,
the resource framework, the table's keys) is built and still holds.

## 1. What this adds

Three resource views in the TUI, and the daemon endpoints behind them:

- **mcp**: each configured MCP server's state and tools, and a reconnect.
- **policy**: every rule in force, per profile, in evaluation order, and a
  revoke for the rules spore learned from `p` answers.
- **memory**: the fact files, a recall search over them, and a delete.

It also fixes a present bug: **a rule learned with `p` does not take effect
until the daemon restarts.** `cmd/spore/wire.go` builds the `policy.Engine`
once; `p` only writes `config.toml`. The same would be true of a revoke, so
the fix is a precondition of the policy view, not a side quest.

### Scope

In: the three TUI views, five daemon routes, `Host.Redial`,
`config.UnlearnRule`, the live engine swap.

Out:
- The web UI's policy, MCP and memory views. They become a follow-up spec
  against these endpoints (the backlog entry for #49 is updated to say so).
- Live reload of hand-written policy. Editing `allow`/`ask`/`deny` in
  `config.toml` still needs a restart; only the spore-managed block reloads.
- Editing written rules, creating facts, or adding MCP servers from the TUI.

### What changed since the 2026-09-22 spec

- MCP now has a supervisor (`internal/mcp/supervise.go`) that redials a
  dropped server with backoff. A `Redial` that closes and dials on its own
  would race it; §3.3 goes through the supervisor instead.
- The `approvals` table's `session_id` has a foreign key to `sessions`, so a
  revoke made from a view (no session) cannot be recorded there as the old
  spec planned. §3.6 uses a structured log line for all three actions.
- `recall.Recall` has a `Delete`, and `Store.UnindexFact` writes a tombstone
  the mirror drains (#31). Delete-fact reuses that path unchanged.

## 2. The engine swap

### 2.1 Rule

The running engine is rebuilt from **the startup policy config with only
`Learned` replaced** by a fresh read of the managed block. Nothing else in
`config.toml` is re-read at runtime.

Why not reload the whole policy section: an unrelated hand edit made an hour
ago would take effect at whatever moment the next approval lands, and a
half-finished edit elsewhere in the file could break or loosen a running
daemon. Why not mutate the engine in memory: the file and the engine could
drift, e.g. a revoke whose file write was refused still taking effect. With
this rule the file stays the only source of truth, and the only thing that
changes live is text spore itself wrote.

### 2.2 Pieces

- **`policy.Guard`** holds its engine in an `atomic.Pointer[Engine]` and
  gains `SetEngine(*Engine)`. `Guard.Run` loads the pointer once per call, so
  a call evaluated before a swap keeps the decision it got; every call after
  uses the new rules.
- **`config.ReadLearned(path) (LearnedPolicy, error)`**: the managed-block
  parse `LearnRule` already does, factored out and exported. A file with no
  block returns an empty policy; more than one begin marker is the same error
  `LearnRule` gives.
- **`config.UnlearnRule(path, decision, rule) error`**: the mirror of
  `LearnRule` — same `learnMu`, same marker check, same parse-before-write,
  same temp-file rename and `0600`. It removes the exact string from the named
  list. A rule not in that list returns `ErrNotLearned` and writes nothing.
  An empty block is still written with its markers (three empty lists), so
  the next `LearnRule` finds it in place. Every byte outside the block is
  unchanged.
- **`policy.Reloader`** owns the startup `config.PolicyConfig`, the config
  path, and the guard:

  ```go
  func NewReloader(path string, base config.PolicyConfig, g *Guard) *Reloader
  func (r *Reloader) Learn(d Decision, rule string) error
  func (r *Reloader) Unlearn(d Decision, rule string) error
  func (r *Reloader) Rules() []ProfileRules // for GET /api/policy, §3.4
  ```

  `Learn` and `Unlearn` each run under one mutex: write the file
  (`LearnRule`/`UnlearnRule`), `ReadLearned`, copy `base` with `Learned`
  replaced, `NewEngine`, `SetEngine`, and keep the copy as the current config
  for `Rules`. The mutex makes two answers arriving together swap in the
  engines in the order their writes happened.

- **`wire.go`**: the `learn` closure passed to `NewGuard` becomes
  `reloader.Learn`. This fixes `p` for every surface — TUI, web UI and
  Discord approvals — since they all resolve through the guard.

### 2.3 Failure

If the file write fails, nothing changes and the error is returned. If the
write succeeds and the rebuild fails (a learned rule that does not parse), the
write stands, the old engine stays, and the error is returned; the next
successful rebuild or a restart applies it. The guard's existing
"learned rule not persisted" span record covers the `p` path.

## 3. Daemon

### 3.1 Wiring

`daemon.Options` gains, each optional:

| field | type | nil means |
|---|---|---|
| `MCP` | `*mcp.Host` | `/api/mcp*` answer 503 |
| `Policy` | `*policy.Reloader` | `/api/policy*` answer 503 |
| `Facts` | `*memory.Cache` | `/api/memory*` answer 503 |
| `FactIndex` | `mem.FactIndexer` | delete skips unindexing (tests only) |
| `Recall` | `recall.Recall` | `?q=` answers 503; the plain list still works |

`buildAgent` already builds the host, the fact cache and the recall backend;
it returns them (or a small struct of them) so `serve.go` can pass them on.

### 3.2 Routes

| route | behaviour |
|---|---|
| `GET /api/mcp` | §3.3 |
| `POST /api/mcp/{server}/reconnect` | `Host.Redial(name)`; unknown server 404; 202 on success |
| `GET /api/policy` | `Reloader.Rules()` as JSON, §3.4 |
| `DELETE /api/policy/learned` | body `{"decision","rule"}` → `Reloader.Unlearn`; `ErrNotLearned` 404; bad decision 400; rebuild failure 500 with the error |
| `GET /api/memory[?q=]` | §3.5 |
| `DELETE /api/memory/{name}` | §3.5; unknown fact 404 |

### 3.3 MCP

`GET /api/mcp` returns `Host.Status()` per server — name, transport, state,
last error, tool names, skipped tools with reasons — and for each tool the
decision and rule the current engine gives a call to it from the `local`
profile with empty arguments (`{}`), plus `depends_on_args: true` when the
deciding rule has an argument predicate. The view uses this to show
`(depends on args)`.

`Host.Redial(name) error` finds the server, and under its lock closes its
session (and, for stdio, lets the existing close path end the child). The
supervisor's `watch` sees the session end, returns, and the loop redials.
`Redial` also sets a per-server "redial requested" flag that makes the
supervisor treat this drop as intentional: it resets backoff to the minimum
and skips the wait, instead of the "disconnected; redialling" warning path.
A server that is currently down (the supervisor is sleeping in backoff) is
woken through a per-server `redial` channel the backoff `select` also waits
on. `Redial` never dials itself, so there is exactly one dialler per server.

### 3.4 Policy

`Reloader.Rules()` returns, for the base ruleset and each profile, the rules
in evaluation order: deny, then allow and ask as `buildRuleset` orders them
(hand-written before learned), then one row for the default decision. Each
row carries `decision`, `rule` (the `Raw` text) and `source`:

- `baseline` — in `config.BaselineDeny()` (an exported copy of the existing
  `baselineDeny`),
- `learned` — in the current `Learned` lists,
- `config` — everything else, including the default row.

A learned deny is global, so it appears under every profile; revoking it
removes one line from the file and every one of those rows goes on the next
refresh. Learned allow/ask appear only in the base (`local`) ruleset, as
`NewEngine` already scopes them.

This needs `Engine` (or `buildRuleset`) to expose its ordered rules; a
`func (e *Engine) Rulesets() map[Profile][]Rule` accessor plus the fallback
is enough. The reloader tags sources, since only it knows the lists.

### 3.5 Memory

`GET /api/memory` returns the cache's facts: name, type, description, body.
With `?q=`, it runs `recall.Search` with `Kinds: ["fact"]` and the default
`k`, and returns hits: name (the chunk's ref id), score, excerpt. No session
scope: this is the local operator's view of their own memory.

`DELETE /api/memory/{name}`: `memory.Delete(dir, name)`, `Facts.Reload()`,
then `FactIndex.UnindexFact`. An unindex error is logged and not returned:
the file is gone, which is what the prompt assembles from, and the tombstone
path retries the vector. This is the rule the refiner follows (and the
behaviour its test now proves). `memory.Delete` already refuses a name that
is not a valid fact name (through `Path`), so a path cannot escape the
directory. It reports a missing fact only as a formatted error today, so it
gains an `ErrNoFact` sentinel, wrapped into the existing message; the route
maps `ErrNoFact` to 404 and any `Path` error to 400.

### 3.6 Audit and who may call

Revoke, reconnect and delete-fact are HTTP routes on the local daemon: the
trust of `spore chat` and the web UI today. Each writes one structured log
line at info: `msg="operator action" actor=tui action=<revoke|reconnect|delete_fact> target=<rule|server|name>`
(plus `decision` for revoke). The `actor` comes from an `X-Spore-Client`
request header the TUI sends, defaulting to `http`, so a later web UI view
logs as itself.

The Discord bridge runs in-process and never makes HTTP calls to the
daemon, so it cannot reach these routes. It has no command table to assert
against — its one command, `/new`, is matched inline in `bridge.go` — so the
guarantee is structural: the bridge reaches the daemon only through
`discord.Options` (`Turns`, `Sessions`, `Guard`, …). A test walks
`discord.Options`' field types by reflection and fails if any is
`*policy.Reloader`, `*mcp.Host` or `*memory.Cache`, so handing the bridge
one of the collaborators these actions need is a deliberate act that breaks
a test. `Guard` stays safe to hand over because revoke lives on the
`Reloader`, not the guard.

## 4. TUI

### 4.1 Views

All three are `Resource` values added to `resources()` after the existing
five. `C`, `P` and `M` are free in both the hotkey table and `app.go`'s
single-key bindings.

| view | key | columns | enter | actions |
|---|---|---|---|---|
| **mcp** | `C` | SERVER · TRANSPORT · STATE · TOOLS (count) · LAST ERROR (flex) | drills into `mcpToolsRes{server}`: TOOL · DECISION · RULE (flex), rule suffixed `(depends on args)` when flagged; skipped tools listed after, decision `skipped`, rule = reason | `r` reconnect, no confirm |
| **policy** | `P` | PROFILE · DECISION · SOURCE · RULE (flex) | detail: the rule, its source, and for non-learned rows `edit config.toml to change this rule` | `x` revoke, `Applies` only to `learned`, confirm `revoke "<rule>"?` |
| **memory** | `M` | NAME · TYPE · DESCRIPTION (flex) | detail: the fact body | `x` delete, confirm `delete fact "<name>"?` |

Row IDs: mcp `server`; mcp tools `server/tool`; policy
`profile/decision/rule`; memory `name`. They are stable across refreshes, so
the cursor follows its row.

Reconnect has no confirmation because it is harmless and idempotent: it asks
for what the supervisor does on its own after any drop.

### 4.2 `:memory <query>`

Today `:<name>` drops arguments when it opens a view. `memory` is the one
view that takes them: `:memory tabs` opens `memoryRes{query: "tabs"}`, whose
rows are recall hits (NAME · SCORE · EXCERPT) and whose title is
`memory · "tabs"`. Plain `M` or `:memory` lists all facts. `/` filters
either list in place, as in every view. `x` works on hit rows too (the name
is the fact's). Other views keep ignoring arguments.

### 4.3 Backend

`Views` gains:

```go
MCP(ctx) ([]daemon.MCPServerJSON, error)
Reconnect(ctx, server string) error
Policy(ctx) (daemon.PolicyJSON, error)
Revoke(ctx, decision, rule string) error
Memory(ctx, query string) (daemon.MemoryJSON, error)
DeleteFact(ctx, name string) error
```

implemented over HTTP in `backend.go` like the existing methods, sending
`X-Spore-Client: tui`.

## 5. Errors

| failure | behaviour |
|---|---|
| a fetch fails | the table keeps its last rows; title shows `stale · <error>`; next tick retries (existing) |
| route 404s (older daemon) | `this daemon is older than the TUI — restart it` (existing) |
| route 503s (subsystem not wired, e.g. no MCP servers) | the view shows `no MCP servers configured` / `recall search is unavailable` in place of rows |
| an action fails | status-bar error; the row stays; the confirmation closes (existing) |
| reconnect fails | STATE and LAST ERROR show it on the next refresh |
| revoke of a rule not in the learned block | 404 → `only rules added with p can be revoked here` |
| rebuild fails after learn or unlearn | the file write stands, the old engine stays, the error is shown; the next rebuild or a restart applies it |
| fact deleted, unindex fails | success; logged; the tombstone path retries |

## 6. Testing

**`internal/config`**
- `UnlearnRule` removes only the named line from the named list; the file is
  byte-identical outside the block; a rule under a different decision, or
  only in hand-written `allow`, is `ErrNotLearned` and writes nothing; two
  begin markers is refused; the result always parses.
- `ReadLearned` on no block, one block, and two markers.

**`internal/policy`**
- **The engine swap, and the bug it fixes:** with a guard and reloader over a
  temp config, `Learn(allow, "fs_write")`, then the very next `fs_write` call
  is allowed with no approval (this test fails on today's code); `Unlearn`,
  then the next call asks again.
- A rebuild failure leaves the old engine in place and returns the error.
- `Rules()` orders rows as `buildRuleset` evaluates them, tags `baseline`,
  `config` and `learned` correctly, and shows a learned deny under every
  profile and a learned allow only under the base.
- Concurrent `Learn` calls both land in the file and in the final engine.

**`internal/mcp`**
- `Redial` on an up server: the supervisor redials without the backoff wait,
  and the other servers' sessions are untouched.
- `Redial` on a down server wakes the backoff sleep.
- `Redial` of an unknown name is an error; after `Close` it is a no-op.

**`internal/daemon`**
- Each route against fakes: shapes, 404s, 400s, 503 when the option is nil.
- `DELETE /api/memory/{name}` removes the file, reloads the cache, and
  writes a recall tombstone; with a failing indexer it still returns 200.
- `DELETE /api/policy/learned` then the next matching tool call asks again,
  through the real guard.
- Each action writes its audit line with the header's actor.
- `discord.Options` holds no `*policy.Reloader`, `*mcp.Host` or
  `*memory.Cache` (reflection over its field types).

**`internal/tui`**
- Each resource: columns and row mapping from fixture JSON, including a
  skipped MCP tool, a `depends on args` rule, and a learned deny repeated
  across profiles.
- `x` on a non-learned policy row does nothing; on a learned row it confirms.
- `:memory tabs` opens the hits view with the query in the title.
- Golden screens for the three views at 100 columns, and the 503 messages.

**End to end**
- The real model against an in-process daemon: answer an approval with `p`,
  the next matching call runs without asking; open `P`, revoke that rule, the
  next call asks again. No restart in between.

**Manual gate (before the PR)**
- With a real MCP server configured: `C`, `r` on it, watch STATE go down and
  back up within a couple of seconds.
- `p` on a real approval, then the same kind of call runs without a prompt;
  revoke it in `P`, and the next one prompts.
- `M`, `:memory <word>`, delete a throwaway fact, confirm it is gone from
  `~/.spore/memory` and from the next `:memory` search.
