"""Task 03 — Every genuine removal writes a tombstone: ClearFactIndex and the two triggers."""
import subprocess
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/store", "internal/recall/mirror"]
CROSSES = ["store -> mirror (recall_tombstones)"]

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
        "Make ClearFactIndex and the messages/summaries delete triggers write\n"
        "tombstones. Steps are in plan_superpowers.md, Task 03. Tests first."
    )


def verify():
    gate.structural(
        "the messages trigger writes a tombstone before it deletes",
        lambda: taskkit.file_contains("internal/store/schema.go", "recall_fts_messages_ad")
        and taskkit.file_contains("internal/store/schema.go", "INSERT INTO recall_tombstones"),
    )
    gate.component(
        "store tests pass",
        ["go", "test", "-tags", TAGS, "-count=1", "./internal/store/..."],
    )
    gate.crossing(
        "a wiped fact index and a deleted message both leave tombstones",
        check=lambda: go_test_passes(
            "./internal/store/",
            "TestClearFactIndexWritesTombstones",
            "TestMessageDeleteTriggerWritesTombstone",
            "TestSummaryDeleteTriggerWritesTombstone",
        ),
    )
    gate.crossing(
        "a deleted message's vector is removed through the mirror",
        check=lambda: go_test_passes("./internal/recall/mirror/", "TestMessageDeletePropagates"),
    )


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
