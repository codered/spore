#!/usr/bin/env python3
"""Scenario B: memory across sessions.

Session 1 is a short working session in which the user states four things
that cannot be guessed from the repository. At its end each tool's own
review runs where it has one (spore /refine, Prime Agent /refine). Session 2
is a NEW session in the same workspace, later: it asks for work that should
apply three of those things, and asks the fourth back.

  B1  the commit message starts with [policy], and the push goes to the
      run's fork remote, onto a branch under the run's release prefix (3 facts)
  B2  the run's staging host and port                                  (1 fact)

The remote, release prefix, host and port are random per run.

Each tool runs in its default configuration (see bench/agents/bench.py for
the shared controls). Where spore asks to save something the user asked it
to keep (memory, agent_note), the harness answers as that user would, allow
once, and counts it.

Usage: scenario_b.py <first run> <last run> [tool,tool,...]   appends to results-b.jsonl
"""
import json, os, re, shutil, subprocess, sys, tempfile, time, urllib.request, uuid

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(os.path.dirname(HERE), "agents"))
import bench  # noqa: E402  (parsers, prices, checkout)

OUT = os.environ.get("SCEN_OUT") or os.path.join(HERE, "results-b.jsonl")
MODEL = bench.MODEL
PORT = 7795

# The facts are drawn fresh for every run, so no file anywhere holds the
# answer: a tool gets one right only by remembering this run's session 1.
def facts():
    tag = uuid.uuid4().hex[:6]
    return {"branch": f"ship-{tag}", "remote": f"fork{tag[:3]}", "host": f"staging-{tag}.internal",
            "port": str(2000 + int(tag[:3], 16) % 8000)}


def session_1(f):
    return [
        f"Some context for this project before we start: we release from {f['branch']}/* branches, never from main. Keep that in mind.",
        "Also, commit messages here start with the package name in square brackets, like `[router] add a route`.",
        f"And I push to my fork, the remote called `{f['remote']}`, not `origin`.",
        f"For later: the staging server for this project is {f['host']}, on port {f['port']}.",
        "What does internal/router do, in two sentences?",
    ]


B1 = ("I've finished a change to internal/policy that adds a 'word matches' rule predicate. "
      "Write the commit message, and give me the exact git push command, branch included.")
B2 = "What's the staging server for this project again, host and port?"


def grade_b1(t, f):
    t = bench.norm(t)
    return {
        "prefix": re.search(r"\[policy\]", t) is not None,
        # Both values are random per run, so naming one at all is recall.
        # Requiring it inside the push command would mark down an answer
        # that knows the release prefix but will not invent a branch name.
        "remote": re.search(r"\b" + re.escape(f["remote"]) + r"\b", t) is not None,
        "release_branch": f["branch"] in t,
    }


def grade_b2(t, f):
    t = bench.norm(t)
    return {"staging": f["host"] in t and re.search(r"\b" + f["port"] + r"\b", t) is not None}


# ---- spore: through the daemon's HTTP API, the one its TUI and web UI use ----

class Spore:
    name = "spore"

    def __init__(self, rd):
        self.rd, self.base, self.approvals, self.proc = rd, f"http://127.0.0.1:{PORT}", 0, None
        self.approved = []
        with open(rd + "/config.toml", "w") as f:
            f.write(f'''default_model = "anthropic/{MODEL}"
data_dir = "{rd}/data"
[providers.anthropic]
kind = "anthropic"
api_key = "${{ANTHROPIC_API_KEY}}"
price_in = 2.0
price_out = 10.0
[daemon]
addr = "127.0.0.1:{PORT}"
[policy]
workspace = "{rd}"
''')  # shipped defaults apart from the ceiling, which must contain the /tmp run dir
        self.start()

    def start(self):
        self.proc = subprocess.Popen([bench.SPORE, "-config", self.rd + "/config.toml", "serve"], cwd=self.rd + "/ws",
                                     stdin=subprocess.DEVNULL, stdout=open(self.rd + "/serve.log", "a"), stderr=subprocess.STDOUT)
        for _ in range(100):
            try:
                urllib.request.urlopen(self.base + "/healthz", timeout=1)
                return
            except OSError:
                time.sleep(0.1)

    def stop(self):
        if self.proc:
            self.proc.terminate()
            self.proc.wait(timeout=15)
            self.proc = None

    def req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(self.base + path, data=data, method=method, headers={"Content-Type": "application/json"})
        raw = urllib.request.urlopen(r, timeout=120).read()
        return json.loads(raw) if raw.strip() else {}

    def new_session(self):
        self.sid = self.req("POST", "/api/sessions", {"workspace": self.rd + "/ws"})["id"]

    def usage_since(self, before):
        d = self.req("GET", f"/api/sessions/{self.sid}")
        calls, text = [], ""
        for m in d["messages"][before:]:
            if m.get("role") not in ("assistant", "note"):
                continue
            if m.get("tokens_in") or m.get("tokens_out"):
                calls.append({"in": m.get("tokens_in") or 0, "out": m.get("tokens_out") or 0,
                              "cache_read": m.get("tokens_cache_read") or 0, "cache_write": m.get("tokens_cache_write") or 0})
            if m.get("role") == "assistant":
                t = "".join(b.get("text", "") for b in m.get("blocks") or [] if b.get("type") == "text")
                if t.strip():
                    text = t
        return calls, text

    def turn(self, q):
        before = len(self.req("GET", f"/api/sessions/{self.sid}")["messages"])
        self.req("POST", f"/api/sessions/{self.sid}/messages", {"text": q})
        t0 = time.monotonic()
        while time.monotonic() - t0 < bench.TIMEOUT:
            for a in self.req("GET", f"/api/sessions/{self.sid}/approvals") or []:
                # The user asked for this to be kept; they would say yes.
                self.req("POST", f"/api/sessions/{self.sid}/approvals/{a['pending_id']}", {"allow": True, "scope": "once"})
                self.approvals += 1
                self.approved.append(a.get("tool"))
            d = self.req("GET", f"/api/sessions/{self.sid}")
            if not d.get("running") and len(d["messages"]) > before + 1:
                break
            time.sleep(0.25)
        return self.usage_since(before)

    def refine(self):
        before = len(self.req("GET", f"/api/sessions/{self.sid}")["messages"])
        self.req("POST", f"/api/sessions/{self.sid}/refine", {})
        return self.usage_since(before)[0]

    def later(self):
        # A later day: the daemon restarts, nothing survives but what is on disk.
        self.stop()
        self.start()

    def close(self):
        self.stop()


