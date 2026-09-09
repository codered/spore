# spore design and implementation stages

The staged plan in section 11 of the design spec is **complete**. What follows
is what each plan actually shipped as, since the numbering drifted from the
original spec in two places: plan 4 replaced Telegram with Discord (amended
2026-08-31), and plan 5 split into three sub-plans.

1. **Core** — store and schema, TOML config with env interpolation, the
   provider vocabulary and registry, Anthropic and OpenAI-compatible streaming
   providers, the call-site model router, pure context assembly, the agent loop,
   compaction with a preserved message archive, OpenTelemetry tracing on
   OpenInference conventions, and the `spore once` / `chat` / `session` CLI.
   Landed as a commit series, not a PR.
2. **Tools and policy** — tool registry, filesystem, shell and web tools, and
   the allow/ask/deny policy engine. PR #2.
3. **Daemon and web UI** — HTTP/SSE API, multi-client sessions, embedded web
   UI. PR #3.
4. **Discord and MCP** — 4a the Discord bridge (PR #4); 4b the MCP client host,
   merged as a commit series: transports with a confined stdio child, remote
   tool adaptation with results marked as external data, dynamic tool sources in
   the registry, supervision with `tools/list_changed` re-listing, and
   `spore mcp list`.
5. **Memory and recall** — 5a fact files, FTS search and the Recall interface
   (PR #6); 5b the Weaviate backend (PR #8); 5c the Phoenix tracing sidecar
   (PR #9).
6. **Per-session workspace** — PR #13, extended by the worktree work in PR #16.
7. **Chat commands and skills** — `/clear`, `/compact`, `/context` and `/usage`
   wired as client-side Bubble Tea intercepts, with one daemon endpoint each
   where new server behaviour was needed (`POST /compact`,
   `GET /api/sessions/{id}/skills`). Then `/skills`, `skill_load`, and the
   ask-gated `skill_install`. PRs #17, #18, #20, #21, #23, #24; defect fixes in
   #25; the skill-cache idle sweep in #26.

Out of band: PR #7 put the environment in the prompt and built a real chat
interface.

## Status

**No plan is in flight.** `docs/backlog.md` is the authority on what is wanted
next; this file is history. Known open items as of 2026-09-08:

- **Sub-agents.** The design spec lists these under non-goals (v1), so building
  them amends section 1 rather than extending it. Four questions are open:
  how a sub-agent's `shell_exec` approval surfaces to the parent's human,
  whether a sub-agent is a session (`parent_id` plus a `ListSessions` rule),
  depth and cost caps for a tree of agents, and what "seeing them running"
  means on Discord, which has neither a live view nor SSE.
- Weaviate vector delete gap.
- MCP path enforcement.
- CI for the container suites.

## Two lessons worth carrying

Scoping `/skills` in plan 7 exposed a wiring bug that had shipped with the
skills subsystem: `Snapshot.Skills` was never populated, so the "Skills you can
load" index in the system prompt was empty in every production turn. A
subsystem can be fully built, fully tested and completely inert in production.

The skill cache (#26) repeated a shape `internal/workspace/describers.go` had
already solved: a map keyed by session root that grows with session history
rather than with live sessions. The fix both times is an idle-TTL sweep on
every miss. Worth checking any new session-root-keyed map against this.
