#!/usr/bin/env python3
"""Final gate: one PASS/FAIL line per requirement, then ALL CHECKS PASS or a count.

    python3 docs/plans/2026-09-22-tui-views-2a1/final_gate.py

Run from the repository root after all five task commits. Exit 0 only when
every line passes.
"""
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import plan_lib
from plan_lib import BASE, COMMITS, PLAN_DIR

results = []


def check(name, fn):
    try:
        ok = bool(fn())
    except Exception as exc:  # a crashed check is a failed check
        ok = False
        name += " (%s: %s)" % (type(exc).__name__, exc)
    results.append(ok)
    print(("PASS  " if ok else "FAIL  ") + name)


def sh(*args):
    return subprocess.run(list(args), capture_output=True, text=True)


def has(path, text):
    with open(path, encoding="utf-8") as fh:
        return text in fh.read()


def unchanged(path):
    return sh("git", "diff", "--quiet", BASE, "HEAD", "--", path).returncode == 0


def task_passes(n):
    script = [p for p in os.listdir(os.path.join(HERE, "tasks")) if p.startswith("task_%s_" % n)][0]
    return sh(sys.executable, os.path.join(HERE, "tasks", script), "--verify-only").returncode == 0


def commits():
    """(subject, full message, sorted files) for every commit after BASE, oldest first."""
    out = []
    for c in sh("git", "rev-list", "--reverse", BASE + "..HEAD").stdout.split():
        msg = sh("git", "log", "-1", "--format=%B", c).stdout
        files = sorted(sh("git", "show", "--name-only", "--format=", c).stdout.split())
        out.append((msg.split("\n")[0], msg, files))
    return out


# --- the brief: what 2a-1 delivers --------------------------------------------
check("R1  the daemon serves GET /api/usage (route at its call site)",
      lambda: has("internal/daemon/server.go", 'mux.HandleFunc("GET /api/usage", s.handleUsage)'))
check("R2  the skills endpoint returns bodies only for ?body=1",
      lambda: has("internal/daemon/skills.go", 'withBody := r.URL.Query().Get("body") == "1"'))
check("R3  cmd/spore's adapter implements tui.Views",
      lambda: has("cmd/spore/tui_backend.go", "var _ tui.Views = tuiBackend{}"))
check("R4  tui.New finds the views through the backend it is given",
      lambda: has("internal/tui/app.go", "if v, ok := be.(Views); ok {"))
check("R5  tui.Run's signature is unchanged", lambda: unchanged("internal/tui/run.go"))
check("R6  four views: skills S, agents A, usage U, jobs J",
      lambda: has("internal/tui/res_registry.go", "return []Resource{skillsRes{}, agentsRes{}, usageRes{}, jobsRes{}}"))
check("R7  hotkeys in NORMAL open views",
      lambda: has("internal/tui/app.go", "if r, ok := resourceByHotkey(k.String()); ok {"))
check("R8  the stop-agent action exists", lambda: has("internal/tui/res_agents.go", 'Label: "stop",'))
check("R9  the cancel-job action exists", lambda: has("internal/tui/res_jobs.go", 'Label: "cancel",'))
check("R10 an open view refreshes every 2 seconds", lambda: has("internal/tui/app.go", "refreshEvery = 2 * time.Second"))
check("R11 the k9s header and title rule are on every screen",
      lambda: has("internal/tui/view.go", "m.headerView(), m.ruleView(), body, m.statusView())"))
check("R12 /usage /skills /agents typed into the input still print text",
      lambda: has("internal/tui/app.go", 'case "clear", "compact", "context", "usage", "skills", "agents":\n\t\treturn m.slash(fields[0])'))
check("R13 usage session totals are rows labelled 'this session'",
      lambda: has("internal/tui/res_usage.go", 'usageCells("this session", r)'))
# --- global constraints ------------------------------------------------------------
check("C1  no new module dependencies: go.mod and go.sum unchanged",
      lambda: unchanged("go.mod") and unchanged("go.sum"))
check("C2  internal/tui never imports cmd/spore",
      lambda: "cmd/spore" not in sh("go", "list", "-tags", "sqlite_fts5", "-deps", "./internal/tui/").stdout)
check("C3  wire constants in internal/daemon/event.go unchanged", lambda: unchanged("internal/daemon/event.go"))
check("C4  on branch tui-views", lambda: sh("git", "rev-parse", "--abbrev-ref", "HEAD").stdout.strip() == plan_lib.BRANCH)
check("C5  no tracked file is modified or staged",
      lambda: sh("git", "status", "--porcelain", "--untracked-files=no").stdout.strip() == "")
cs = commits()
check("C6  exactly six commits after %s: the plan, then tasks 01-05" % BASE, lambda: len(cs) == 6)
check("C7  the first commit after %s changes only the plan directory" % BASE,
      lambda: all(f.startswith(PLAN_DIR + "/") for f in cs[0][2]))
for i, n in enumerate(sorted(COMMITS), start=1):
    subject, files = COMMITS[n]
    check("C8.%s commit %s has its exact message and trailers" % (n, n),
          lambda i=i, n=n: cs[i][1].rstrip("\n") == plan_lib.message(n).rstrip("\n"))
    check("C9.%s commit %s changes exactly its %d files" % (n, n, len(files)),
          lambda i=i, files=files: cs[i][2] == sorted(files))
check("C10 the user's .agents/ and fix_task1.py were never committed",
      lambda: not any(f.startswith(plan_lib.USER_FILES) for c in cs for f in c[2]))
check("C11 the branch changes nothing outside the plan's file map",
      lambda: set(sh("git", "diff", "--name-only", BASE, "HEAD").stdout.split())
      <= {f for _, fs in COMMITS.values() for f in fs} | set(cs[0][2]))
# --- every task's own gate, at HEAD -----------------------------------------------
for n in sorted(COMMITS):
    check("G%s task %s's gates pass (--verify-only)" % (n, n), lambda n=n: task_passes(n))
check("G06 make fmtcheck vet lint test tidycheck vulncheck and go test -race, at HEAD in a detached worktree",
      lambda: task_passes("06"))

failed = results.count(False)
print("ALL CHECKS PASS" if failed == 0 else "%d CHECK(S) FAILED" % failed)
sys.exit(0 if failed == 0 else 1)