# ---- the others: one process per turn ----

class CLI:
    def __init__(self, name, rd):
        self.name, self.rd, self.approvals, self.approved = name, rd, 0, []
        os.makedirs(rd + "/sessions", exist_ok=True)

    def new_session(self):
        self.first, self.oc_session = True, None

    def run(self, q):
        if self.name in ("pi", "prime"):
            exe = "pi" if self.name == "pi" else bench.PRIME
            c = [exe, "-p", "--mode", "json", "--session-dir", self.rd + "/sessions", "-ne", "-ns", "-np", "-nc"]
            if self.name == "prime":
                c.append("--offline")
            if not self.first:
                c.append("-c")
            c += ["--provider", "anthropic", "--model", MODEL, q]
            env = None
        else:
            _, env, _ = bench.run_opencode(self.rd, q)
            c = ["opencode", "run", "--pure", "--format", "json", "--model", "anthropic/" + MODEL]
            if self.oc_session:
                c += ["--session", self.oc_session]
            c.append(q)
        out = self.rd + "/turn.out"
        with open(out, "w") as fo, open(self.rd + "/turn.err", "a") as fe:
            subprocess.run(c, cwd=self.rd + "/ws", env=dict(os.environ, **(env or {})), stdin=subprocess.DEVNULL,
                           stdout=fo, stderr=fe, timeout=bench.TIMEOUT)
        self.first = False
        if self.name == "opencode":
            for e in bench.jsonl(out):
                sid = e.get("sessionID") or e.get("part", {}).get("sessionID")
                if sid and not self.oc_session:
                    self.oc_session = sid
            return bench.run_opencode(self.rd, q)[2](out)
        return bench.piish(bench.jsonl(out))

    def turn(self, q):
        return self.run(q)

    def refine(self):
        # Prime Agent's own review, as the user would type it. pi and opencode
        # have none.
        if self.name != "prime":
            return None
        return self.run("/refine")[0]

    def later(self):
        pass

    def close(self):
        pass


def one(tool, run):
    rd = tempfile.mkdtemp(prefix="sscen-b.", dir="/tmp")
    try:
        shutil.copytree(bench.WS, rd + "/ws")
        agent = Spore(rd) if tool == "spore" else CLI(tool, rd)
        try:
            f = facts()
            agent.new_session()
            s1 = []
            for q in session_1(f):
                calls, _ = agent.turn(q)
                s1 += calls
            r = agent.refine()
            review = "none" if r is None else "ran"
            s1 += r or []
            agent.later()
            agent.new_session()
            c1, t1 = agent.turn(B1)
            c2, t2 = agent.turn(B2)
        finally:
            agent.close()
        g = {**grade_b1(t1, f), **grade_b2(t2, f)}
        rec = {"tool": tool, "run": run, "review": review, "approvals": agent.approvals, "approved_tools": agent.approved,
               "facts": g, "facts_applied": sum(g.values()),
               "session1_cost": round(bench.cost(s1), 5), "session2_cost": round(bench.cost(c1 + c2), 5),
               "cost": round(bench.cost(s1 + c1 + c2), 5), "b1_answer": t1[-800:], "b2_answer": t2[-400:]}
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{tool:9} run{run} facts={rec['facts_applied']}/4 {g} approvals={agent.approvals} "
              f"review={review} ${rec['cost']:.4f}", flush=True)
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
                one(tool, run)
            order = order[1:] + order[:1]
    finally:
        shutil.rmtree(bench.WS, ignore_errors=True)


if __name__ == "__main__":
    main()
