# spore — the /skills command and the prompt wiring fix

**Date:** 2026-09-06
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-09-04-spore-chat-commands-skills-design.md` section `/skills`; `2026-08-29-spore-design.md`

## 1. What this adds

The fifth asked-for command, `/skills`, was left out of plan 7 together with
a bug found while scoping it: `Snapshot.Skills` is never populated, so the
system prompt's "Skills you can load" index is empty in every production
turn. This change ships the command and fixes the wiring, because a listing
command would ship untestable while the index it describes never reaches the
model. The fix is the agent holding the same `*skill.Caches` the tools were
already built with, and the assembler reading it on every snapshot.

## 2. The prompt index fix

`internal/agent.Agent` gains one field:

```go
// Skills is the cache set shared with the skill tools. Nil means the tests
// that built the Agent directly with New; buildAgent attaches the real one.
Skills *skill.Caches
```

`Snapshot` resolves the skills directory the way the skill tools do —
`a.Cfg.SkillsDir(policy.WorkspaceFrom(ctx))` — and fills `snap.Skills` from
the cache. Under `skills.scope = "workspace"` a session with no workspace of
its own resolves to an empty directory string, and the cache answers zero
skills without an error, so the handler is safe by the same rule the tools
rely on.

`buildAgent` constructs the cache set and passes it to `buildTools` instead
of `buildTools` creating it privately. The tools and the prompt index read
the same caches, one definition, one reload pattern.

## 3. The endpoint

    GET /api/sessions/{id}/skills

    200 {"skills": [
           {"name": "review", "description": "...", "body_tokens": 410, "loaded": true}
         ],
         "errors": ["badfile: unknown frontmatter key "title""]}

The handler resolves the session's workspace to a directory with
`cfg.SkillsDir`, reads the cached set through the agent's `Caches`, sizes
each body with `agent.EstimateTokens`, and derives `loaded` by scanning the
session's messages for `skill_load` tool_use blocks. An empty directory
string — a workspace-scope session rooted at nothing — returns an empty list
with no error, matching the cache's own rule.

Per-file errors from the last load ride along as strings so a skill with
broken frontmatter is visible rather than silently absent from the index.

## 4. The client

`client.listSkills(ctx, sessionID)` returns the structured payload; the CLI
joins the transcript-scanning-free rendering. The Bubble Tea handler formats
one line per skill — name, description, token estimate, a `loaded` marker
when true — and one line per error when any are reported. The plain loop
inherits it, because both loops call the same render once rendering lives in
one function. Rendering is client-side like `/context` and `/usage`; the
server only computes the payload.

## 5. Testing

Daemon tests carry the behavioural weight: an endpoint test builds a session,
writes two skill files and one broken one into a temp skill directory, appends
a `skill_load` tool call to the transcript, and asserts loaded marking, body
sizes, and the error list. The agent test asserts the wired index reaches the
assembled system prompt. The TUI test asserts the render against a literal
payload struct.

## 6. Out of scope

Cache eviction (`Caches` entries are never evicted) remains deferred with the
describer cache in `docs/backlog.md`. The combined `POST commands` endpoint
the earlier design sketched remains out of scope; the five commands stand as
the client-intercepted pattern plan 7 established.
