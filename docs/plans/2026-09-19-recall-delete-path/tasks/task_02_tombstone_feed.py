"""Task 02 — A deleted fact reaches the mirror: tombstone table, cursor, delete phase."""
import subprocess
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/store", "internal/recall/mirror"]
CROSSES = ["store -> mirror (recall_tombstones, recall_sync.del_cursor)"]

TAGS = "sqlite_fts5"


def go_test_passes(pkg, *names):
    pattern = "^(" + "|".join(names) + ")$"
    proc = subprocess.run(
        ["go", "test", "-tags", TAGS, "-count=1", "-v", "-run", pattern, pkg],
        capture_output=True, text=True,
    )
    out = (proc.stdout or "") + (proc.stderr or "")
    if proc.returncode != 0:
        return False
    return all(("--- PASS: " + n) in out for n in names)


def apply():
    raise ManualTask(
        "Add recall_tombstones, the del_cursor migration, the tombstone write in\n"
        "UnindexFact, the store feed methods, and the mirror's delete phase.\n"
        "Steps are in plan_superpowers.md, Task 02. Write the tests first."
    )


def verify():
    gate.structural(
        "recall_tombstones is in the schema with AUTOINCREMENT",
        lambda: taskkit.file_contains("internal/store/schema.go", "recall_tombstones")
        and taskkit.file_contains("internal/store/schema.go", "INTEGER PRIMARY KEY AUTOINCREMENT"),
    )
    gate.structural(
        "recall_sync gains del_cursor through a migration, not a schema edit",
        lambda: taskkit.file_contains("internal/store/store.go", "migrateRecallSync")
        or taskkit.file_contains("internal/store/recall.go", "migrateRecallSync")
        or taskkit.file_contains("internal/store/tombstone.go", "migrateRecallSync"),
    )
    gate.structural(
        "the mirror drains tombstones",
        lambda: taskkit.file_contains("internal/recall/mirror/mirror.go", "TombstonesSince")
        or taskkit.file_contains("internal/recall/mirror/delete.go", "TombstonesSince"),
    )
    gate.component(
        "store tests pass, including the tombstone writes",
        ["go", "test", "-tags", TAGS, "-count=1", "./internal/store/..."],
    )
    gate.crossing(
        "UnindexFact writes a tombstone and none is written by IndexFact's replace",
        check=lambda: go_test_passes(
            "./internal/store/",
            "TestUnindexFactWritesTombstone",
            "TestIndexFactWritesNoTombstone",
            "TestRecallSyncGainsDelCursor",
        ),
    )
    gate.crossing(
        "a deleted fact reaches the backend as a Delete, and a re-created one does not",
        check=lambda: go_test_passes(
            "./internal/recall/mirror/",
            "TestDeletePropagatesToTarget",
            "TestStaleTombstoneSkipped",
            "TestDeleteFailureKeepsCursor",
            "TestInsertsRunBeforeDeletes",
        ),
    )


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
