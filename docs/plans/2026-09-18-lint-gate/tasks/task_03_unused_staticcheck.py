"""Task 03 -- clear unused and staticcheck, then enable them on the gate.

One of these is not cosmetic. staticcheck reports SA4006 in
internal/policy/guard.go: a value assigned to `claimed` is never read. In a
policy guard that is either dead code or a dropped check, so it is looked at
before it is silenced.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/policy", "internal/subagent", "internal/recall", "internal/store", "cmd/spore"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)"]


def apply():
    raise ManualTask(
        "Clear 3 unused and 7 staticcheck findings, then add `- staticcheck` "
        "and `- unused` to .golangci.yml. Steps, and the rule for which "
        "findings are fixed rather than silenced, are in plan_superpowers.md, "
        "Task 03. Read internal/policy/guard.go (SA4006) before touching it: "
        "report what the dropped value means, do not delete it to make the "
        "gate green."
    )


def verify():
    gate.structural(
        "both linters are on the gate",
        lambda: taskkit.file_contains(".golangci.yml", "    - staticcheck")
        and taskkit.file_contains(".golangci.yml", "    - unused"),
    )
    gate.structural(
        "the SA4006 site carries a written decision, not a silent deletion",
        lambda: taskkit.file_contains(
            "docs/plans/2026-09-18-lint-gate/decisions.md", "guard.go"
        ),
    )
    gate.component("the suite still passes", ["make", "test"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
