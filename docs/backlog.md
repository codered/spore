# Backlog

Wanted, not yet scheduled, and known gaps in what has shipped. Each entry
records what was asked for -- or what was knowingly left undone -- and the open
questions that must be answered before it can be specified, so the next
brainstorm starts where this one stopped rather than from the request.

Nothing here is a commitment to an order. Stages 5a, 5b, 5c, 6 and 7 have all
shipped; the staged plan in section 11 of the design spec is complete, and
everything below is what has been asked for since.

## Chat commands and skills: shipped, with two things left

Closed. All five asked-for commands shipped: `/clear` and `/compact` in plan 7,
and `/skills` with this change. The commands are client-side intercepts in the
Bubble Tea model, with one daemon endpoint each where new server behaviour was
needed (`POST /compact`, `GET /skills`); the open question about a combined
`POST /api/sessions/{id}/commands` dispatch table was answered by the cheaper
path -- plan 7 wired the intercepts, and the web UI never saw them.

The `/skills` open question was answered by building the reading that was
asked for: `SKILL.md` folders under the data directory, loaded on demand
through `skill_load` and the ask-gated `skill_install`. The command lists each
skill with its estimated body size, marks the ones already loaded by scanning
the transcript for `skill_load` calls, and reports the per-file errors `Load`
returned.

Scoping the command found a wiring bug that shipped with the skills
subsystem: `Snapshot.Skills` was never populated, so the "Skills you can
load" index in the system prompt was empty in every production turn. The fix
is in this change -- the agent holds the same `*skill.Caches` the tools were
built with, and `Snapshot` fills the index on every turn.

## The skill cache never evicts

`skill.Caches` holds one `Cache` per skills directory and nothing removes an
entry. Under the default global scope that is exactly one entry and costs
nothing. Under `skills.scope = "workspace"` it is one entry per distinct
session root the daemon has ever served — a mutex, a timestamp and a list of
names and descriptions each — so it grows with session history rather than
with live sessions.

This is the gap `internal/workspace/describers.go` used to have and no longer
does: `Describers.Describe` sweeps entries idle beyond `idleTTL` on every
miss, which is about ten lines and bounds the map by live use. The same sweep
would work here unchanged.

**Open questions**

1. Is a TTL sweep the right rule for skills, given a skills directory is read
   far less often than an environment section is rebuilt? A cache that is only
   consulted once per turn may want a longer idle window than the describers'.
2. Is anything else keyed by session root and unbounded, or are these the only
   two?

## Sub-agents: shipped

Closed. `agent_run` waits for a sub-agent's answer, `agent_spawn` starts one
in the background and `agent_result` collects it. Two tools were chosen over
one tool with a flag. `/agents` lists what a session launched. Section 1 of
the design spec no longer lists sub-agents as a non-goal.

The four open questions were answered as follows:

1. Approvals: a child gets the trust profile of the agent that launched it,
   so a Discord-launched child stays on `remote`. Its asks go to the human at
   the root of the chain through an ancestor walk that only goes up, so a
   child can answer neither its own approval nor a sibling's. Remembered
   "this session" answers are looked up at the root, so one answer covers the
   whole tree.
2. A sub-agent is a session with a `parent_id`. `ListSessions` hides children
   unless asked, and `spore session list --all` shows them.
3. Budget: `[subagents]` sets the maximum depth, the maximum cost of the
   whole tree and the maximum number of children running at once under one
   root.
4. Seeing them: `/agents` and `GET /api/sessions/{id}/agents` poll rather
   than stream, so a child's output stays off the parent's stream.

## Deleting a fact leaves its vector behind

Closed. The mirror now carries deletions as well as insertions. A removal from
`recall_fts` writes a row to `recall_tombstones` in the same transaction, and
the mirror drains that feed under a second cursor, `recall_sync.del_cursor`,
after each insert pass. `recall.Recall` gained `Delete`, so every backend
answers for deletion rather than one of them silently lacking it. Design:
`docs/superpowers/specs/2026-09-19-spore-recall-delete-path-design.md`.

The three open questions are answered:

1. **A tombstone, not a diff.** The tombstone is exact and costs one table plus
   a seven-day sweep; the diff needed no schema but cost a full scan of both
   sides and left a stale window measured in minutes. The feed is general over
   `(kind, ref_id)`: fact deletion is its only producer today, but the
   `messages` and `summaries` delete triggers write tombstones too, so message
   and summary deletion work on the day something deletes one.
