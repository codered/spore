# spore — sub-agents

**Date:** 2026-09-09
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-08-29-spore-design.md` section 1 (non-goals), section 2
(architecture), section 11 (stages)

## 1. What this adds

Sub-agents: a session may launch another agent, blocking on its answer or
detaching it to work in the background, and a human may see what is running.

The design spec lists "Sub-agents / agent teams" under **non-goals (v1)**,
deferred until the single-agent loop is solid. That condition is met — plans
1 to 7 have shipped — so this change **removes that line from section 1**
rather than merely extending the system around it. Section 2's package list
gains `internal/subagent`, and section 11 gains a stage.

Four uses were asked for, and the design serves all four:

1. **Context isolation** — delegate a token-heavy task so the parent's
   context sees only the conclusion.
2. **Parallel fan-out** — several independent probes at once.
3. **Cheaper model delegation** — mechanical work on a smaller model.
4. **Long-running background work** — a task that outlives the parent's turn.

The first three want the parent to block on a returned value; the fourth
cannot, because a tool call returning to a turn that already ended has
nowhere to return to. Two tools resolve this rather than one tool with a
flag: `agent_run` blocks, `agent_spawn` detaches.

## 2. Architecture

A new package `internal/subagent` owns one `Supervisor`. It is the single
place that knows what is running, and it has three consumers: the tools, the
daemon endpoint, and the startup sweep.

```go
type Supervisor struct {
    mu      sync.Mutex
    running map[string]*child // child session id -> live state
    agent   *agent.Agent      // attached after buildAgent constructs it
    store   *store.Store
    cfg     config.SubagentConfig
}

func (s *Supervisor) Attach(a *agent.Agent)
func (s *Supervisor) Run(ctx context.Context, parentID, prompt string) (string, error)
func (s *Supervisor) Spawn(ctx context.Context, parentID, prompt string) (string, error)
func (s *Supervisor) Result(ctx context.Context, childID string) (Status, error)
func (s *Supervisor) List(ctx context.Context, parentID string) ([]Status, error)
func (s *Supervisor) Cancel(ctx context.Context, childID string) error

