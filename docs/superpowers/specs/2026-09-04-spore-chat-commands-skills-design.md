# spore — chat commands and skills

**Date:** 2026-09-04
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-08-29-spore-design.md` sections 3, 5, 6, 8, 9 and 11

## 1. What this adds

The interactive client has no commands. Everything a session knows about
itself — what is in its prompt, what it has cost, whether it is about to
compact — is either invisible or reachable only by reading the database.
This design adds a command surface to the daemon API and the five commands
asked for: `/compact`, `/context`, `/usage`, `/clear` and `/skills`.

`/skills` is not a listing over something that exists. It requires a skills
subsystem: markdown files under the data directory, an index of their names
and descriptions in every prompt, and a tool the model calls to pull one in.
That subsystem is the larger half of this design, and it is specified here in
full rather than deferred, because the command that lists skills is worth
little without them.

Two properties hold throughout. Commands are daemon behaviour, not client
behaviour, so every surface gets them at once and each is tested once. And
the skills directory is read-only by construction: it sits outside
`policy.Workspace`, where the baseline deny rule `fs_*(path outside
workspace)` — which no approval may talk past — already forbids every
filesystem tool. The one write path is a tool of its own, ask-gated and
non-learnable.

## 2. The command surface

### Endpoint

    POST /api/sessions/{id}/commands
    {"name": "compact", "args": ""}

    200 {"name": "compact", "text": "folded 14 messages; 38k -> 9k tokens"}

`internal/daemon/commands.go` holds a dispatch table from name to
`func(ctx context.Context, sess store.Session, args string) (CommandResult, error)`.
Adding a command is one table entry and one function, and
`internal/daemon/commands_test.go` is the single place command behaviour is
tested.

The result envelope is deliberately thin:

```go
type CommandResult struct {
    Name string `json:"name"`
    Text string `json:"text"`
}
```

`Text` is rendered server-side. The CLI prints it into the transcript pane;
the web UI puts it in a `<pre>`. Rendering therefore lives on the one path
both clients already share, rather than being reimplemented per surface and
drifting. No structured payload is defined: every command's output today is a
block of text, and a field nothing consumes is a field nothing tests.

### Commands are not messages

A command appends nothing to `messages` and starts no turn, so its output
never enters the model's context. This is what makes `/context` honest — it
reports the prompt as it stands, without having changed it by being asked.

The two commands that mutate session state (`/clear` and `/compact`) publish
a hub event after the change, so every attached client learns the summary
boundary moved rather than only the caller. A CLI session and the web UI open
on the same session stay in agreement.

### Concurrency

`/clear` and `/compact` rewrite the summary boundary that a running turn is
reading. Both return 409 when the session has a turn in flight, reusing the
`ErrTurnRunning` path `handlePostMessage` already uses. `/context`, `/usage`
and `/skills` are reads and are answered at any time.

### Clients

`client.command(ctx, sessionID, name, args)` joins `cmd/spore/client.go`.
Both loops — the Bubble Tea model and `chatPlain` — parse only a leading `/`
on a non-empty line, split the first word as the name and pass the remainder
as `args`. Neither client knows any individual command, so a client can never
hold a stale command list: an unknown name returns 400 carrying the valid
names, which the client prints.

A bare `/` and a line that merely contains a slash are ordinary messages.

Discord slash commands beyond `/new` remain out of scope, as section 8 of the
design spec already states. The endpoint exists when that changes.

## 3. The four mechanical commands

### `/compact`

`MaybeCompact` currently fuses the threshold test with the folding. Split it:
`Compact(ctx, sessionID)` folds unconditionally, and `MaybeCompact` becomes
the threshold test plus a call to it. Behaviour on the turn path is
unchanged.

The command calls `Compact`. The protected-window guard stays inside
`Compact`, so a session with `len(live) <= KeepRecent` is told there is
nothing outside the protected window to fold rather than receiving a silent
no-op that reads as a failure. The result reports messages folded and the
estimated size before and after.

### `/context`

`SnapshotTokens` returns a single total today. A command that reports the
parts must not compute them a second way, or the two estimates drift and the
one that drives compaction is not the one the user is shown. Add:

```go
type Breakdown struct {
    System      int
    Environment int
    Facts       int
    Skills      int
    Summary     int
    Messages    int
}

