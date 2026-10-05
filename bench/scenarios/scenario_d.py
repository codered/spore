#!/usr/bin/env python3
"""Scenario D: frontier money only for the conversation.

The session benchmark's ten turns (bench/agents/session.py) plus a /refine at
the end, on spore three ways:

  D0  everything on Claude Sonnet 5.5 (defaults)
  D1  titles, compaction, refinement and sub-agent turns routed to a local
      gpt-oss-20b with [[route]]; everything else default
  D2  D1 plus [context] max_tokens = 24000, so compaction runs on the local
      model during the session; turns 9 and 10 depend on early turns, which
      tests whether compaction kept them

Local tokens cost $0. The other agents have no per-call-site routing (NA);
their cost for the same ten turns is in bench/agents/sessions-2026-10-05.jsonl.

Usage: LOCAL_BASE_URL=... LOCAL_MODEL=... scenario_d.py <first run> <last run> [D0,D1,D2]
"""
import json, os, shutil, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "agents"))
import bench  # noqa: E402
import session  # noqa: E402  (TURNS)
from scenario_b import Spore  # noqa: E402

OUT = os.environ.get("SCEN_OUT") or os.path.join(HERE, "results-d.jsonl")
LOCAL_BASE_URL = os.environ.get("LOCAL_BASE_URL", "http://10.10.30.15:8888/v1")
LOCAL_MODEL = os.environ.get("LOCAL_MODEL", "unsloth/gpt-oss-20b-GGUF")
METER_PORT = 8898  # meter.py: every Anthropic call, metered from the API's own usage


class RoutedSpore(Spore):
    def __init__(self, rd, variant):
        self.variant = variant
        super().__init__(rd)

    def start(self):
        cfg = self.rd + "/config.toml"
        body = open(cfg).read()
        if "base_url" not in body.split("[policy]")[0]:
            # spore's own records leave out title and compaction calls, so
            # every Anthropic call goes through the meter instead.
            body = body.replace('kind = "anthropic"\n', f'kind = "anthropic"\nbase_url = "http://127.0.0.1:{METER_PORT}"\n', 1)
            open(cfg, "w").write(body)
        if self.variant != "D0" and "[[route]]" not in open(cfg).read():
            with open(cfg) as f:
                body = f.read()
            # Top-level keys must precede the first table, so routes and the
            # local provider are added as tables at the end.
            body += f'''
[providers.local]
kind = "openai"
base_url = "{LOCAL_BASE_URL}"

[[route]]
when = "title|compaction|refinement|subagent"
model = "local/{LOCAL_MODEL}"
'''
            if self.variant == "D2":
                body += "\n[context]\nmax_tokens = 24000\n"
            with open(cfg, "w") as f:
                f.write(body)
        super().start()

def one(variant, run):
    rd = tempfile.mkdtemp(prefix="sscen-d.", dir="/tmp")
    try:
        shutil.copytree(bench.WS, rd + "/ws")
        meter_log = rd + "/meter.jsonl"
        meter = subprocess.Popen([sys.executable, os.path.join(HERE, "meter.py"), str(METER_PORT), meter_log])
        time.sleep(0.5)
        sp = RoutedSpore(rd, variant)
        try:
            sp.new_session()
            correct, turns = 0, []
            for i, (q, ok) in enumerate(session.TURNS, 1):
                _, text = sp.turn(q)
                good = bool(ok(bench.norm(text)))
                correct += good
                turns.append({"turn": i, "correct": good})
            sp.refine()
            compacted = (sp.req("GET", f"/api/sessions/{sp.sid}").get("summary_through") or 0) > 0
            local = sum(1 for m in sp.req("GET", f"/api/sessions/{sp.sid}")["messages"]
                        if m.get("role") in ("assistant", "note") and m.get("model") and "claude" not in m["model"])
        finally:
            sp.close()
            meter.terminate()
        sonnet = []
        for line in open(meter_log):
            u = json.loads(line)["usage"]
            sonnet.append({"in": u.get("input_tokens", 0), "out": u.get("output_tokens", 0),
                           "cache_write": u.get("cache_creation_input_tokens", 0),
                           "cache_read": u.get("cache_read_input_tokens", 0)})
        rec = {"variant": variant, "run": run, "correct": correct, "turns": turns,
               "late_turns_correct": sum(t["correct"] for t in turns if t["turn"] >= 9),
               "cost": round(bench.cost(sonnet), 5), "sonnet_calls": len(sonnet),
               "local_calls_in_transcript": local, "compacted": compacted, "approvals": sp.approvals}
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{variant} run{run} ${rec['cost']:.4f} correct={correct}/10 late={rec['late_turns_correct']}/2 "
              f"sonnet_calls={len(sonnet)} local={local} compacted={compacted}", flush=True)
    finally:
        shutil.rmtree(rd, ignore_errors=True)


def main():
    first, last = int(sys.argv[1]), int(sys.argv[2])
    variants = sys.argv[3].split(",") if len(sys.argv) > 3 else ["D0", "D1", "D2"]
    bench.WS = bench.checkout()
    try:
        for run in range(first, last + 1):
            for v in variants:
                one(v, run)
    finally:
        shutil.rmtree(bench.WS, ignore_errors=True)


if __name__ == "__main__":
    main()
