# Backlog

Wanted, not yet scheduled, and known gaps in what has shipped. Each entry
records what was asked for -- or what was knowingly left undone -- and the open
questions that must be answered before it can be specified, so the next
brainstorm starts where this one stopped rather than from the request.

Nothing here is a commitment to an order. Stages 5a, 5b, 5c and 6 have all
shipped; the staged plan in section 11 of the design spec is complete, and
everything below is what has been asked for since.

## Chat commands

The interactive client has no commands. Wanted, in the order they were asked
for:

- `/clear` — start fresh. Ambiguous by design: spore never deletes messages,
  so this is either a new session or a summary boundary moved to "now".
- `/compact` — run `MaybeCompact` on demand rather than waiting for the
  0.75 threshold. The mechanism already exists (`internal/agent/compact.go`);
  what is missing is a way to ask for it.
- `/context` — what is in the prompt right now: system, environment section,
  facts, summary, live messages, each with its token estimate.
  `SnapshotTokens` already computes every part of this.
- `/usage` — tokens and cost, for the session and in total. The `messages`
  table already carries `tokens_in`, `tokens_out` and `cost_usd` per row.
- `/skills` — meaning undecided; see the open question below.

**Open questions**

1. Do commands live in the daemon API or in the terminal client? The spec's
   invariant is that the CLI and the web UI are thin clients over one API
   (section 8), which argues for `POST /api/sessions/{id}/commands` so every
   surface gets them and the behaviour is tested once. The cheaper answer is
   to handle them in the Bubble Tea model, where the web UI never sees them.
2. What is a skill here? Three readings, and they are different sizes:
   Claude-Code-style `SKILL.md` folders loaded on demand (a subsystem the
   size of the facts plan); an introspection command listing the tools
   actually available and the policy decision each would get (no new
   subsystem); or a pin over the existing fact files, which are already
   markdown with frontmatter.
3. What does `/clear` do to a session that a bridge is bound to? A Discord
   thread maps to one session (`bridge_bindings`), so "new session" would
   silently unbind the thread.

## Sub-agents

Wanted: launching sub-agents, and seeing which are running.

The design spec lists "Sub-agents / agent teams" under **non-goals (v1)**,
deferred until the single-agent loop is solid. Building this reverses that
decision, so section 1 of the spec is amended by the same change, not merely
extended.

**Open questions**

1. Approvals. A sub-agent that calls `shell_exec` reaches the same guard.
   Does its approval surface to the parent's human tagged with its origin
   (no new trust surface, and a Discord-launched sub-agent stays on the
   `remote` profile), does it get a stricter profile of its own, or is it
   confined to the allow list so it can never block on a human?
2. Is a sub-agent a session? Making it one buys persistence, recall indexing
   and `session show` for free, and costs a `parent_id` column and a rule for
   what `ListSessions` hides.
3. Budget. `maxIterations` bounds one turn's round trips; nothing bounds a
   tree of agents. A sub-agent that launches sub-agents needs a depth cap and
   a cost ceiling, and both belong in config next to `context`.
4. What "seeing them running" means on each surface: the terminal has a live
   view, the web UI has SSE, Discord has neither.

## Deleting a fact leaves its vector behind

Known gap, shipped that way in 5b, deliberately. The FTS index is written
inside `AppendMessage`'s transaction and `UnindexFact` deletes the row there,
but Weaviate is a mirror driven forward from a watermark over `recall_fts` and
the mirror only moves forward. It has no delete path, so the vector copy of a
deleted fact survives until the next `recall reindex` and can surface in a
semantic search in the meantime.

The blast radius is bounded -- a stale hit on content the user removed, never a
wrong answer to a keyword search, and never data loss -- which is why 5b landed
without it. Fixing it properly is a delete path through the mirror, which is a
task of its own rather than something to smuggle into a backend review.

**Open questions**

1. What does the mirror learn deletions from? The watermark is a high-water
   mark over an append-only feed, and a deletion is not an append. Either
   `UnindexFact` writes a tombstone row the feed carries forward, or the
   mirror periodically diffs its object ids against `recall_fts`. The
   tombstone is exact and costs a table plus a retention rule; the diff needs
   no schema change and costs a full scan of both sides.
2. Is deletion allowed to fail? Every other Weaviate write is non-fatal by
   design -- the store is a mirror and the keyword index is the record. A
   delete that silently fails leaves exactly the stale object this entry is
   about, so it may need a retry that the mirror's forward-only model has no
   place to put.
3. Does `recall reindex` stay the escape hatch either way, and is it worth
   running on a schedule until the delete path exists?

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

Known gap, long-standing. The design spec (section 6) says MCP tool path
arguments should be evaluated against the calling session's workspace, the
same way filesystem tools are bounded by policy. The implementation does not:
`internal/config/config.go` builds `path outside workspace` rules for `fs_*`
only, and `mcp__*` appears only in the default `ask` list, with no path
predicate. An MCP tool call naming an absolute path outside the calling
session's workspace — or outside the ceiling entirely — resolves to `ask`,
and if a human approves it, the call runs unbounded by the policy engine.

The blast radius is bounded by the ask-gate and the remote profile's blanket
denial. For a local session, every MCP call requires human approval before it
runs, which gives the operator a chance to notice and refuse calls with
suspicious paths. For the `remote` trust profile (Discord and scheduled jobs),
`mcp__*` is denied outright, so a bridge user cannot reach any server at all.
This gap predates stage 6 — before that change, the workspace was the single
ceiling and `mcp__*` was equally unchecked then. It is not a regression.

Fixing it properly means adding an `mcp__*` path rule to the baseline deny
set (the list no approval may override), which is a behaviour change with its
own blast radius and needs its own design pass.

**Open questions**

1. Does the path-check rule belong in the baseline deny set (so it always
   applies and no approval can override it) or in the default ask/deny lists
   (editable by the operator)? The ask-gated behaviour of local sessions
   already gives the human a chance to refuse; the baseline deny would be a
   different level of containment.
2. Path-argument extraction recognises only certain argument names (those
   listed in the MCP spec as path-shaped). A server using a different name for
   its path argument would slip past any rule. Does a rule give enough cover
   to matter, or does it need to extract broader context?
3. Does a server that legitimately works outside any session's root — a shared
   index or an external tool — need an opt-out, or is it enough to rely on the
   ask-gate to let the operator choose?
