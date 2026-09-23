"""Helpers shared by this plan's task scripts. Stdlib only.

Go source is embedded in the task scripts with four-space indentation, so the
Python stays readable; go() turns each leading group of four spaces into a tab,
which is exactly gofmt's indentation. Alignment spaces inside a line are kept.
"""
from __future__ import annotations

import os
import subprocess
import sys

import taskkit
from taskkit import Drift, Editor

TAGS = ["-tags", "sqlite_fts5"]


class One(Editor):
    """An Editor that commits at most one op.

    hashline 0.9.1 misplaces an insert that follows an earlier multi-line
    insert in the same patch (it is shifted up by the earlier insert's length;
    observed while building this plan). One op per session re-reads the file
    between edits, so every anchor is fresh and nothing is renumbered.
    """

    def commit(self):
        if len(self._ops) > 1:
            raise RuntimeError("%s: %d ops in one session; use one One() per op" % (self.path, len(self._ops)))
        super().commit()


def go(src: str) -> str:
    """Four-space indented Go -> tab-indented Go, ending in exactly one newline."""
    out = []
    for line in src.strip("\n").split("\n"):
        n = len(line) - len(line.lstrip(" "))
        out.append("\t" * (n // 4) + " " * (n % 4) + line[n:])
    return "\n".join(out) + "\n"


def lines(src: str) -> list[str]:
    """go(src) as a list of lines, for Editor ops."""
    return go(src).rstrip("\n").split("\n")


def offset(ed: Editor, anchor, k: int, expected: str):
    """The anchor k lines after `anchor`, which must read `expected`."""
    idx = anchor.n - 1 + k
    if idx < 0 or idx >= len(ed._lines) or ed._lines[idx].content != expected:
        got = ed._lines[idx].content if 0 <= idx < len(ed._lines) else "<EOF>"
        raise Drift("%s:%d: expected %r, found %r" % (ed.path, anchor.n + k, expected, got))
    return ed._lines[idx]


def append_go(path: str, src: str, marker: str) -> None:
    """Append Go code after the file's last line, one blank line between.
    Skipped when `marker` is already in the file."""
    if taskkit.file_contains(path, marker):
        return
    with One(path) as ed:
        ed.insert_after(ed._lines[-1], [""] + lines(src))


def todo(path: str, marker: str) -> bool:
    """True while `marker` is absent from path: the edit it marks has not landed."""
    return not taskkit.file_contains(path, marker)


def gotest(*pkgs: str, run: str | None = None, race: bool = True, count: int | None = None) -> list[str]:
    cmd = ["go", "test"] + TAGS
    if race:
        cmd.append("-race")
    if count is not None:
        cmd.append("-count=%d" % count)
    if run:
        cmd += ["-run", run]
    return cmd + list(pkgs)


def gofmt_clean(*paths: str) -> bool:
    proc = subprocess.run(["gofmt", "-l"] + list(paths), capture_output=True, text=True)
    return proc.returncode == 0 and proc.stdout.strip() == ""


def vet_ok(*pkgs: str) -> bool:
    return subprocess.run(["go", "vet"] + TAGS + list(pkgs), capture_output=True).returncode == 0


# A red run that shows any of these failed for a reason other than the one
# the check names: the new code is wrong, not merely unimplemented.
UNINTENDED = ["redeclared", "syntax error", "imported and not used", "declared and not used",
              "cannot use", "mismatched types", "too many errors"]


def red(checks) -> int:
    """Red before green. Each check is (description, cmd, [substrings]).

    The command must FAIL, its output must contain every substring, and it
    must contain none of UNINTENDED. Prints one fixed line per check, so a
    worker can compare the output exactly.
    """
    code = 0
    for desc, cmd, needles in checks:
        if cmd[:2] == ["go", "test"]:
            cmd = cmd[:2] + ["-gcflags=-e"] + cmd[2:]  # every compile error, not the first ten
        proc = subprocess.run(cmd, capture_output=True, text=True)
        out = proc.stdout + proc.stderr
        missing = [n for n in needles if n not in out]
        missing += ["no " + u for u in UNINTENDED if u in out]
        if proc.returncode != 0 and not missing:
            print("RED as expected: " + desc)
        else:
            code = 1
            print("NOT RED: %s (exit %d, missing %r)" % (desc, proc.returncode, missing))
            print(out[-3000:], file=sys.stderr)
    return code


def golden(name: str) -> list[str]:
    path = os.path.join("internal/tui/testdata", name + ".golden")
    with open(path, encoding="utf-8") as fh:
        return fh.read().split("\n")


def cells(s: str) -> int:
    """Display width, the way ansi.StringWidth counts it for this UI's glyphs."""
    import unicodedata
    return sum(2 if unicodedata.east_asian_width(c) in "WF" else 1 for c in s)


def golden_fits(name: str, width: int, height: int) -> bool:
    """No line wider than the screen, and exactly one line per screen row."""
    rows = golden(name)
    return len(rows) == height and all(cells(r) <= width for r in rows)


# ------------------------------------------------------------ commits --
# One commit per task: the exact message and the exact files. commit.py and
# final_gate.py both read this table, so the plan cannot disagree with itself.

BASE = "174b2c1"  # the source plan's commit; the plan directory is committed on top of it
PLAN_DIR = "docs/plans/2026-09-22-tui-views-2a1"
BRANCH = "tui-views"
TRAILER = ("Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>\n"
           "Claude-Session: https://claude.ai/code/session_01JQTKYKdzDqhTRTMqvtqJxU")

COMMITS = {
    "01": ("daemon: GET /api/usage and skill bodies; the adapter serves the views over them", [
        "cmd/spore/tui_backend.go",
        "cmd/spore/tui_e2e_test.go",
        "internal/daemon/server.go",
        "internal/daemon/skills.go",
        "internal/daemon/skills_test.go",
        "internal/daemon/usage.go",
        "internal/daemon/usage_test.go",
        "internal/store/usage.go",
        "internal/store/usage_test.go",
        "internal/tui/views.go",
    ]),
    "02": ("tui: the resource framework and the table every view runs in", [
        "internal/tui/resource.go",
        "internal/tui/table.go",
        "internal/tui/table_test.go",
    ]),
    "03": ("tui: skills, agents, usage and jobs resources", [
        "internal/tui/app_test.go",
        "internal/tui/block_test.go",
        "internal/tui/res_agents.go",
        "internal/tui/res_jobs.go",
        "internal/tui/res_registry.go",
        "internal/tui/res_skills.go",
        "internal/tui/res_test.go",
        "internal/tui/res_usage.go",
    ]),
    "04": ("tui: a k9s header with key hints, a title rule, and a status bar of keys", [
        "internal/tui/header.go",
        "internal/tui/testdata/approval-insert-100.golden",
        "internal/tui/testdata/approval-normal-100.golden",
        "internal/tui/testdata/screen-100.golden",
        "internal/tui/testdata/screen-160.golden",
        "internal/tui/testdata/screen-60.golden",
        "internal/tui/testdata/sessions-all-100.golden",
        "internal/tui/testdata/tool-expanded-100.golden",
        "internal/tui/view.go",
    ]),
    "05": ("tui: open views by hotkey or :command, refresh them, act on rows; an end-to-end views test", [
        "cmd/spore/tui_e2e_test.go",
        "internal/tui/app.go",
        "internal/tui/app_test.go",
        "internal/tui/header.go",
        "internal/tui/testdata/view-jobs-confirm-100.golden",
        "internal/tui/testdata/view-skills-100.golden",
        "internal/tui/testdata/view-skills-detail-100.golden",
        "internal/tui/testdata/view-stale-100.golden",
        "internal/tui/testdata/view-usage-100.golden",
        "internal/tui/view.go",
        "internal/tui/view_test.go",
    ]),
}

# The user's own untracked files. They are never staged and never committed.
USER_FILES = (".agents/", "fix_task1.py")


def message(task: str) -> str:
    return COMMITS[task][0] + "\n\n" + TRAILER + "\n"
