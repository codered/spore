#!/usr/bin/env python3
"""Commit one finished task: exactly its files, with exactly its message.

    python3 docs/plans/2026-09-22-tui-views-2a1/commit.py 01

Refuses (exit 1, nothing staged) when the branch is wrong or when the changed
files differ from the task's list. Run it from the repository root.
"""
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import plan_lib


def git(*args, stdin=None):
    return subprocess.run(["git"] + list(args), capture_output=True, text=True, input=stdin)


def changed():
    """Tracked changes plus untracked files, minus the user's own files."""
    out = git("status", "--porcelain", "--untracked-files=all").stdout
    paths = set()
    for line in out.splitlines():
        path = line[3:]
        if not path.startswith(plan_lib.USER_FILES):
            paths.add(path)
    return paths


def main(argv):
    if len(argv) != 1 or argv[0] not in plan_lib.COMMITS:
        print("usage: commit.py NN, where NN is one of " + ", ".join(plan_lib.COMMITS))
        return 1
    task = argv[0]
    subject, files = plan_lib.COMMITS[task]
    branch = git("rev-parse", "--abbrev-ref", "HEAD").stdout.strip()
    if branch != plan_lib.BRANCH:
        print("REFUSED: on branch %r, want %r" % (branch, plan_lib.BRANCH))
        return 1
    have, want = changed(), set(files)
    if have != want:
        print("REFUSED: the changed files differ from task %s's list" % task)
        for p in sorted(have - want):
            print("  unexpected: " + p)
        for p in sorted(want - have):
            print("  missing:    " + p)
        return 1
    add = git("add", "--", *files)
    if add.returncode != 0:
        print(add.stderr)
        return 1
    com = git("commit", "-q", "-F", "-", stdin=plan_lib.message(task))
    if com.returncode != 0:
        print(com.stdout + com.stderr)
        return 1
    print("committed task %s: %s" % (task, subject))
    for p in files:
        print("  " + p)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