// Status is one child, live or finished. It is what agent_result returns to
// the model and what the endpoint renders; a live row is read from the
// supervisor's map, a finished one from subagent_runs.
type Status struct {
    ID      string
    Prompt  string
    State   string // running | done | failed | interrupted
    Result  string // the child's final assistant text; empty until done
    Error   string
    Depth   int
    CostUSD float64
    Started time.Time
    Ended   time.Time // zero while running
}
```

`internal/tool/subagent` wraps the supervisor as three tools: `agent_run`,
`agent_spawn` and `agent_result`. All three report `ReadOnly() == false`. A
child mutates, so it must never join the loop's parallel read-only batch; the
fan-out concurrency comes from the model issuing several `agent_spawn` calls,
not from the dispatcher.

`internal/agent` does not import `internal/tool`, so `internal/subagent` may
hold an `*agent.Agent` without a cycle. The wiring order in `buildAgent` is
the one constraint: the registry is built before the Agent, so the supervisor
is constructed empty, the tools are registered against it, the guard and the
Agent are built as they are today, and `sup.Attach(a)` closes the loop last.
Nothing here depends on the daemon, so `spore once` and scheduled jobs get
`agent_run` for free.

### A child turn is a routed call site

`router.ValidSite` is a closed set of four sites. This adds a fifth,
`SiteSubagent = "subagent"`, and child turns name it. Cheaper model
delegation is then pure configuration —

    [[route]]
    when  = "subagent"
    model = "haiku"

— with no model selection code anywhere in the sub-agent path.

## 3. Data model

`sessions` gains one column, through the existing `migrateSessions`:

    parent_id TEXT NOT NULL DEFAULT ''

with `idx_sessions_parent` on it. `ListSessions` filters to `parent_id = ''`
by default, and takes an `--all` flag to include children: the rule is
top-level only unless asked, so `session list` does not fill with fan-out
transcripts.

Run bookkeeping lives in its own table rather than on `sessions`, because it
is sub-agent-only state where `parent_id` is generally useful:

```sql
CREATE TABLE IF NOT EXISTS subagent_runs (
  session_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  parent_id  TEXT NOT NULL,
  prompt     TEXT NOT NULL,
  state      TEXT NOT NULL,  -- running | done | failed | interrupted
  result     TEXT NOT NULL DEFAULT '',
  error      TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at   TEXT NOT NULL DEFAULT ''
);
```

Parentage is deliberately in both places. The approval ancestor walk and the
`ListSessions` filter both need it without joining a subsystem table.

Child cost needs no new storage. `messages.cost_usd` already carries it, and
the tree ceiling sums it over the root and its descendants.

## 4. Approvals and trust

A child's `policy.Session` inherits the parent's `Profile` and `Workspace`
verbatim, and gets its own `ID`.

Inheriting the profile is what stops a sub-agent gaining reach its launcher
lacks: a Discord-launched child stays on `remote`, where `mcp__*`, `memory`
and `skill_install` are denied outright. Inheriting the workspace keeps the
child's `fs_*` calls inside the same ceiling, and is also the right default —
a child helping with the parent's task needs the parent's files. The distinct
ID keeps every call the child makes individually attributable in the audit
log.

`deny` is absolute for children exactly as for parents, and never escalates
to a human.

### Routing an ask to the human

A pending approval raised by a child must reach the clients a human actually
has attached, which are attached to the root of the parent chain, not to the
child. `Pending` gains a tree-aware variant that returns a session's own
pending calls together with those of its descendants, each tagged with the
originating child id and prompt so the human can see what they are answering
for.

`Resolve` needs the matching change, and the ownership check it relies on
lives inside `store.ClaimPendingCall`, which matches `session_id` atomically
in one statement. That SQL is **not** loosened. Authorization happens in
front of it: verify the answering session is an ancestor of the pending row's
session, then claim with the row's own session id, unchanged. The claim stays
exactly as atomic against double answers as it is today, and there is no
time-of-check gap, because parentage is immutable once written.

The walk is upward only. A child can never answer its own approval, and a
sibling can never answer another's.

### Remembered decisions are root-scoped

`SessionDecision` remembers "always allow this session". A child looks that
up under the **root** of its chain, so one answer covers the tree.

This is a deliberate widening, and the reason is that the alternative does
not work. Per-child scoping makes a five-way fan-out ask five times for the
same tool, and detached children have no one attached to answer at all, so
their asks would run out the approval timeout and deny — turning background
work into silent failure. Root scoping means an approval given in a parent
context authorises that tool across transcripts the human has not read; the
containment for that is the profile the whole tree shares, and the baseline
deny set no approval can override.

## 5. Lifecycle

A detached child must not inherit the parent turn's context, which is
cancelled when the turn ends. `internal/agent` already has `persistCtx` for
this case; `Spawn` detaches through it and keeps the `context.CancelFunc` in
the supervisor's map.

### `agent_spawn` is daemon-only

A daemon that starts up should mark `running` rows `interrupted`, because the
goroutines that were running them died with the previous process. A blind
sweep would also clobber runs belonging to a concurrently running `spore
once`.

The cut that removes the ambiguity: `agent_spawn` requires a daemon-hosted
session. A one-shot CLI process has nothing to collect a detached result
anyway, so `agent_spawn` from a non-daemon session returns a tool error
telling the model to use `agent_run` instead. Blocking `agent_run` works on
every surface. The startup sweep is then unambiguously the daemon's own.

### Cancellation

Cancelling a child is a human action, not a model one, so it is not a fourth
tool. It is `DELETE /api/sessions/{id}/agents/{childID}` plus an affordance
in `/agents`. A cancelled child's turn ends, its row goes `interrupted`, and
its partial transcript stays readable through `session show`.

## 6. Containment

Configuration gains a `[subagents]` block:

| Key              | Default | Bounds                                  |
|------------------|---------|-----------------------------------------|
| `max_depth`      | 2       | A top-level session spawns children; children may not spawn. |
| `max_cost_usd`   | 1.00    | Summed over the root and all descendants. |
| `max_concurrent` | 4       | Children running at once under one root. |

`maxIterations` (12) already bounds one agent's round trips; none of it
bounds a tree. Depth is computed by walking `parent_id`. Cost is
`SUM(cost_usd)` over the tree, checked before each child turn.
`max_concurrent` is included even though depth and cost were the two asked
for, because an unbounded `agent_spawn` batch reaches provider rate limits
well before it reaches the cost ceiling.

Exceeding any of the three refuses **new** spawns, with a tool error the
model can read and route around. Children already running are left to finish:
each is independently bounded by `maxIterations`, and killing one mid-turn
would discard work already paid for.

## 7. Visibility

    GET /api/sessions/{id}/agents

    200 {"agents": [
           {"id": "…", "prompt": "audit the policy tests",
            "state": "running", "elapsed_s": 41,
            "cost_usd": 0.021, "depth": 1}
         ]}

Live rows are read from the supervisor, finished ones from `subagent_runs`.

`/agents` is a client-side intercept in the Bubble Tea model, following the
pattern `/skills` and `/usage` shipped with: intercept in the client, one
daemon endpoint where new server behaviour is needed. It therefore works
identically on all three surfaces, including Discord, which has neither a
live view nor SSE. It polls rather than streams — the human asks, and gets
the answer as of now.

Streaming child events into the parent's SSE was considered and rejected: it
would reintroduce to the parent's display exactly the noise that context
isolation removes from its prompt.

The intercept must handle the plain and non-TTY path as well as the TUI. A
`/skills` intercept that worked only in the TUI was one of the three defects
found after plan 7 shipped.

## 8. Testing

- Supervisor: depth refusal, tree-cost refusal, concurrency capping, and that
  a refusal is a tool error rather than a failed turn.
- Policy: a child cannot answer its own approval; a sibling cannot answer
  another's; a parent can answer a child's; a `remote` parent's child is
  `remote`; a child inherits the parent's workspace.
- Store: the `parent_id` migration against a database created before it, and
  `ListSessions` hiding children unless asked.
- Lifecycle: a detached child survives its parent's turn ending; a `running`
  row left by a previous process becomes `interrupted` at startup;
  `agent_spawn` outside the daemon is refused.
- Router: a child turn names the `subagent` site and routes by it.

Policy tests build their `config.PolicyConfig` explicitly rather than on
`config.Default()`. `config.Load` adds the baseline deny set, which silently
satisfies security assertions that were meant to be testing the rules under
test.

## 9. Staging

One stage, in dependency order: schema and `ListSessions`; the supervisor and
`agent_run`; the policy ancestor walk and root-scoped decisions; `agent_spawn`,
the runs table and the startup sweep; the endpoint, `/agents` and cancel.

`agent_run` alone is a shippable increment — it serves three of the four
stated uses and needs neither the runs table nor the sweep — so the split
point, if one is wanted, is after the policy work.
