# Recall delete path — Plan Index

## Seams

| Seam | From | To | Verified by |
|---|---|---|---|
| `recall.Recall.Delete` | internal/recall/mirror | weaviate, sqlitefts, Fallback | `TestDeleteRemovesObjectByID`, `TestFallbackDeleteUsesPrimary` |
| mirror -> weaviate HTTP | internal/recall/mirror | a real Weaviate object id | `TestDeleteMissingObjectIsSuccess` (httptest), `TestDeletedFactLeavesNoVector` (live) |
| `recall_tombstones` | internal/store | internal/recall/mirror | `TestDeletePropagatesToTarget` |
| `recall_sync.del_cursor` | internal/store | internal/recall/mirror | `TestDeleteFailureKeepsCursor` |
| `recall reindex` -> mirror | cmd/spore | internal/recall/mirror | `TestResetClearsTombstonesAndDelCursor` |

## Tasks

| # | Title | Kind | Tier | Touches | Crosses | Files | Script |
|---|---|---|---|---|---|---|---|
| 01 | Delete reaches Weaviate | manual | T2 | internal/recall, weaviate, sqlitefts, tool/mem | mirror -> weaviate HTTP | internal/recall/recall.go, internal/recall/fallback.go, internal/recall/weaviate/weaviate.go, internal/recall/sqlitefts/sqlitefts.go | tasks/task_01_delete_reaches_weaviate.py |
| 02 | Tombstone feed and the delete phase | manual | T2 | internal/store, internal/recall/mirror | store -> mirror | internal/store/schema.go, internal/store/store.go, internal/store/recall.go, internal/recall/mirror/mirror.go | tasks/task_02_tombstone_feed.py |
| 03 | Every removal writes a tombstone | manual | T2 | internal/store, internal/recall/mirror | store -> mirror | internal/store/schema.go, internal/store/recall.go | tasks/task_03_all_removal_producers.py |
| 04 | Sweep and reindex reset | manual | T2 | internal/store, internal/recall/mirror, cmd/spore | store -> mirror, reindex -> mirror | internal/recall/mirror/mirror.go, internal/store/recall.go | tasks/task_04_sweep_and_reset.py |
| 05 | Live Weaviate proof, backlog closed | manual | T2 | internal/recall/weaviate, docs | mirror -> live weaviate HTTP | internal/recall/weaviate/integration_test.go, docs/backlog.md | tasks/task_05_live_weaviate_proof.py |

## Notes on the tiers

Every task is T2. Task 01 crosses the process boundary to Weaviate through an
`httptest` server, which is a real HTTP request against the real client, not a
stub of it. Tasks 02-04 cross the store/mirror seam against a real SQLite
store rather than a fake `Source`, because the seam being wired is a table.

Task 05's gate needs Docker. It fails cleanly where Docker is absent rather
than skipping, because "the delete path has never run against a real vector
store" is the condition the backlog entry was about. ◪ The development machine
has Docker, and both tagged suites have been run there before.

## Why every task is manual

`apply()` raises `ManualTask` in all five; the gates are fully written and are
the contract. Two reasons, both from this repository rather than from the
general case:

1. The house style is a graded artifact here. Comments carry the reasoning for
   a decision, not a restatement of the code, and the prose is held to
   ASD-STE100 by the `mission-grade` skill. A script that emits Go would emit
   the wrong prose.
2. Tests come first. Each task's crossing gate names the tests it requires, so
   the worker writes those tests before the code that satisfies them. A
   scripted `apply()` would land the implementation first and invert that.

Gate names are exact. A gate that runs `go test -run` also asserts `--- PASS:
<name>` in the output, because `-run` matching nothing exits 0 and would pass
vacuously for a test nobody wrote.