func SnapshotBreakdown(snap Snapshot, cfg config.ContextConfig) Breakdown
```

and redefine `SnapshotTokens` as the sum of its fields. One definition; the
compaction trigger keeps using the same number it always has.

One subtlety the handler must respect: `Snapshot` fills `Environment` from
`a.Env(policy.WorkspaceFrom(ctx))`. The command handler builds its context
with the session's own workspace exactly as a turn does, or `/context`
reports an environment section the real prompt would not have.

The result lists each part with its estimate, the total, and the total
against `context.max_tokens` and the `compact_at` threshold — so it also
answers "am I about to compact".

### `/usage`

Two store methods over `messages`:

```go
func (s *Store) SessionUsage(ctx context.Context, sessionID string) ([]UsageRow, error)
func (s *Store) TotalUsage(ctx context.Context) ([]UsageRow, error)
```

Each sums `tokens_in`, `tokens_out` and `cost_usd`, grouped by model. The
result is the session's totals, the per-model rows, and the all-time totals.

Cost is always shown, regardless of `show_cost`. That flag governs the
per-turn footer, where cost is noise most of the time; `/usage` is an
explicit request for exactly this number. A provider configured without
`price_in`/`price_out` reports zero cost with its token counts intact.

### `/clear`

`SetSummary(ctx, id, "", lastSeq)`. The boundary moves to the newest message
and the summary text is emptied, so the next prompt is system, environment,
facts and skills, and nothing else.

Nothing is deleted. Every message stays in `messages`, recall still finds
them, `session show` and `session export` are unaffected, and the session id
does not change — so a Discord thread bound to this session through
`bridge_bindings` keeps working, which opening a new session would silently
break.

One fix travels with it: `SetSummary` writes the summary text into
`recall_fts` unconditionally, so clearing would insert an empty document.
Guard the index write on non-empty text.

The result names how many messages are now outside the prompt.

## 4. Skills

### Layout

    <skills dir>/<name>/SKILL.md

One directory per skill. The frontmatter carries `name` and `description`,
both required, and `name` must equal the directory name so the two cannot
disagree. The body is markdown.

```markdown
---
name: release-checklist
description: The steps for cutting a spore release
---

Tag from master only. Run make test and make vet first...
```

`internal/skill` owns this layer and mirrors `internal/memory`: filesystem
only, no database handle, no knowledge of sessions.

```go
type Skill struct {
    Name, Description, Body, Path string
}

