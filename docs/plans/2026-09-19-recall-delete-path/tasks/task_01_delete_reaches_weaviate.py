"""Task 01 — Delete crosses the recall interface to Weaviate over HTTP."""
import subprocess
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/recall", "internal/recall/weaviate", "internal/recall/sqlitefts", "internal/tool/mem"]
CROSSES = ["mirror -> weaviate HTTP"]

TAGS = "sqlite_fts5"


def go_test_passes(pkg, *names):
    """True when `go test` exits 0 AND each named test reports --- PASS.

    The exit code alone is not enough: `-run` matching nothing exits 0, so a
    gate written on the exit code passes vacuously for a test nobody wrote.
    """
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
        "Add Delete to recall.Recall and implement it in every backend.\n"
        "Steps are in plan_superpowers.md, Task 01. Write the tests first.\n"
        "The gates below are runnable now and are the contract."
    )


def verify():
    gate.structural(
        "recall.Recall declares Delete",
        lambda: taskkit.file_contains(
            "internal/recall/recall.go", "Delete(ctx context.Context, kind, refID string) error"
        ),
    )
    gate.structural(
        "weaviate deletes by the derived object id",
        lambda: taskkit.file_contains("internal/recall/weaviate/weaviate.go", "func (b *Backend) Delete(")
        or taskkit.file_contains("internal/recall/weaviate/delete.go", "func (b *Backend) Delete("),
    )
    gate.component(
        "the recall tree builds and its own tests pass",
        ["go", "test", "-tags", TAGS, "-count=1", "./internal/recall/...", "./internal/tool/mem/..."],
    )
    gate.crossing(
        "a Delete leaves spore as a DELETE for the right object id, and a 404 is success",
        check=lambda: go_test_passes(
            "./internal/recall/weaviate/",
            "TestDeleteRemovesObjectByID",
            "TestDeleteMissingObjectIsSuccess",
        ),
    )
    gate.crossing(
        "Fallback forwards a Delete to the primary",
        check=lambda: go_test_passes("./internal/recall/", "TestFallbackDeleteUsesPrimary"),
    )


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
