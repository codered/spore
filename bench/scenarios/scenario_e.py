#!/usr/bin/env python3
"""Scenario E: delegation that keeps the main context small.

One session per tool: an investigation across four packages, asked for with
sub-agents, then five factual follow-ups in the same session. Measured:

  parent_ctx    input tokens of the first follow-up call: how much of the
                investigation the main context carried forward
  followup_cost what the five follow-ups cost
  cost          the whole session, sub-agents included
  correct       follow-ups answered right, out of 5

Variants: spore (sub-agents on), spore-nosub (agent_* denied: the same task
done in one context), opencode and Prime Agent (both have sub-agents), pi (no
sub-agents: NA for delegation, its one-context cost still measured).

Usage: scenario_e.py <first run> <last run> [variant,...]   appends to results-e.jsonl
"""
import json, os, re, shutil, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "agents"))
import bench  # noqa: E402
from scenario_b import Spore, CLI  # noqa: E402

OUT = os.environ.get("SCEN_OUT") or os.path.join(HERE, "results-e.jsonl")

TASK = ("Use sub-agents for this: have one sub-agent read each of internal/policy, internal/kernel, "
        "internal/recall and internal/refine and report how that package handles a failure. Then give me "
        "a one-paragraph comparison and recommend one pattern for the whole codebase.")
FOLLOWUPS = [
    ("How many idle minutes before refinement runs on its own, by default?", lambda t: re.search(r"\b10\b", t)),
    ("Which recall backends does spore have?", lambda t: re.search(r"sqlite ?fts|fts5", t, re.I) and re.search(r"weaviate", t, re.I)),
    ("Which function in internal/kernel starts the go_run child process?", lambda t: "startChild" in t),
    ("Refinement edits from which kind of session are applied immediately rather than proposed?",
     lambda t: re.search(r"\bchat\b", t, re.I)),
    ("What is the default approval_timeout?", lambda t: re.search(r"\b5m\b|\b5 ?minutes?\b|five minutes", t, re.I)),
]


def inp(c):
    return c["in"] + c["cache_write"] + c["cache_read"]


class SporeTree(Spore):
    """Spore with the cost of every session in the tree, children included."""

    def __init__(self, rd, nosub):
        self.nosub = nosub
        super().__init__(rd)

    def start(self):
        if self.nosub and "agent_*" not in open(self.rd + "/config.toml").read():
            # Writing any policy list replaces all the defaults, so the
            # shipped allow and ask lists are written out with the deny.
            with open(self.rd + "/config.toml", "a") as f:
                f.write('allow = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*", "schedule_list", "job_output", '
                        '"schedule_notify", "recall_search", "skill_load", "refine"]\n'
                        'ask = ["fs_write", "fs_edit", "shell_exec", "schedule_create", "schedule_cancel", "mcp__*", '
                        '"memory", "skill_install", "agent_note"]\n'
                        'deny = ["agent_*"]\n')
        super().start()

    def tree_calls(self):
        calls = []
        for s in self.req("GET", "/api/sessions?children=1") or []:
            d = self.req("GET", f"/api/sessions/{s['id']}")
            for m in d["messages"]:
                if m.get("role") in ("assistant", "note") and (m.get("tokens_in") or m.get("tokens_out")):
                    calls.append({"in": m.get("tokens_in") or 0, "out": m.get("tokens_out") or 0,
                                  "cache_read": m.get("tokens_cache_read") or 0,
                                  "cache_write": m.get("tokens_cache_write") or 0})
        return calls


def opencode_total(rd):
    """opencode's own total across every session it ran, sub-agents included."""
    _, env, _ = bench.run_opencode(rd, "")
    out = subprocess.run(["opencode", "stats"], cwd=rd + "/ws", env=dict(os.environ, **env),
                         capture_output=True, text=True, timeout=60).stdout
    return out


def one(variant, run):
    rd = tempfile.mkdtemp(prefix="sscen-e.", dir="/tmp")
    try:
        shutil.copytree(bench.WS, rd + "/ws")
        tool = "spore" if variant.startswith("spore") else variant
        agent = SporeTree(rd, variant == "spore-nosub") if tool == "spore" else CLI(tool, rd)
        notes = {}
        try:
            agent.new_session()
            t0 = time.monotonic()
            task_calls, _ = agent.turn(TASK)
            follow_calls, correct, first_follow = [], 0, None
            for q, ok in FOLLOWUPS:
                calls, text = agent.turn(q)
                if first_follow is None and calls:
                    first_follow = inp(calls[0])
                follow_calls += calls
                correct += bool(ok(bench.norm(text)))
            wall = time.monotonic() - t0
            if tool == "spore":
                total = agent.tree_calls()
                notes["subagent_sessions"] = len(agent.req("GET", "/api/sessions?children=1") or []) - 1
            else:
                total = task_calls + follow_calls
            if tool == "opencode":
                notes["opencode_stats"] = opencode_total(rd)[-1500:]
        finally:
            agent.close()
        rec = {"variant": variant, "run": run, "parent_ctx": first_follow,
               "task_cost": round(bench.cost(task_calls), 5), "followup_cost": round(bench.cost(follow_calls), 5),
               "cost": round(bench.cost(total), 5), "correct": correct, "wall_s": round(wall, 1),
               "approvals": agent.approvals, **notes}
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{variant:12} run{run} parent_ctx={first_follow} followups=${rec['followup_cost']:.4f} "
              f"total=${rec['cost']:.4f} correct={correct}/5 {notes.get('subagent_sessions', '')}", flush=True)
    finally:
        shutil.rmtree(rd, ignore_errors=True)


def main():
    first, last = int(sys.argv[1]), int(sys.argv[2])
    variants = sys.argv[3].split(",") if len(sys.argv) > 3 else ["spore", "spore-nosub", "opencode", "pi", "prime"]
    bench.WS = bench.checkout()
    try:
        order = list(variants)
        for run in range(first, last + 1):
            for v in order:
                one(v, run)
            order = order[1:] + order[:1]
    finally:
        shutil.rmtree(bench.WS, ignore_errors=True)


if __name__ == "__main__":
    main()