func Load(dir string) ([]Skill, []error)   // sorted by name; per-file errors
func Read(dir, name string) (Skill, error)
func ValidName(name string) error
```

A `skill.Cache` mirrors `memory.Cache`, so building the per-turn index does
not hit the disk on every turn and a directory-level read failure preserves
the cached list rather than emptying it.

`ValidName` repeats memory's lowercase-kebab rule rather than importing it.
Three duplicated lines are the better trade against a dependency between two
otherwise independent filesystem packages, and the rule is security-relevant
here: `skill_load(name)` turns a model-supplied string into a path.

### Read-only by construction

The skills directory sits outside `policy.Workspace`. The baseline deny rule
`fs_*(path outside workspace)` is always in force and no approval removes it,
so `fs_write` and `fs_edit` cannot reach a skill file. There is exactly one
write path into the skills directory, and it is `skill_install`.

### `skill_load`

One read-only tool in `internal/tool/skill/`. It takes a name and returns the
SKILL.md body as tool output; an oversized body truncates through the
registry's existing `MaxOutput`, which needs no new mechanism. A missing or
malformed skill returns a tool *error result*, not a turn failure: the model
sees it and may choose another skill.

It joins the default allow list next to `recall_search`. It stays allowed
under the `remote` trust profile, which adds only `mcp__*` and `memory` to
its deny list. That is deliberate: a skill body is the operator's own prose
in the operator's own data directory, and loading it is a read with no
persistent effect.

### `skill_install`

The one write path, and the "temporary permission" the operator asked for.
It takes a name, a description and a body, and writes
`<skills dir>/<name>/SKILL.md` atomically — temp file then rename, as
`memory.Write` does — confined by `internal/skill` rather than by policy path
rules.

It goes in the default **ask** list, and the guard is told `patternOK =
false` for it. `ScopePattern` therefore downgrades to `ScopeOnce`: "always
allow this pattern" is never learned into a standing rule, so every install
is approved on its own. The permission is granted for one write and is gone
afterwards, using the mechanism the guard already has rather than a new one.

Installing over an existing name is an update, and the approval prompt says
so rather than overwriting silently.

Under the `remote` profile `skill_install` is **denied**, for the same shape
of reason `memory` already is: a skill written once shapes every later turn
in every session, so a single injection through a bridge would otherwise
plant permanent context.

### Skills in the prompt

`Snapshot` gains `Skills []skill.Skill`. `Assemble` emits a skills section of
`name — description` lines only — never bodies — placed after facts. The
fixed order of section 3 of the design spec becomes:

1. System prompt
2. Memory facts
3. **Skills index**
4. Compaction summary
5. The live message tail

`ContextConfig` gains `skill_budget`, defaulting to 500, with the same
overflow behaviour facts have: the list is truncated and the count of omitted
skills is stated. `SnapshotBreakdown` gains its matching field, so `/context`
reports what the index costs.

Bodies enter the prompt only as `skill_load` tool results, which live in the
transcript like any other tool result and age out through compaction. This is
the same reasoning section 3 of the design spec gives for reaching recall
through `recall_search` rather than injecting it: loading is visible in the
transcript, costs nothing on a turn that does not need it, and is checked by
the policy engine like any other call. No session state records what is
loaded, and nothing survives compaction verbatim.

### Configuration

    [skills]
    scope = "global"            # or "workspace"; default global
    dir   = "~/.spore/skills"   # global scope only

`global`, the default, is one directory read by every session and the target
of every install. It is the answer for a personal agent: skills are the
operator's, not a project's, and one directory is what an operator can
actually manage.

`workspace` makes each session read and install into `.spore/skills` under
its own root, so a project's skills travel with the project. Two consequences
must be understood before choosing it. A session rooted at a cloned
repository will load text the operator did not write into its prompt index,
which is why this is opt-in and not the default — spore's policy model
assumes the prompt is yours. And a session with no directory of its own
(`<data_dir>/sessions/<id>`: the web UI, the scheduler, a bridge) simply sees
no skills.

### `/skills`

Lists each skill's name, description and estimated body size; marks the ones
already loaded in this session, derived by scanning the transcript for
`skill_load` results rather than by keeping state; and reports the per-file
errors `Load` returned, so a skill with broken frontmatter is visible rather
than silently missing from the index.

## 5. Errors

| Case | Behaviour |
|---|---|
| Unknown command name | 400 with the list of valid names |
| Unknown session | 404, through the existing `findSession` |
| `/clear` or `/compact` with a turn running | 409, reusing `ErrTurnRunning` |
| `/compact` with nothing outside the protected window | 200, saying so |
| `skill_load` on a missing or malformed skill | tool error result, turn continues |
| `skill_install` over an existing name | an update; the approval prompt says so |
| Malformed SKILL.md frontmatter | skipped by `Load`, reported by `/skills` |
| Provider with no configured prices | zero cost, token counts intact |

## 6. Testing

Table tests per package, no network, following section 10 of the design spec.

- `internal/daemon/commands_test.go` — every command through the real
  handler, in the style of `api_test.go`; the 400 and 409 paths; the hub
  event published by `/clear` and `/compact`.
- `internal/agent` — a property test that `SnapshotBreakdown` sums to
  `SnapshotTokens`, so the two cannot drift; the skills section and its
  budget overflow; `Compact` folding unconditionally while `MaybeCompact`
  stays threshold-gated; the nothing-to-fold case.
- `internal/store` — `SessionUsage` and `TotalUsage` including the per-model
  grouping; `/clear`'s boundary move; an empty summary writing no
  `recall_fts` row.
- `internal/skill` — load and validation; a name disagreeing with its
  directory; traversal-shaped names; cache reload semantics on a
  directory-level failure.
- `internal/tool/skill` — `skill_load` truncation and unknown-name results;
  `skill_install` atomic write and its rejection of bad names.
- `internal/policy` — `skill_install` with `patternOK = false` downgrading
  `ScopePattern` to `ScopeOnce`; the `remote` profile denying it. These build
  their config explicitly rather than from `config.Default()`: `Load` adds
  the baseline deny, and a policy test built on the bare default silently
  loses its security assertions.
- `cmd/spore` — slash parsing in both `tui_test.go` and the plain loop,
  including that a bare `/` and a message containing a slash are still
  messages.

## 7. Out of scope

Discord slash commands beyond `/new`. Skills carrying supporting files beyond
`SKILL.md`. Installing a skill from a URL or an archive. `/clear --new`, which
would open a session and rebind the thread. Sticky loaded skills that survive
compaction verbatim. A structured payload on `CommandResult`.
