"""Task 06 — every gate CI runs, at HEAD, in a detached worktree.

Source plan: Task 7 Step 1. Nothing to apply: the gate runs on the committed
branch, never on the working tree, so an uncommitted file cannot make it pass.
Run it only after Task 05 is committed.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import subprocess

from taskkit import gate

import plan_lib

KIND = "scripted"
TIER = "T1"
TOUCHES = ["whole repository"]
CROSSES = []

# A fresh detached worktree at HEAD, removed whatever happens.
AT_HEAD = """
set -e
d=$(mktemp -d)
git worktree add -q --detach "$d" HEAD
trap 'git worktree remove --force "$d"' EXIT
cd "$d"
%s
"""


def at_head(script):
    return ["bash", "-c", AT_HEAD % script]


def apply():
    pass  # verify-only: the branch is already built by Tasks 01-05


def tasks_committed():
    subjects = subprocess.run(["git", "log", "--format=%s", plan_lib.BASE + "..HEAD"],
                              capture_output=True, text=True).stdout.splitlines()
    return all(subject in subjects for subject, _ in plan_lib.COMMITS.values())


def verify():
    gate.structural("tasks 01-05 are committed on this branch", tasks_committed)
    gate.component("make fmtcheck vet lint", at_head("make fmtcheck vet lint"))
    gate.component("make test", at_head("make test"))
    gate.component("go test -race ./... (what CI runs)", at_head("go test -tags sqlite_fts5 -race -timeout 10m ./..."))
    gate.component("make tidycheck vulncheck", at_head("make tidycheck vulncheck"))


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