2. **Deletion may fail, and is retried rather than lost.** A failed delete
   stops the pass with the cursor where it was, so the next tick retries the
   same tombstone; the ones behind it wait. The error is returned rather than
   swallowed, so `Run` reports the mirror as behind instead of logging that it
   caught up. This is the head-of-line blocking that insert batches already
   have.
3. **`recall reindex` stays the escape hatch**, and now also clears the
   tombstone table and zeroes `del_cursor` -- deletes queued against a dropped
   collection mean nothing. It is not worth running on a schedule: the sweep
   discards a tombstone only after seven days, which is far longer than a
   sidecar is plausibly down.

The one case worth remembering: a fact deleted and written again under the same
name occupies the same object id, because `objectID` hashes kind and ref id
alone. Before applying a tombstone the mirror asks whether `recall_fts` holds a
row for that key, and skips it if so. Without that guard the delete path
destroys live vectors, which is worse than the bug it fixes.

Proved end to end against a real Weaviate by `TestDeletedFactLeavesNoVector`
in the `weaviate`-tagged suite: index a fact, find it by semantic search,
delete it, run one mirror pass, and it is gone.

## The container tests: both suites have now run

Closed, and kept here because the next person to ask "has this ever actually
run?" deserves the answer rather than the question. Plan 5b added
`internal/recall/weaviate/integration_test.go` and the `make test-weaviate`
target; plan 5c added `internal/trace/phoenix/integration_test.go` and
`make test-phoenix`. Both have now been run against real servers on the
development machine, which does have Docker.

`make test-phoenix` passed against a real Phoenix collector: the exporter spore
builds is accepted on the default endpoint, and the collector logged the trace
export it received. `make test-weaviate` passed against a real Weaviate and its
model2vec sidecar, including `TestLiveRoundTrip` -- the compose file starting,
readiness detection, the collection created as mapped, and a full index and
search round trip through a real embedding container. Neither run left a
container or a volume behind.

The default suite is unaffected: both suites are guarded by a build tag
(`-tags phoenix` or `-tags weaviate`) and a `docker` lookup, so they skip
rather than fail where the containers are absent, and `make test` compiles
neither.

**Open questions**

1. Should either suite run in CI, and if so, is it a gate or advisory? Pinned
   images and a network make them the first tests here that can fail for
   reasons unrelated to the change. A local run proves the path works; it does
   not stop it rotting.
2. Nothing else. The honesty question these entries used to raise -- whether
   `spore recall setup` and `spore trace setup` were shippable while untested
   -- is answered: both provisioning paths have been exercised end to end.

## MCP path arguments are not checked against the session workspace

Closed. The design spec (section 6) promised that MCP tool path arguments are
evaluated against the calling session's workspace; `baselineDeny` bounded
`fs_*` only, and `mcp__*` appeared solely in the editable default `ask` list.
An MCP call naming a path outside the session's workspace resolved to `ask`,
and a human who approved it ran it with nothing in the policy engine bounding
it. `baselineDeny` now carries `mcp__*(any path outside workspace)`, which no
approval, learned rule or profile can override. Design:
`docs/superpowers/specs/2026-09-15-spore-mcp-path-containment-design.md`.

The three open questions are answered:

1. **Baseline deny, not an editable default.** Section 6 already promised a
   hard bound, and an editable rule protects only operators who never edit it.
2. **Paths are found by name and by shape.** Values under path-like key names
   at any depth, plus any string at any depth that looks like an absolute
   path, a `~` path or a `file://` URI. Name-only detection misses servers
   with unusual argument names; schema-based detection would trust a schema
   written by the server being restricted, the same reason `readOnlyHint` is
   ignored. Relative paths are judged where the server opens them, which is
   the ceiling, not the session root.
3. **Yes, an opt-out, per server.** `local_paths = false` on `[[mcp.server]]`,
   set by the operator who declared the server, for one whose paths are remote
   — repository paths, object keys. Not per tool: tool names change between
   server versions and a per-tool list would silently stop matching.

## The skill cache evicts idle directories

Closed. `skill.Caches` swept nothing, so under `skills.scope = "workspace"` it
held one entry per distinct session root the daemon had ever served. It now
sweeps entries idle beyond an hour on every miss, the same rule
`internal/workspace/describers.go` uses, which bounds the map by live use
instead of by session history.

The open question about the right idle window is answered by use rather than by
argument: a skills directory is read once per turn, so an entry untouched for an
hour belongs to a session that is over. The other question — whether anything
else is keyed by session root and unbounded — was checked at the same time, and
these two were the only ones.
