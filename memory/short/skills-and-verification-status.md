# Skills and verification status

The `/skills` feature shipped. Recent fixes cover production wiring, plain/non-TTY interception, matching `skill_load` results, folded messages after clear/compaction, and regression tests. Build, vet, and `go test ./...` pass with `sqlite_fts5`. Remaining backlog: sub-agents, Weaviate vector delete, MCP path enforcement, and CI for container suites. Full history is in `long/skills-and-verification-status.md`.
