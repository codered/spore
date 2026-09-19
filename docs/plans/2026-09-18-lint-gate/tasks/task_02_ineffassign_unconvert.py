"""Task 02 -- clear ineffassign and unconvert, then enable them on the gate.

Two findings, both small and both in code the compiler is happy with, which
is the point: they are the cheapest proof that a linter can be added to the
live gate without the gate going red.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/trace", "cmd/spore", ".golangci.yml"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)"]


def apply():
    raise ManualTask(
        "Fix two findings and enable their linters. Steps are in "
        "plan_superpowers.md, Task 02:\n"
        "  internal/trace/trace_test.go -- ineffectual assignment to ctx\n"
        "  cmd/spore/approve.go         -- unnecessary conversion\n"
        "Then add `- ineffassign` and `- unconvert` to .golangci.yml.\n"
        "The gate below is runnable now and will tell you when you are done."
    )


def verify():
    gate.structural(
        "both linters are on the gate",
        lambda: taskkit.file_contains(".golangci.yml", "    - ineffassign")
        and taskkit.file_contains(".golangci.yml", "    - unconvert"),
    )
    gate.component("the suite still passes", ["make", "test"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
