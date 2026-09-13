# spore design and implementation stages

The staged plan in section 11 of the design spec is **complete**. Plans 1-7 have
all shipped to `master`:

1. **Core**: store, config, providers, agent loop, compaction, router, CLI.
2. **Tools and policy**: registry, filesystem, shell, web, policy engine (#2).
3. **Daemon and web UI**: HTTP/SSE API, multi-client sessions, embedded UI (#3).
4. **Discord and MCP**: 4a the Discord bridge (#4), 4b the MCP client host.
5. **Memory and recall**: 5a local fact files and FTS (#6), 5b the Weaviate
   backend (#8), 5c the Phoenix tracing sidecar (#9).
6. **Per-session workspace** (#13, #16).
7. **Chat commands and skills**: `/clear`, `/compact`, `/context`, `/usage`,
   `/skills`, plus `skill_load` and the ask-gated `skill_install`
   (#17, #18, #20, #21, #23, #24; fixes in #25, cache eviction in #26).

Current status: **no plan in flight.** Everything wanted from here lives in
`docs/backlog.md`, which is the authority, not this file. The largest open item
is **sub-agents**, which reverses a stated v1 non-goal and so amends section 1
of the spec. Smaller gaps: Weaviate vector delete, MCP path enforcement, and CI
for the container suites.

Full per-plan detail is in `long/implementation-plans.md`.
