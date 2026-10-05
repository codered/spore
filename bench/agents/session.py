#!/usr/bin/env python3
"""Session benchmark: ten questions in one session, per tool.

bench.py measures one-question sessions, where every tool pays to write its
whole prompt into the cache and reads it back only a few times. This one
measures a working session: the same workspace and the same session for ten
turns, each tool continuing its session the way a user would. Turns build on
each other, one depends on what was said earlier, and the last asks for a
recap.

Same controls as bench.py: model claude-sonnet-5-5 with each tool's default
thinking, personal extensions/skills/context files off, a fresh copy of the
repo at WORKSPACE_REF per session, stdin closed, cost from tokens at one
price table. spore is driven through its HTTP API, the one its TUI and web UI
use; the others continue their session from the command line.

Usage: session.py <first run> <last run> [tool,tool,...]   appends to sessions.jsonl
"""
import json, os, re, shutil, subprocess, sys, tempfile, time, urllib.request

import bench  # the per-tool parsers, prices and checkout

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "sessions.jsonl")
MODEL = bench.MODEL
PORT = 7793

# (question, grader over the normalised answer)
TURNS = [
    ("What address does the spore daemon listen on by default? Answer in one line.",
     lambda t: "127.0.0.1:7777" in t),
    ("Which three Go files under internal/ have the most lines? Give each path and exact line count.",
     lambda t: bench.near(t, "tui/app.go", "1528") and bench.near(t, "store/store.go", "1088") and bench.near(t, "store/store_test.go", "1057")),
    ("Of those three files, which are test files?",
     lambda t: "store_test.go" in t),
    ("What settings does the [subagents] config section accept, and what is each default?",
     lambda t: bench.near(t, "max_depth", "2") and bench.near(t, "max_cost_usd", r"1(?:\.0+)?") and bench.near(t, "max_concurrent", "4")),
    ("What is the default approval_timeout?",
     lambda t: re.search(r"\b5m\b|\b5 ?minutes?\b|five minutes", t, re.I) is not None),
    ("Which directory directly under internal/ has the most Go test functions (names starting with Test, in _test.go files, subdirectories included), and how many?",
     lambda t: bench.near(t, "tui", "156")),
    ("What is the default max_output for a tool result, in bytes?",
     lambda t: re.search(r"\b30000\b|\b30 ?000\b|30_000", t) is not None),
    ("What is the default kernel mode, and what are the two modes?",
     lambda t: re.search(r"\bcode\b", t) is not None and re.search(r"\btools\b", t) is not None),
    ("What was the first question I asked in this session, and what was your answer?",
     lambda t: "7777" in t),
    ("Recap: list every answer you gave in this session, one line each.",
     lambda t: all(x in t for x in ("7777", "1528", "156"))),
]


def norm(t):
    return bench.norm(t)


# ---- spore: through the daemon's HTTP API ----

class Spore:
    def __init__(self, rd):
        self.rd = rd
        self.base = f"http://127.0.0.1:{PORT}"
        with open(rd + "/config.toml", "w") as f:
            f.write(f'''default_model = "anthropic/{MODEL}"
data_dir      = "{rd}/data"
[providers.anthropic]
kind      = "anthropic"
api_key   = "${{ANTHROPIC_API_KEY}}"
price_in  = 2.0
price_out = 10.0
[daemon]
addr = "127.0.0.1:{PORT}"
[policy]
workspace = "{rd}/ws"
default   = "allow"
ask       = []
''')
        self.proc = subprocess.Popen([bench.SPORE, "-config", rd + "/config.toml", "serve"], cwd=rd + "/ws",
                                     stdin=subprocess.DEVNULL, stdout=open(rd + "/serve.log", "w"), stderr=subprocess.STDOUT)
        for _ in range(100):
            try:
                urllib.request.urlopen(self.base + "/healthz", timeout=1)
                break
            except OSError:
                time.sleep(0.1)
        self.sid = self.req("POST", "/api/sessions", {"workspace": rd + "/ws"})["id"]
        self.seen = 0

    def req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        raw = urllib.request.urlopen(r, timeout=30).read()
        return json.loads(raw) if raw.strip() else {}

    def turn(self, q):
        before = len(self.req("GET", f"/api/sessions/{self.sid}")["messages"])
        self.req("POST", f"/api/sessions/{self.sid}/messages", {"text": q})
        t0 = time.monotonic()
        while time.monotonic() - t0 < bench.TIMEOUT:
            d = self.req("GET", f"/api/sessions/{self.sid}")
            if not d.get("running") and len(d["messages"]) > before + 1:
                break
            time.sleep(0.25)
        calls, text = [], ""
        for m in d["messages"][before:]:
            if m.get("role") != "assistant":
                continue
            if m.get("tokens_in") or m.get("tokens_out"):
                calls.append({"in": m.get("tokens_in") or 0, "out": m.get("tokens_out") or 0,
                              "cache_read": m.get("tokens_cache_read") or 0, "cache_write": m.get("tokens_cache_write") or 0})
            t = "".join(b.get("text", "") for b in m.get("blocks") or [] if b.get("type") == "text")
            if t.strip():
                text = t
        return calls, text

    def close(self):
        self.proc.terminate()
        self.proc.wait(timeout=10)


