"""Task 05 -- clear unparam, then enable it on the gate.

unparam reports signatures that promise more than they deliver: a returned
error that is always nil, a parameter that only ever receives one value.
Two are in production code and are the ones worth acting on.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/agent", "cmd/spore", ".golangci.yml"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)"]


def apply():
    raise ManualTask(
        "Clear the unparam findings in non-test code (test helpers are "
        "already excluded by the Task 04 block), then add `- unparam` to "
        "'.golangci.yml'. The two sites are (*Agent).runTools in "
        "internal/agent/agent.go, whose error result is always nil, and "
        "(*chatUI).nextApproval in cmd/spore/tui.go, whose Cmd result is "
        "always nil. Steps are in plan_superpowers.md, Task 05. If either "
        "signature is the way it is because a caller will need it, say so in "
        "the report and add the exclusion instead of changing the signature."
    )


def verify():
    gate.structural(
        "unparam is on the gate",
        lambda: taskkit.file_contains(".golangci.yml", "    - unparam"),
    )
    gate.component("the suite still passes", ["make", "test"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
