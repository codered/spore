# Backlog

Wanted, not yet scheduled. Each entry records what was asked for and the open
questions that must be answered before it can be specified, so the next
brainstorm starts where this one stopped rather than from the request.

Nothing here is a commitment to an order. The next stage is 5b (Weaviate
recall backend, `recall setup|status|teardown`, `trace setup`).

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
