#!/usr/bin/env python3
"""Scenario C: work that happens while you are away (spore).

The user asks, in conversation, for a one-off job two minutes ahead, approves
the schedule as they would, and leaves: the harness sends nothing more. After
the job's time it checks, through the daemon's API, that the job ran, that
its answer is right (bench.py's T1 key: the three largest Go files under
internal/), and what it cost, setup and run together. Anything the job had
to ask for while nobody was there is recorded too.

pi and opencode have no scheduler out of the box (NA). Prime Agent schedules
through resident workers (`prime-agent schedule add`); it is reported
separately in the README.

Usage: scenario_c.py <first run> <last run>   appends to results-c.jsonl
"""
import datetime, json, os, shutil, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "agents"))
import bench  # noqa: E402
from scenario_b import Spore  # noqa: E402  (daemon, API, approvals)

OUT = os.environ.get("SCEN_OUT") or os.path.join(HERE, "results-c.jsonl")
LEAD = 120  # seconds between asking and the job's time
WAIT = 600  # how long after that to look for the run


def calls_of(messages):
    out = []
    for m in messages:
        if m.get("role") in ("assistant", "note") and (m.get("tokens_in") or m.get("tokens_out")):
            out.append({"in": m.get("tokens_in") or 0, "out": m.get("tokens_out") or 0,
                        "cache_read": m.get("tokens_cache_read") or 0, "cache_write": m.get("tokens_cache_write") or 0})
    return out


def final_text(messages):
    text = ""
    for m in messages:
        if m.get("role") == "assistant":
            t = "".join(b.get("text", "") for b in m.get("blocks") or [] if b.get("type") == "text")
            if t.strip():
                text = t
    return text


def one(run):
    rd = tempfile.mkdtemp(prefix="sscen-c.", dir="/tmp")
    try:
        shutil.copytree(bench.WS, rd + "/ws")
        sp = Spore(rd)
        try:
            sp.new_session()
            when = (datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(seconds=LEAD)).replace(microsecond=0)
            ask = (f"Schedule a one-off job for {when.isoformat().replace('+00:00', 'Z')}: list the three Go files "
                   "under internal/ in this project that have the most lines, with each path and its exact line count.")
            sp.turn(ask)
            setup_approvals = list(sp.approved)
            setup = calls_of(sp.req("GET", f"/api/sessions/{sp.sid}")["messages"])
            # The user is away: nothing is sent or answered from here on.
            job, runs = None, []
            deadline = time.time() + LEAD + WAIT
            while time.time() < deadline:
                jobs = sp.req("GET", "/api/jobs") or []
                if jobs:
                    job = jobs[0]
                    runs = sp.req("GET", f"/api/jobs/{job['id']}/runs") or []
                    if runs and runs[0].get("status") not in ("running", "pending", "", None):
                        break
                time.sleep(10)
            ran, text, run_calls, waiting = False, "", [], 0
            if runs:
                ran = True
                sid = (runs[0].get("session") or {}).get("id")
                if sid:
                    d = sp.req("GET", f"/api/sessions/{sid}")
                    text, run_calls = final_text(d["messages"]), calls_of(d["messages"])
                    waiting = len(sp.req("GET", f"/api/sessions/{sid}/approvals") or [])
                text = text or runs[0].get("output") or ""
        finally:
            sp.close()
        correct = bench.grade("T1", text) if text else False
        rec = {"tool": "spore", "run": run, "job_created": job is not None, "ran": ran, "correct": correct,
               "setup_approvals": setup_approvals, "approvals_waiting_in_job": waiting,
               "run_state": runs[0].get("status") if runs else None,
               "setup_cost": round(bench.cost(setup), 5), "job_cost": round(bench.cost(run_calls), 5),
               "cost": round(bench.cost(setup + run_calls), 5), "answer": text[-800:]}
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"spore run{run} created={rec['job_created']} ran={ran} correct={correct} state={rec['run_state']} "
              f"approvals={setup_approvals} waiting={waiting} ${rec['cost']:.4f}", flush=True)
    finally:
        shutil.rmtree(rd, ignore_errors=True)


def main():
    first, last = int(sys.argv[1]), int(sys.argv[2])
    bench.WS = bench.checkout()
    try:
        for run in range(first, last + 1):
            one(run)
    finally:
        shutil.rmtree(bench.WS, ignore_errors=True)


if __name__ == "__main__":
    main()
