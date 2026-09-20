# The recall delete path — implementation plan

**Goal:** deleting a fact removes its vector from Weaviate, not just its row
from `recall_fts`. The mirror gains a second, non-monotonic channel — a
tombstone feed — so a deletion reaches a backend that only ever moved forward.

**Architecture:** spore is a single Go binary. `internal/store` owns SQLite and
every write to `recall_fts`. `internal/recall` declares the search interface;
`sqlitefts` and `weaviate` implement it and `Fallback` composes them.
`internal/recall/mirror` carries rows from the keyword index to a vector
backend from a watermark, because an HTTP call cannot join the transaction that
writes a message.

**Tech stack:** Go, SQLite (FTS5, `-tags sqlite_fts5`), Weaviate via
`weaviate-go-client/v5`, `make` for every gate.

**Spec:** `docs/superpowers/specs/2026-09-19-spore-recall-delete-path-design.md`.
Read it before task 01. It carries the reasoning; this file carries the steps.

## Global constraints

- **Every command runs from the repository root**, and every `go` invocation
  carries `-tags sqlite_fts5`. `make test` already does.
- **Tests first.** Write the failing test named in the task's gate, watch it
  fail for the right reason, then write the code. The gate asserts `--- PASS:
  <name>`, so a test that does not exist fails the gate.
- **Assert on stored state, not on a nil error.** ◪ In this repository a green
  suite once hid a missing store write for weeks (the SA4006 regression in
  `internal/policy/guard.go`): a statement whose value looked unused was the
  only thing moving an approval out of `pending`. Every tombstone test reads
  `recall_tombstones` back.
- **Resolve temp dirs through symlinks** in any test that compares paths. ◪ On
  macOS `/var` is a symlink to `/private/var`, which reddened CI five times.
  Follow the pattern already in the tree.
- **Comments carry reasoning, not restatement.** Match the density and voice of
  the file you are editing; `internal/recall/mirror/mirror.go` is the model.
- **Do not widen the change.** No session-deletion feature, no `recall status`
  additions, no changes to how inserts are mirrored. The spec's section 6 lists
  what is out of scope.
- **Gates are the contract.** `python3 docs/plans/2026-09-19-recall-delete-path/run.py --status`
  from the repository root reports measured state. Run it after each task.
- **One commit per task**, subject in the sentence style of recent history.

---

- [x] **Task 01 — Delete reaches Weaviate** <!-- task_01_delete_reaches_weaviate.py -->

Add the method and implement it everywhere, before any of the tombstone work.
Nothing calls it yet; this task proves the far end of the seam works.

1. Write `internal/recall/weaviate/weaviate_test.go` additions first:
   - `TestDeleteRemovesObjectByID` — stand up an `httptest.Server`, point a
     `Backend` at it, call `Delete(ctx, recall.KindFact, "prefers-tabs")`, and
     assert the server saw `DELETE` on the path carrying
     `objectID(recall.KindFact, "prefers-tabs")`. Derive the expected id with
     the same `objectID` helper rather than pasting a UUID.
   - `TestDeleteMissingObjectIsSuccess` — the server answers 404; `Delete`
     returns nil.
2. `TestFallbackDeleteUsesPrimary` in `internal/recall/fallback_test.go`: the
   fake primary records the call, the secondary records nothing.
3. `TestDeleteRemovesRow` in `internal/recall/sqlitefts/sqlitefts_test.go`.
4. Then implement:
   - `internal/recall/recall.go` — add to the `Recall` interface:
     ```go
     // Delete removes one indexed chunk. It is not an error for the chunk to
     // be absent: the mirror can hold a tombstone for a row whose insert never
     // reached the backend, and a delete that can never succeed would stall
     // the cursor behind it forever.
     Delete(ctx context.Context, kind, refID string) error
     ```
   - `internal/recall/weaviate` — `Data().Deleter().WithClassName(Collection).
     WithID(objectID(kind, refID)).Do(ctx)`. Wrap errors through the existing
     `tidy()`. A 404 from the client is success; detect it from the client's
     typed error rather than by matching the message text if the client exposes
     a status code, and say in a comment which you used and why.
   - `internal/recall/sqlitefts` — `DELETE FROM recall_fts WHERE kind = ? AND
     ref_id = ?`. Note in a comment that this backend is never a mirror target;
     the method is real so the interface does not lie.
   - `internal/recall/fallback.go` — forward to the primary, with the same
     one-line reasoning `Index` carries.
   - The three test fakes that implement `recall.Recall`:
     `internal/recall/fallback_test.go`, `internal/recall/mirror/mirror_test.go`,
     `internal/tool/mem/recall_test.go`. The mirror's fake should **record**
     deletes; tasks 02-04 assert on them.

