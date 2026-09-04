# spore design and implementation stages

The project is being implemented in five distinct plans:
1. **Core**: Store and schema, config, provider interface, agent loop, compaction, router, and basic CLI (`spore once`, `spore chat`).
2. **Tools and policy**: Tool registry, filesystem, shell, web, and policy engine (allow/ask/deny).
3. **Daemon and web UI**: HTTP/SSE API, multi-client sessions, and embedded web UI.
4. **MCP and Telegram**: MCP client host and Telegram bridge.
5. **Memory and recall**: Fact files, FTS search, and the Recall interface (including Weaviate backend).

The current status is **Plan 2 (tools and policy)** implementation.
