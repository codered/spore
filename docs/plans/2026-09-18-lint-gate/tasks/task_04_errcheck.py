"""Task 04 -- clear errcheck, then enable it on the gate.

The largest group: unchecked error returns, mostly `Close`, `Fprintf` and
`os.Remove`. Production code states the decision (`_ =` or a handled error);
test code is excluded by path, because a test that ignores a teardown error
is not a defect.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["cmd/spore", "internal/config", "internal/memory", "internal/provider", "internal/recall", "internal/trace"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)"]


def apply():
    raise ManualTask(
        "Clear the errcheck findings in non-test code, add the _test.go "
        "exclusion block, then add `- errcheck` to .golangci.yml. The four "
        "rewrite shapes, copied exactly, are in plan_superpowers.md, Task 04. "
        "Two sites drop an error that should be reported rather than "
        "discarded: internal/config/write.go and internal/memory/memory.go "
        "both ignore `tmp.Close()` on the write path of an atomic replace. "
        "Handle those; note them in the report."
    )


def verify():
    gate.structural(
        "errcheck is on the gate",
        lambda: taskkit.file_contains(".golangci.yml", "    - errcheck"),
    )
    gate.structural(
        "tests are excluded by path, not by blanket nolint",
        lambda: taskkit.file_contains(".golangci.yml", "exclusions:")
        and taskkit.file_contains(".golangci.yml", "path: _test\\.go"),
    )
    gate.component("the suite still passes", ["make", "test"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
