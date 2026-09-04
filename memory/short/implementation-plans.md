# spore design and implementation stages

The project is being implemented in five distinct plans:
1. **Core**: Store and schema, config, provider interface, agent loop, compaction, and router.
2. **Tools and policy**: Tool registry, filesystem, shell, web, and policy engine.
3. **Daemon and web UI**: HTTP/SSE API, multi-client sessions, and embedded web UI.
4. **Discord and MCP**: 4a the Discord bridge, then 4b the MCP client host.
5. **Memory and recall**: Fact files, FTS search, and the Recall interface.

Current status: **Plans 1-3 complete** (Plan 3 merged as PR #3 on 2026-08-31).
Plan 4a (Discord bridge) is next; its spec section was amended on 2026-08-31 to
replace Telegram with Discord.
