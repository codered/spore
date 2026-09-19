"""Task 06 -- put gosec on the gate, with every remaining finding justified.

spore launches subprocesses and reads paths the user names: that is the
product, not a defect, so most gosec hits here are answered with a written
justification rather than a code change. A `//nolint` with no reason is not
an answer, and the gate checks for one on every line that carries a nolint.
"""
import os
import re
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["internal/tool/fs", "internal/skill", "internal/daemon", "internal/subagent", "cmd/spore"]
CROSSES = ["source -> golangci-lint (via .golangci.yml)"]

NOLINT = re.compile(r"//\s*nolint:([\w,]+)\s*//\s*\S")


def _every_nolint_is_justified():
    """Every `//nolint:` in Go source carries `// <reason>` after it."""
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "docs", "web", "assets")]
        for name in files:
            if not name.endswith(".go"):
                continue
            path = os.path.join(root, name)
            with open(path, encoding="utf-8") as fh:
                for line in fh:
                    if "//nolint" in line.replace(" ", "") and not NOLINT.search(line):
                        return False
    return True


def apply():
    raise ManualTask(
        "Fix the gosec findings that are real, justify the rest, then add "
        "`- gosec` to .golangci.yml. Two are fixed rather than justified: "
        "G118 in internal/subagent/supervisor.go, which uses "
        "context.Background() where a request-scoped context is in hand, and "
        "the 0755/0644 permission sites, which become 0750/0600 unless a "
        "test proves they cannot. Everything else takes a nolint with a "
        "written reason. Steps are in plan_superpowers.md, Task 06."
    )


def verify():
    gate.structural(
        "gosec is on the gate",
        lambda: taskkit.file_contains(".golangci.yml", "    - gosec"),
    )
    gate.structural("every nolint states a reason", _every_nolint_is_justified)
    gate.component("the suite still passes", ["make", "test"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
