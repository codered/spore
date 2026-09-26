# spore — continual refinement

**Date:** 2026-09-26
**Status:** approved (brainstorming dialogue), awaiting written-spec review.

## 1. What this adds

Spore already has the state a learning agent needs: memory facts
(`<data_dir>/memory/*.md`, written by the `memory` tool) and per-workspace
project notes (`agent.md`, written by `agent_note`). What it lacks is anything
that decides what belongs there. A correction the user gives today is lost
unless they say "remember that", and nothing prunes or merges facts that have
gone stale.

This spec adds **refinement**, modelled on prime-agent's `/refine`: a separate
LLM pass reviews a session's recent conversation and emits small, evidence-backed
create/update/delete edits to facts and project notes. Every edit is recorded
in a ledger with before/after content so it can be rolled back. Refinement
runs unattended — on compaction, on idle, or when the model asks — as well as
on a manual `/refine`.

### Decisions made during brainstorming

| Question | Decision |
|---|---|
| Purpose | Both: capture lessons that would otherwise be lost, and keep facts/notes tidy — unattended, including Discord sessions |
| Approval gate | Trust by origin: `chat` sessions apply immediately; `discord`, `job`, `unknown` sessions write only quarantined proposals the user accepts from the TUI. Everything is ledgered and reversible |
| Editable state | Memory facts and workspace `agent.md` only. Skills and `soul.md` are out of reach |
| Triggers | Compaction, session idle, model request (`refine` tool), manual `/refine` |
| Shape | Structured LLM pass returning JSON edits; spore's own code validates and applies them (not a sub-agent with write tools) |
| Refiner model | A new router call site, `refinement`; a cheaper model is an ordinary `[[routes]]` entry, no new setting |
| Cache | No special delivery. An applied edit changes the system prefix and costs one cache miss on the next turn, as a manual `memory` write does today |

### Non-goals

- Editing skills, `soul.md`, config, or policy.
- Posting proposals or refinement notices to Discord.
- A CLI surface for review. The TUI and the HTTP API are the surfaces.
- Refining sub-agent sessions. The parent's review covers their work.

## 2. Security model

Refinement writes persistent context that shapes every later turn. Today the
three tools that do this (`memory`, `skill_install`, `agent_note`) are
approved one write at a time (`nonLearnable`, `internal/policy/guard.go`), and
the `remote` profile denies them outright so a Discord injection cannot plant
permanent context. An unattended refiner must not become a way around that.
Three rules hold it:

1. **Trust follows the content's origin, not the trigger.** A round's edits
   apply only when the session's `Source` is `chat`. For `discord`, `job` and
   `unknown` they are stored as `proposed` and change nothing until accepted.
   A `/refine` the user runs from the TUI on a Discord-origin session still
   quarantines, because the injected text is in the session whoever triggers
   the review.
2. **The refiner never sees tool-result content.** Its transcript carries
   user and assistant text and tool-call names; tool results appear only as
   `[tool result: N bytes]`, as compaction already renders them. A web page or
   file the session read cannot speak to the refiner. A lesson has to appear
   in what the user or spore actually said.
3. **The edit vocabulary is closed.** Only the kinds in §3.2 exist. A planner
   reply naming any other kind or target is dropped, not interpreted.

`subagent` sessions are never refined: the sweeper skips them and `Round`
refuses them.

## 3. Components

### 3.1 Package `internal/refine`

One job per file:

- **`transcript.go`** — builds the refiner's input from the session's
  messages with `seq > refined_through` (or up to a given boundary for the
  compaction trigger). Note rows are skipped. Tool results are replaced as in §2.
- **`plan.go`** — the LLM call on `router.SiteRefinement`. System prompt:
  the refiner's instructions and the edit schema. User content: the transcript,
  the current facts (name, type, description, body), the workspace `agent.md`
  text if any, and optional user/model `instructions`. Output: a JSON object
  `{"edits": [...]}`. Output budget derives from the model, as compaction's does.
