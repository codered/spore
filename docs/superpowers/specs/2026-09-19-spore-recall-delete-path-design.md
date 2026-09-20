# spore — the recall delete path

**Date:** 2026-09-19
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-09-03-spore-weaviate-recall.md` (the mirror is no longer
forward-only)
**Closes:** `docs/backlog.md`, "Deleting a fact leaves its vector behind"

## 1. What this fixes

Deleting a fact removes its row from `recall_fts` and leaves its vector in
Weaviate. `Store.UnindexFact` (`internal/store/recall.go:93`) deletes the
keyword row inside the transaction that owns it, but the Weaviate copy is
written by `internal/recall/mirror`, which moves forward from a watermark over
`recall_fts` rowids. A deletion is not an append, so the mirror cannot see it.
The vector survives until the next `recall reindex` and can surface in a
semantic search in the meantime.

The blast radius is a stale hit on content the user removed. It is never a
wrong answer to a keyword search and never data loss, which is why 5b shipped
without the fix.

### The scope is smaller than it looks

Only fact deletion can happen today. Production code deletes no sessions and
no messages: `DELETE FROM sessions` appears once in the tree, in
`internal/store/recall_test.go:127`. The `recall_fts` delete triggers on
`messages` and `summaries` exist and are correct, but nothing fires them. So
the live sources of a stale vector are exactly two, both fact-shaped:

1. The `mem` tool deleting a fact, through `UnindexFact`.
2. `Store.ClearFactIndex`, which wipes every fact row at daemon start and on
   `recall reindex` before re-indexing what is still on disk. A fact file
   deleted by hand leaves its vector behind the same way.

The mechanism below is nevertheless general over `(kind, ref_id)`, and the
existing triggers write tombstones too. Message and summary deletion then work
on the day something deletes them, at a cost of about six lines of SQL now
instead of a second design later.

## 2. Decisions

| Question (from the backlog) | Answer |
|---|---|
| What does the mirror learn deletions from? | **A tombstone table**, written in the same transaction as the `recall_fts` delete, drained by the mirror under its own cursor. A periodic id diff needs no schema but costs a full scan of both sides and leaves a stale window measured in minutes. An inline call from `UnindexFact` puts HTTP in the tool's path and loses the deletion entirely when Weaviate is down, which is the bug being fixed. |
| Is deletion allowed to fail? | **Yes, the way every other mirror write is allowed to fail.** A failed delete does not advance the cursor; it is logged and retried on the next tick. The tombstone is the durable record that makes the retry possible. |
| Does `recall reindex` stay the escape hatch? | **Yes**, and it now also clears the tombstone table and zeroes the delete cursor, because pending deletes against a dropped collection mean nothing. |
| Facts only, or every kind? | **Every kind.** One channel keyed on `(kind, ref_id)`; facts are simply its only producer today. |
| Where does `Delete` live? | **On `recall.Recall`.** An optional `Deleter` interface would let a backend silently lack a delete path, which is the exact bug class being closed. |

## 3. Architecture

### Schema

```sql
CREATE TABLE IF NOT EXISTS recall_tombstones (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  kind       TEXT NOT NULL,
  ref_id     TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_recall_tombstones_age ON recall_tombstones(created_at);
```

`AUTOINCREMENT` rather than a bare rowid: SQLite reuses the highest rowid after
a delete, and a reused id would move a backend's delete cursor backwards over
rows it had already applied.

The table carries no `fts_rowid`. An earlier draft did, to tell a deletion from
a later re-creation of the same name. It is not needed: `objectID` derives from
`kind` and `ref_id` alone, so there is at most one object per key, and "should
this object exist" is exactly "does `recall_fts` hold a row for this key". The
guard in section 3.3 reads that directly.

`recall_sync` gains a second cursor beside the first:

```sql
ALTER TABLE recall_sync ADD COLUMN del_cursor INTEGER NOT NULL DEFAULT 0;
```

One row per backend still holds both, so a backend cannot be caught up on
inserts and behind on deletes in two different rows. `recall_sync` already
exists in shipped databases, so this is a migration rather than a schema-string
edit — `migrateRecallSync`, in the shape of `migrateSessions`
(`internal/store/store.go:72`), reading `PRAGMA table_info` and adding the
column when it is missing. `recall_tombstones` is new, so `CREATE TABLE IF NOT
EXISTS` in `schema.go` covers it.

### Who writes a tombstone

Not every `DELETE FROM recall_fts`. `IndexFact` deletes and re-inserts as its
update path (`internal/store/recall.go:77`); a tombstone there would be churn
that the guard immediately discards. Tombstones come only from genuine
removals:

- **`UnindexFact`** — one row, in a transaction with the delete it records.
- **`ClearFactIndex`** — `INSERT INTO recall_tombstones (kind, ref_id,
  created_at) SELECT kind, ref_id, ? FROM recall_fts WHERE kind = 'fact'`
  before the delete, in one transaction.
- **The two existing delete triggers** on `messages` and `summaries` — the
  same insert-before-delete shape inside the trigger body. A trigger cannot be
  attached to an FTS5 virtual table, which is why the insert sits in the
  triggers that already own the delete rather than in one trigger on
  `recall_fts`.

### The mirror pass

`Mirror.Once` gains a second phase, after the insert phase it already has:

```text
inserts (unchanged)
then, for each tombstone with id > del_cursor, oldest first:
    does recall_fts hold a row for (kind, ref_id)?
      yes -> the key was re-indexed; the tombstone is stale, skip it
      no  -> target.Delete(ctx, kind, ref_id)
    advance del_cursor only after the target accepted
then sweep tombstones older than tombstoneTTL
```

Inserts run first on purpose. A row read into an in-flight batch can be deleted
in SQLite before that batch lands, which creates the object after its tombstone
was written; draining tombstones afterwards catches it on the same pass instead
of leaving a stale object until the next tick.

The guard is what makes a delete-then-recreate safe. A fact deleted and written
again under the same name occupies the same object id, so a delete applied
after the re-insert would remove a live vector. Seeing a row for the key means
the re-index already happened, and the insert phase will overwrite the object
in its own time, so the tombstone has nothing left to do.

`tombstoneTTL` is seven days, a package constant in `mirror`.

### Backends

`recall.Recall` gains one method:

```go
// Delete removes one indexed chunk. It is not an error for the chunk to be
// absent: the mirror can hold a tombstone for a row whose insert never
// reached the backend, and a delete that can never succeed would stall the
// cursor behind it forever.
Delete(ctx context.Context, kind, refID string) error
```

- **weaviate** — `Data().Deleter().WithClassName(Collection).WithID(objectID(
  kind, refID))`, errors wrapped through the existing `tidy()`. A 404 is
  success, per the contract above.
- **sqlitefts** — `DELETE FROM recall_fts WHERE kind = ? AND ref_id = ?`. It is
  never a mirror target, because the store owns writes to `recall_fts`, but the
  method is real rather than a stub so the interface does not lie.
- **Fallback** — forwards to the primary, for the same reason `Index` does: the
  secondary is the keyword index and is already correct by the time this runs.
- The three test fakes that implement `recall.Recall`
  (`internal/recall/fallback_test.go`, `internal/recall/mirror/mirror_test.go`,
  `internal/tool/mem/recall_test.go`) gain the method.

### `recall reindex`

`cmd/spore/recall.go` already calls `DropAll` and `Mirror.Reset`. `Reset`
additionally zeroes `del_cursor` and clears `recall_tombstones`.

## 4. Failure semantics

The posture `Mirror.Run` already states — "the vector store being down is a
degraded state to sit in, not a reason to stop the daemon" — extends unchanged.

- A failed `Delete` leaves `del_cursor` where it was, logs at warn, and retries
  on the next tick.
- A tombstone that fails permanently blocks the ones behind it. Accepted: it is
  the head-of-line behaviour insert batches already have, and `recall reindex`
  is the escape hatch.
- The sweep can drop a tombstone a long-offline backend never applied, which
  loses that deletion. Also accepted, and documented rather than engineered
  around; `recall reindex` is again the escape hatch. Seven days is far longer
  than a sidecar is plausibly down.

## 5. Testing

- **Store.** A tombstone is written by `UnindexFact`; one per fact by
  `ClearFactIndex`; **none** by `IndexFact`'s replace path; one by the
  `messages` delete trigger. Every assertion reads `recall_tombstones`
  contents, not just a nil error.
- **Mirror.** Against a fake target that records calls: a delete is applied; a
  stale tombstone is skipped when the key is back in `recall_fts`; a target
  error leaves `del_cursor` unmoved and the next pass retries; inserts are
  ordered before deletes; the sweep removes aged rows and spares fresh ones.
- **weaviate.** Object-id derivation for a delete, and a 404 from an `httptest`
  server treated as success.
- **Integration** (`-tags weaviate`, `make test-weaviate`): index a fact,
  search finds it, delete the fact, run `mirror.Once`, search no longer finds
  it. This is the test that proves the bug closed; the others prove the parts.

A green suite is not on its own evidence that a store write happened — the
SA4006 regression in `internal/policy/guard.go` passed a full suite while an
approval write was missing. The store assertions above are written against that
failure mode.

## 6. Out of scope

- A real session-deletion path. The triggers will write tombstones, but
  nothing deletes a session today and this change does not add that.
- Reporting pending deletions in `spore recall status`.
- Any change to how inserts are mirrored.
