"""Task 04 — Bound the table: the seven-day sweep, and reindex clearing the feed."""
import subprocess
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/store", "internal/recall/mirror", "cmd/spore"]
CROSSES = ["store -> mirror (recall_tombstones)", "cmd/spore recall reindex -> mirror"]

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
        "Add the tombstoneTTL sweep to the mirror pass and make Mirror.Reset\n"
        "clear the tombstone table and del_cursor.\n"
        "Steps are in plan_superpowers.md, Task 04. Tests first."
    )


def verify():
    gate.structural(
        "the mirror owns a tombstone TTL",
        lambda: taskkit.file_contains("internal/recall/mirror/mirror.go", "tombstoneTTL")
        or taskkit.file_contains("internal/recall/mirror/delete.go", "tombstoneTTL"),
    )
    gate.component(
        "the whole default suite still passes",
        ["go", "test", "-tags", TAGS, "-count=1", "./..."],
    )
    gate.crossing(
        "aged tombstones are swept, fresh ones are spared, and reindex clears the feed",
        check=lambda: go_test_passes(
            "./internal/recall/mirror/",
            "TestSweepDropsAgedTombstones",
            "TestSweepSparesFreshTombstones",
            "TestResetClearsTombstonesAndDelCursor",
        ),
    )


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
