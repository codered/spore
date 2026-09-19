"""Task 00 -- baseline the working tree so the lint seam can be wired cleanly.

The repository arrives with an unfinished lint attempt: a v1 `.golangci.yml`
that golangci-lint v2 refuses to parse, and a CI step that installs an
unpinned `latest`. Both are replaced by task 01, so they are taken off the
tree first and kept in `preexisting/` for reference.

The revert itself is Step 0 of plan_superpowers.md, done by the worker with
git. This script gates that it happened and adds the coverage artefacts to
.gitignore.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import Editor, gate

KIND = "scripted"
TIER = "T1"
TOUCHES = ["repo config"]
CROSSES = []

PLAN = "docs/plans/2026-09-18-lint-gate"


def apply():
    """Ignore the coverage artefacts the coverage push left in the tree."""
    if not taskkit.file_contains(".gitignore", "coverage.out"):
        with Editor(".gitignore") as ed:
            ed.insert_after(
                ed.locate("/spore-*-*"),
                ["coverage.out", "coverage_subagent.out"],
            )


def verify():
    gate.structural(
        # Absence holds only until task 01 writes the v2 config, so what is
        # gated is that the v1 file is gone -- not that no config exists.
        "the v1 .golangci.yml is off the tree",
        lambda: not os.path.exists(".golangci.yml")
        or taskkit.file_contains(".golangci.yml", 'version: "2"'),
    )
    gate.structural(
        "ci.yml is back at its committed state (no unpinned lint step)",
        lambda: not taskkit.file_contains(".github/workflows/ci.yml", "golangci-lint"),
    )
    gate.structural(
        "the replaced files are kept for reference",
        lambda: os.path.exists(PLAN + "/preexisting/golangci-v1.yml")
        and os.path.exists(PLAN + "/preexisting/ci-lint-step.patch"),
    )
    gate.structural(
        "coverage artefacts are ignored",
        lambda: taskkit.file_contains(".gitignore", "coverage.out"),
    )
    gate.component("gofmt is clean", ["make", "fmtcheck"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