---

- [x] **Task 02 — Tombstone feed and the delete phase** <!-- task_02_tombstone_feed.py -->

The vertical slice: a fact deleted in SQLite becomes a `Delete` at the backend.

1. Schema, in `internal/store/schema.go`:
   ```sql
   CREATE TABLE IF NOT EXISTS recall_tombstones (
     id         INTEGER PRIMARY KEY AUTOINCREMENT,
     kind       TEXT NOT NULL,
     ref_id     TEXT NOT NULL,
     created_at TEXT NOT NULL
   );

   CREATE INDEX IF NOT EXISTS idx_recall_tombstones_age ON recall_tombstones(created_at);
   ```
   `AUTOINCREMENT` is load-bearing: SQLite reuses the highest rowid after a
   delete, and a reused id would move a backend's cursor backwards over rows it
   had already applied. Say that in the table's comment, in the voice of the
   `recall_sync` comment above it.
2. Migration. `recall_sync` exists in shipped databases, so `del_cursor` is an
   `ALTER TABLE`, not a schema-string edit. Write `migrateRecallSync` in the
   shape of `migrateSessions` (`internal/store/store.go:72`) — read `PRAGMA
   table_info(recall_sync)`, add `del_cursor INTEGER NOT NULL DEFAULT 0` when
   missing — and call it from where `migrateSessions` is called.
   `TestRecallSyncGainsDelCursor` opens a database written without the column
   and asserts it is there afterwards.
3. `UnindexFact` writes a tombstone in the same transaction as the delete. It
   is currently a bare `deleteIndex` against `s.db`; it needs a transaction.
   **`IndexFact` must not write one** — its delete-then-insert is an update, and
   a tombstone there is churn the guard would discard. `TestUnindexFactWrites-
   Tombstone` and `TestIndexFactWritesNoTombstone` are both required.
4. Store feed methods, beside `IndexRowsSince`:
   - `Tombstone` struct: `ID int64`, `Kind`, `RefID`, `CreatedAt string`.
   - `TombstonesSince(ctx, cursor int64, limit int) ([]Tombstone, error)` —
     `id > cursor ORDER BY id LIMIT ?`.
   - `DelCursor(ctx, backend string) (int64, error)` and
     `SetDelCursor(ctx, backend string, cursor int64) error`, mirroring
     `SyncCursor`/`SetSyncCursor`. `SetDelCursor` must upsert the same
     `recall_sync` row rather than insert a second one.
   - `IsIndexed(ctx, kind, refID string) (bool, error)` — does `recall_fts`
     hold a row for this key. This is the guard.
5. The mirror's delete phase, in `Mirror.Once`, **after** the insert loop. Widen
   the `Source` interface with the four new methods.
   ```text
   for each tombstone with id > del_cursor, oldest first:
       IsIndexed(kind, ref_id)?
         yes -> the key was re-indexed; the tombstone is stale, skip it
         no  -> target.Delete(ctx, kind, ref_id)
       SetDelCursor only after the target accepted
   ```
   Inserts run first on purpose — a row read into an in-flight batch can be
   deleted in SQLite before the batch lands, and draining afterwards catches it
   on the same pass. Put that reasoning in the comment.
6. Tests in `internal/recall/mirror`, against a **real SQLite store**, not a
   fake `Source` — the seam is a table:
   - `TestDeletePropagatesToTarget` — index a fact, `Once`, unindex it, `Once`,
     assert the fake target recorded the delete for that `(kind, ref_id)`.
   - `TestStaleTombstoneSkipped` — delete then re-index the same name before
     the pass; assert no delete reached the target.
   - `TestDeleteFailureKeepsCursor` — a target that errors leaves `del_cursor`
     unmoved, and the next pass retries the same tombstone.
   - `TestInsertsRunBeforeDeletes` — the fake records call order.

