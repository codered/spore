# Skills and verification status

The stray `cmd/spore/spore` binary was removed and ignored in commit `486714e`. The `/skills` feature shipped in commit `44035fb`, including `GET /api/sessions/{id}/skills`, TUI rendering, server-side loaded detection, and per-file errors.

A production wiring bug was fixed: `Snapshot.Skills` now receives the shared `skill.Caches` used by tools, and Snapshot fills the prompt index. Follow-up commit `df5c324` fixed plain/non-TTY `/skills` interception, required successful matching `skill_load` tool results, ignored folded messages after clear/compaction, and added regression tests.

Verification passed with `sqlite_fts5`: build, vet, and `go test ./....` The remaining backlog is sub-agents, the Weaviate vector delete gap, MCP path enforcement, and CI for container suites. The untracked `spore_image_gen.md` file remains intentionally untouched.
