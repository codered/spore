#!/usr/bin/env python3
"""Run one task and print its result as fixed lines a worker can compare.

    python3 docs/plans/2026-09-22-tui-views-2a1/step.py 01          # apply + verify
    python3 docs/plans/2026-09-22-tui-views-2a1/step.py 01 --red    # tests only; they must fail

Apply prints "exit N", then one "<tier> PASS|FAIL <gate>" line per gate. On
any failure it then prints "--- details" and what went wrong. Run it from the
repository root.
"""
import glob
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
SHOWN = ("RED as expected", "NOT RED", "WARN", "DRIFT", "MANUAL", "GATE FAILED")


def main(argv):
    if not argv:
        print(__doc__)
        return 1
    found = glob.glob(os.path.join(HERE, "tasks", "task_%s_*.py" % argv[0]))
    if len(found) != 1:
        print("no single task %s in %s/tasks" % (argv[0], HERE))
        return 1
    env = dict(os.environ, PYTHONDONTWRITEBYTECODE="1")
    if "--red" in argv:
        proc = subprocess.run([sys.executable, found[0], "--red"], capture_output=True, text=True, env=env)
        print(proc.stdout.rstrip())
        if proc.returncode != 0:
            print("--- details")
            print(proc.stderr.rstrip()[-4000:])
        return proc.returncode
    proc = subprocess.run([sys.executable, found[0]], capture_output=True, text=True, env=env)
    for line in proc.stderr.splitlines():
        if line.startswith(("RED as expected",)):
            print(line)
    report = {}
    for line in reversed(proc.stdout.splitlines()):
        try:
            report = json.loads(line)
            break
        except ValueError:
            continue
    print("exit %d" % proc.returncode)
    for t in report.get("tiers", []):
        print("%s %s %s" % (t["tier"], "PASS" if t["ok"] else "FAIL", t["desc"]))
    if report.get("backend") == "fallback":
        print("WARN backend: fallback — " + "; ".join(report.get("warnings", [])))
    if proc.returncode != 0:
        print("--- details")
        for t in report.get("tiers", []):
            if not t["ok"]:
                print(t.get("detail", "")[-4000:])
        print("\n".join(l for l in proc.stderr.splitlines() if l.startswith(SHOWN)))
        print(proc.stderr.rstrip()[-4000:])
    return proc.returncode


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