---

- [ ] **Task 03 — Every removal writes a tombstone** <!-- task_03_all_removal_producers.py -->

Task 02 covered `UnindexFact`. Two producers remain.

1. `ClearFactIndex` — in one transaction:
   ```sql
   INSERT INTO recall_tombstones (kind, ref_id, created_at)
     SELECT kind, ref_id, ? FROM recall_fts WHERE kind = 'fact';
   DELETE FROM recall_fts WHERE kind = 'fact';
   ```
   This is the path that catches a fact file deleted by hand: the daemon wipes
   the index at start and re-indexes what is still on disk, so the file that is
   gone leaves a tombstone and nothing re-creates it. The guard discards the
   tombstones for the facts that came back.
2. The two existing delete triggers in `schema.go`
   (`recall_fts_messages_ad`, `recall_fts_summaries_ad`) — insert the tombstone
   **before** the delete inside the trigger body, reading `kind` and `ref_id`
   from the `recall_fts` row about to go. A trigger cannot be attached to an
   FTS5 virtual table, which is why this sits in the triggers that already own
   the delete; note that in a comment.
3. Nothing fires those triggers in production today. They are wired now so that
   session deletion works the day it lands — do not add session deletion.
4. Tests: `TestClearFactIndexWritesTombstones`,
   `TestMessageDeleteTriggerWritesTombstone`,
   `TestSummaryDeleteTriggerWritesTombstone` in `internal/store`, and
   `TestMessageDeletePropagates` in `internal/recall/mirror` — delete a message
   row, run `Once`, assert the target saw the delete.

---

- [ ] **Task 04 — Sweep and reindex reset** <!-- task_04_sweep_and_reset.py -->

1. `tombstoneTTL = 7 * 24 * time.Hour`, a package constant in `mirror` beside
   `batchSize`. At the end of each pass, sweep rows older than it — a store
   method, `SweepTombstones(ctx, before time.Time) (int, error)`, so the SQL
   stays in `internal/store`.
2. The sweep can drop a tombstone a long-offline backend never applied, which
   loses that deletion. That is accepted; `recall reindex` is the escape hatch.
   Put the trade-off in the constant's comment, not only in the spec.
3. `Mirror.Reset` additionally zeroes `del_cursor` and clears
   `recall_tombstones`: `cmd/spore/recall.go` drops the collection before
   calling it, and pending deletes against a dropped collection mean nothing.
4. Tests: `TestSweepDropsAgedTombstones`, `TestSweepSparesFreshTombstones`,
   `TestResetClearsTombstonesAndDelCursor`. Control age by writing
   `created_at` directly rather than by sleeping.
5. This task's component gate runs the **whole** suite (`./...`), because
   `Reset` and the `Source` interface are reached from `cmd/spore`.

---

- [ ] **Task 05 — Live Weaviate proof, backlog closed** <!-- task_05_live_weaviate_proof.py -->

1. `TestDeletedFactLeavesNoVector` in
   `internal/recall/weaviate/integration_test.go`, under the existing
   `weaviate` build tag and its `docker` lookup: index a fact through the
   mirror, search and find it, delete the fact through the store, run
   `mirror.Once`, search again and find nothing. Follow the existing
   `TestLiveRoundTrip` for compose setup and teardown; leave no container or
   volume behind.
2. Run it: `spore recall setup` if needed, then `make test-weaviate`. This gate
   needs Docker and fails without it — a delete path that has never run against
   a real vector store is the condition the backlog entry was about.
3. Rewrite the `docs/backlog.md` entry "Deleting a fact leaves its vector
   behind" as closed, in the voice of the other closed entries. It must begin
   `Closed. The mirror now carries deletions` and answer the three open
   questions the entry raised: what the mirror learns deletions from, whether
   deletion may fail, and whether `recall reindex` stays the escape hatch.

---

## When every gate is green

Run `make lint vulncheck tidycheck test` before opening the PR — CI runs all of
them and ◪ the lint gate is pinned, so a finding here is a real finding. Do not
"fix" a lint finding by deleting a statement without checking whether the
expression writes to the store; that is exactly how the SA4006 regression
happened.