- **`apply.go`** — validates each edit, decides apply vs. propose (§2 rule 1),
  writes the ledger row, then the file (§5.3), and returns a round result.
- **`refine.go`** — `Round(ctx, sessionID, trigger, instructions) (Result, error)`,
  the single entry point every trigger calls, plus the per-session in-flight
  guard.

### 3.2 Edits

| Kind | Target | Payload |
|---|---|---|
| `fact.create` | fact name | type, description, body |
| `fact.update` | fact name | any of type, description, body |
| `fact.delete` | fact name | — |
| `notes.append` | workspace `agent.md` | text |
| `notes.replace` | workspace `agent.md` | full new text |

Every edit also carries a one-line `rationale` that cites what in the
transcript justifies it.

Validation, per edit; a failing edit is dropped and logged, the rest proceed:

- Fact edits pass through `memory.Fact.Validate` (kebab-case name, closed type
  set, single-line description, non-empty body). `update`/`delete` require the
  fact to exist; `create` requires it not to.
- `notes.*` requires the session to have a workspace.
- At most one edit per target per round, which keeps a round's edits
  independent for rollback (§4.2).
- At most `refine.max_edits` (default 5) edits per round; extras are dropped.
- A body-size cap per edit (fact body 4 KiB, notes append 2 KiB, notes replace
  16 KiB, matching `persona`'s `warnBytes`). Oversize edits are dropped, never
  truncated.

### 3.3 Store

Additive migration in `internal/store/schema.go`:

```sql
CREATE TABLE IF NOT EXISTS refinements (
  id          INTEGER PRIMARY KEY,
  round_id    TEXT    NOT NULL,
  session_id  TEXT    NOT NULL,
  trigger     TEXT    NOT NULL,  -- manual | compaction | idle | model
  kind        TEXT    NOT NULL,  -- §3.2
  target      TEXT    NOT NULL,  -- fact name, or agent.md path
  before      TEXT,              -- NULL when the target did not exist
  after       TEXT,              -- NULL when the edit deletes it
  rationale   TEXT    NOT NULL,
  status      TEXT    NOT NULL,  -- applied | proposed | rejected | rolled_back | stale
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
```

`before`/`after` hold the whole file content (frontmatter included for facts),
so rollback and staleness compare bytes, not parsed fields.

Two new `sessions` columns:

- `refined_through INTEGER NOT NULL DEFAULT 0` — seq watermark; a round
  reviews only rows above it and advances it on success.
- `refine_attempted_at INTEGER NOT NULL DEFAULT 0` — last round start, used by
  the idle sweeper's backoff (§5.1).

### 3.4 Router and config

- `router.SiteRefinement = "refinement"`.
- `[refine]` config section: `enabled` (default true), `idle_minutes`
  (default 10), `max_edits` (default 5). Invalid values fail `config.Load`
  validation, not silently default.

## 4. Flow

### 4.1 Triggers

All four call `Round`. The in-flight guard allows one round per session; a
compaction, idle or model trigger arriving while one runs is dropped (the
running round, or the next trigger, covers the same messages); a manual
`/refine` gets `409 a refine is already running` so its instructions are not
silently lost.

- **Compaction.** When `Compact` folds, it starts a round in a background
  goroutine over messages up to the fold boundary. Compaction never deletes
  rows, so the round reads full originals and the turn does not wait.
- **Idle.** A sweeper on the daemon's existing tick selects sessions where:
  `updated_at` is older than `idle_minutes`; a user message exists with
  `seq > refined_through`; `refine_attempted_at < updated_at`; no turn is
  running (hub); `Source != subagent`.
- **Model.** A new `refine` tool, args `{instructions?: string}`. It sets a
  per-turn "refine after this turn" flag and returns `{"scheduled": true}`
  immediately; a second call in the same turn replaces the instructions. The
  agent runs the round after the turn ends. In code mode it is reachable as
  `spore.Call("refine", …)` through the generated catalogue. It is on the
  default Allow list: scheduling is harmless, and §2 governs what applies.
- **Manual.** `/refine [instructions]` in the TUI calls
  `POST /api/sessions/{id}/refine`, which runs a round synchronously and
  returns its result.

### 4.2 Review and rollback (TUI)

- `/refine review` opens the list of `proposed` rows (the confirm-modal
  pattern from #44): source session, trigger, rationale, before/after diff.
  `a` accepts, `r` rejects, `A` accepts all. Accept runs the staleness check
  (§5.3) first. The tab bar shows a pending-proposal count when non-zero.
- `/refine rollback` rolls back the session's most recent applied round;
  `/refine rollback <round>` a specific one. Each edit is restored to
  `before` (create → delete the file; delete → restore; notes → old text). A
  stale edit is skipped and reported; the others still roll back, which is
  safe because a round holds at most one edit per target.

API: `GET /api/refinements?status=proposed`, `POST /api/refinements/{id}/accept`,
`POST /api/refinements/{id}/reject`, `POST /api/sessions/{id}/refine/rollback`
with optional `round_id`.

### 4.3 After a round

- `refined_through` advances to the last seq the round reviewed.
- A note row is appended to the session, e.g.
  `refined: 2 applied (fact create go-test-flags, notes append), 1 proposed`.
  Note rows never reach the model; the model sees the effect through the next
  turn's system prefix.
- Usage is recorded against the session under the `refinement` site, so
  `/usage` shows it.
- A trace span wraps the round, like compaction's.

## 5. Failure handling

### 5.1 Round failures

Provider error, unparseable JSON, or a truncated reply: the round fails, the
error is recorded on the span and logged, and `refined_through` does not move.
`refine_attempted_at` was set when the round started, so the idle sweeper does
not retry until the session gets a new message. A manual `/refine` reports the
error to the user.

### 5.2 Edit failures

An invalid edit (§3.2) is dropped and logged; the others proceed. A round with
zero valid edits still succeeds and advances the watermark — reviewing and
finding nothing is a result.

### 5.3 Write ordering and staleness

- **Staleness at apply.** Before writing, each target is re-read and compared
  with what the planner was shown. A mismatch (a `memory` tool write or hand
  edit during planning) records the edit as `stale` and writes nothing.
- **Staleness at accept/rollback.** Accept requires the target to still equal
  `before`; rollback requires it to equal `after`. Otherwise the row becomes
  `stale` and nothing is overwritten.
- **Ordering.** The ledger row is inserted first, the file written second. A
  crash between them leaves a row claiming an edit that never landed; a later
  rollback finds the file ≠ `after` and marks it stale. No write ever goes
  unrecorded.

## 6. Testing

A fake provider returns scripted JSON, as the compaction tests do.

**Security properties** — each a named test:

- A round on a `discord`, `job` or `unknown` session writes no files and
  records only `proposed` rows, including when triggered by manual `/refine`.
- An injection string placed in a tool result is absent from the request the
  fake provider receives.
- `subagent` sessions are never selected by the sweeper and `Round` refuses them.
- A planner reply with an unknown kind, or naming a skill or `soul.md`, is
  dropped.

**Ledger and files:**

- Create/update/delete facts, roll back, files byte-identical to the start.
- Hand edit between propose and accept → `stale`, file untouched.
- Row inserted, file never written, then rollback → `stale`, no overwrite.
- Over `max_edits`, duplicate target, bad name, notes without workspace,
  oversize body → only the valid edits apply.

**Triggers:**

- Sweeper eligibility with an injected clock: idle vs. active, new user
  message vs. none, turn running, backoff after failure.
- `refine` tool sets the flag; the round runs after the turn; a second call
  replaces instructions.
- Compaction starts a round over the folded range without blocking.
- The watermark advances only on success.

**End to end (daemon):** a turn containing a correction, the session goes
idle, the fact exists and a note row was written; `/refine rollback` removes it.

Temp directories in tests use the symlink-resolved path (macOS). Verification
runs in a detached worktree at HEAD.