# ---- the others: one process per turn, continuing the session ----

class CLI:
    def __init__(self, tool, rd):
        self.tool, self.rd, self.first, self.oc_session = tool, rd, True, None
        os.makedirs(rd + "/sessions", exist_ok=True)

    def cmd(self, q):
        if self.tool in ("pi", "prime"):
            exe = "pi" if self.tool == "pi" else bench.PRIME
            c = [exe, "-p", "--mode", "json", "--session-dir", self.rd + "/sessions", "-ne", "-ns", "-np", "-nc"]
            if self.tool == "prime":
                c.append("--offline")
            if not self.first:
                c.append("-c")
            return c + ["--provider", "anthropic", "--model", MODEL, q], None
        # opencode: same isolated config as bench.py, continuing by session id
        _, env, _ = bench.run_opencode(self.rd, q)
        c = ["opencode", "run", "--pure", "--format", "json", "--model", "anthropic/" + MODEL]
        if self.oc_session:
            c += ["--session", self.oc_session]
        return c + [q], env

    def turn(self, q):
        c, extra = self.cmd(q)
        out = self.rd + "/turn.out"
        with open(out, "w") as fo, open(self.rd + "/turn.err", "a") as fe:
            subprocess.run(c, cwd=self.rd + "/ws", env=dict(os.environ, **(extra or {})), stdin=subprocess.DEVNULL,
                           stdout=fo, stderr=fe, timeout=bench.TIMEOUT)
        self.first = False
        if self.tool == "opencode":
            events = bench.jsonl(out)
            for e in events:
                sid = e.get("sessionID") or e.get("part", {}).get("sessionID")
                if sid and not self.oc_session:
                    self.oc_session = sid
            return bench.run_opencode(self.rd, q)[2](out)
        return bench.piish(bench.jsonl(out))

    def close(self):
        pass


def session(tool, run):
    rd = tempfile.mkdtemp(prefix="ssess.", dir="/tmp")
    try:
        shutil.copytree(bench.WS, rd + "/ws")
        agent = Spore(rd) if tool == "spore" else CLI(tool, rd)
        turns = []
        try:
            limit = int(os.environ.get("SESSION_TURNS", len(TURNS)))  # a smoke test runs fewer
            for i, (q, ok) in enumerate(TURNS[:limit], 1):
                t0 = time.monotonic()
                calls, text = agent.turn(q)
                wall = time.monotonic() - t0
                rec = {"turn": i, "wall_s": round(wall, 2), "calls": len(calls),
                       **{k: sum(c[k] for c in calls) for k in ("in", "cache_write", "cache_read", "out")},
                       "cost_usd": round(bench.cost(calls), 5), "correct": bool(ok(norm(text))), "answer": text[-800:]}
                turns.append(rec)
                print(f"{tool:9} run{run} turn{i:2} {wall:6.1f}s calls={rec['calls']:2} ${rec['cost_usd']:.4f} correct={rec['correct']}", flush=True)
        finally:
            agent.close()
        out = {"tool": tool, "run": run, "turns": turns,
               "cost_usd": round(sum(t["cost_usd"] for t in turns), 5),
               "wall_s": round(sum(t["wall_s"] for t in turns), 1),
               "correct": sum(t["correct"] for t in turns)}
        with open(OUT, "a") as f:
            f.write(json.dumps(out) + "\n")
        print(f"== {tool} run{run}: ${out['cost_usd']:.3f}  {out['wall_s']}s  {out['correct']}/{len(turns)} correct", flush=True)
    finally:
        shutil.rmtree(rd, ignore_errors=True)


def main():
    first, last = int(sys.argv[1]), int(sys.argv[2])
    only = sys.argv[3].split(",") if len(sys.argv) > 3 else ["spore", "opencode", "pi", "prime"]
    bench.WS = bench.checkout()
    try:
        order = list(only)
        for run in range(first, last + 1):
            for tool in order:
                session(tool, run)
            order = order[1:] + order[:1]
    finally:
        shutil.rmtree(bench.WS, ignore_errors=True)


if __name__ == "__main__":
    main()
