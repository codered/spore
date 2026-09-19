"""Task 08 -- the final gate, plus the report the work is handed back with.

Every requirement in the brief has a line here. A requirement without a gate
line is how a plan quietly drops one, so this list is the brief.
"""
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import ManualTask, gate

KIND = "manual"
TIER = "T2"
TOUCHES = ["whole repo"]
CROSSES = ["source -> golangci-lint", "source -> govulncheck", "make targets -> CI"]

PLAN = "docs/plans/2026-09-18-lint-gate"
REPORT = PLAN + "/REPORT.md"

LINTERS = [
    "errcheck",
    "gosec",
    "govet",
    "ineffassign",
    "staticcheck",
    "unconvert",
    "unparam",
    "unused",
]

COMMITS = [
    "ci: baseline the tree for the lint gate",
    "ci: wire the lint gate end to end",
    "fix: clear ineffassign and unconvert",
    "fix: clear unused and staticcheck",
    "fix: clear errcheck",
    "fix: clear unparam",
    "fix: put gosec on the lint gate",
    "ci: wire the dependency gate and clear its findings",
]

SECTIONS = [
    "## Result",
    "## What changed per task",
    "## Findings I judged rather than silenced",
    "## Deviations",
    "## Commands and their output",
]


def _git(args):
    proc = subprocess.run(["git"] + args, capture_output=True, text=True)
    return proc.stdout.strip() if proc.returncode == 0 else ""


def _all_linters_enabled():
    return all(taskkit.file_contains(".golangci.yml", "    - " + name) for name in LINTERS)


def _one_commit_per_task():
    """The eight task commits are present, in order, on top of master."""
    log = _git(["log", "--format=%s", "master..HEAD"]).splitlines()
    return [s for s in reversed(log) if s in COMMITS] == COMMITS


def _report_is_complete():
    if not os.path.exists(REPORT):
        return False
    with open(REPORT, encoding="utf-8") as fh:
        text = fh.read()
    return all(s in text for s in SECTIONS) and "make lint" in text


def apply():
    raise ManualTask(
        "Write " + REPORT + " from the template in plan_superpowers.md, "
        "Task 08. Paste command output verbatim, including anything that was "
        "red before it was green. List every deviation from the plan, or "
        "write 'none'."
    )


def verify():
    gate.structural(
        "the work is on a branch, not on master",
        lambda: _git(["rev-parse", "--abbrev-ref", "HEAD"]) != "master",
    )
    gate.structural("all eight linters are on the gate", _all_linters_enabled)
    gate.structural(
        "the lint config is v2 and carries the sqlite_fts5 build tag",
        lambda: taskkit.file_contains(".golangci.yml", 'version: "2"')
        and taskkit.file_contains(".golangci.yml", "- sqlite_fts5"),
    )
    gate.structural(
        "both tools are pinned, neither is latest",
        lambda: taskkit.file_contains("Makefile", "GOLANGCI_VERSION := v2.13.2")
        and taskkit.file_contains("Makefile", "GOVULNCHECK_VERSION := v1.8.0")
        and not taskkit.file_contains(".github/workflows/ci.yml", "@latest")
        and not taskkit.file_contains(".github/workflows/ci.yml", "version: latest"),
    )
    gate.structural(
        "CI runs the same targets a developer runs",
        lambda: taskkit.file_contains(".github/workflows/ci.yml", "make lint-install && make lint")
        and taskkit.file_contains(".github/workflows/ci.yml", "make vulncheck-install && make vulncheck")
        and taskkit.file_contains(".github/workflows/ci.yml", "make tidycheck"),
    )
    gate.structural(
        "no golangci-lint action is left behind",
        lambda: not taskkit.file_contains(".github/workflows/ci.yml", "golangci-lint-action"),
    )
    gate.structural(
        "the tests the coverage push added are still in the tree",
        lambda: taskkit.file_contains("cmd/spore/client_test.go", "func Test")
        and taskkit.file_contains("internal/tool/subagent/tools_test.go", "TestToolSchemasAreValidJSON"),
    )
    gate.structural("one commit per task, in order", _one_commit_per_task)
    gate.structural("the report is written and complete", _report_is_complete)
    gate.component("gofmt is clean", ["make", "fmtcheck"])
    gate.component("go vet is clean", ["make", "vet"])
    gate.component("the full suite passes", ["make", "test"])
    gate.component("go.mod and go.sum are tidy", ["make", "tidycheck"])
    gate.crossing("the lint gate runs green end to end", cmd=["make", "lint"])
    gate.crossing("the dependency gate runs green end to end", cmd=["make", "vulncheck"])


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
