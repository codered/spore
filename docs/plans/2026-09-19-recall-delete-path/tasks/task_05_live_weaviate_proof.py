"""Task 05 — Prove it against a real Weaviate, and close the backlog entry."""
import shutil
import subprocess
import sys
import os

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/recall/weaviate", "internal/recall/mirror", "docs"]
CROSSES = ["mirror -> live weaviate HTTP"]


def live_round_trip():
    """Run the tagged suite against a real Weaviate and require the named test.

    This gate needs Docker and a Weaviate the compose file can start, which is
    the machine this repository is developed on. Where Docker is absent the
    gate fails rather than skipping: a delete path that has never run against a
    real vector store is exactly the thing the backlog entry complained about.
    """
    if shutil.which("docker") is None:
        print("GATE NEEDS DOCKER: run this task on the development machine, "
              "after `spore recall setup`.")
        return False
    proc = subprocess.run(["make", "test-weaviate"], capture_output=True, text=True)
    out = (proc.stdout or "") + (proc.stderr or "")
    if proc.returncode != 0:
        print(out[-2000:])
        return False
    return "--- PASS: TestDeletedFactLeavesNoVector" in out


def apply():
    raise ManualTask(
        "Add TestDeletedFactLeavesNoVector to the weaviate integration suite and\n"
        "rewrite the backlog entry as closed.\n"
        "Steps are in plan_superpowers.md, Task 05."
    )


def verify():
    gate.structural(
        "the integration test exists under the weaviate tag",
        lambda: taskkit.file_contains(
            "internal/recall/weaviate/integration_test.go", "TestDeletedFactLeavesNoVector"
        ),
    )
    gate.structural(
        "the backlog entry is closed",
        lambda: taskkit.file_contains("docs/backlog.md", "Deleting a fact leaves its vector behind")
        and taskkit.file_contains("docs/backlog.md", "Closed. The mirror now carries deletions"),
    )
    gate.component(
        "the tagged suite still compiles",
        ["go", "vet", "-tags", "sqlite_fts5 weaviate", "./internal/recall/..."],
    )
    gate.crossing(
        "a deleted fact is gone from a real Weaviate after one mirror pass",
        check=live_round_trip,
    )


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
