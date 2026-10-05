#!/usr/bin/env python3
"""Head-to-head: spore vs opencode vs pi vs Prime Agent on the same tasks.

Every run: a fresh copy of the workspace at /tmp/sbench.XXXX/ws, stdin closed,
model anthropic/claude-sonnet-5-5 with each tool's default thinking, the
user's extensions/skills/context files disabled. Usage is read per LLM call
from each tool's own output; cost is computed here from the tokens at one
price table, so a tool's own price catalogue cannot skew it.

The workspace is this repository at WORKSPACE_REF, so the expected answers
below stay true as the code moves on. Needs ANTHROPIC_API_KEY, a built
./spore (make build), and opencode, pi and prime-agent on PATH (or
PRIME_AGENT pointing at the binary).

Usage: bench.py <first run> <last run> [tool,tool,...]   appends to results.jsonl
       analyze.py [results file]
"""
import json, os, re, shutil, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(os.path.dirname(HERE))
WORKSPACE_REF = "ee67c8d"  # the commit the expected answers below were taken from
WS = None  # set by main: a checkout of WORKSPACE_REF
OUT = os.path.join(HERE, "results.jsonl")
SPORE = os.path.join(REPO, "spore")
PRIME = os.environ.get("PRIME_AGENT") or shutil.which("prime-agent") or os.path.expanduser("~/.local/share/prime-agent/bin/prime-agent")
MODEL = "claude-sonnet-5-5"
TIMEOUT = 600

# $ per million tokens, Claude Sonnet 5.5.
PRICE = {"in": 2.00, "cache_write": 2.50, "cache_read": 0.20, "out": 10.00}

TASKS = {
    "T1": "Which three Go files under internal/ have the most lines? Give each file's path and exact line count.",
    "T2": "Which three directories directly under internal/ contain the most Go test functions (functions whose name starts with Test, in _test.go files, including subdirectories)? Give each with its exact count.",
    "T3": "What settings does the [subagents] config section accept, and what is each one's default value?",
    "T4": "What address does the spore daemon listen on by default? Answer in one line.",
}


def norm(t):
    return t.replace(",", "").replace("`", "").replace("*", "")


def near(text, key, val, span=80):
    """key appears with val within span characters after it."""
    return re.search(re.escape(key) + r".{0,%d}?\b(?:%s)\b" % (span, val), text, re.S | re.I) is not None


def grade(task, text):
    t = norm(text)
    if task == "T1":
        return all([near(t, "tui/app.go", "1528"), near(t, "store/store.go", "1088"), near(t, "store/store_test.go", "1057")])
    if task == "T2":
        return all([near(t, "tui", "156"), near(t, "daemon", "124"), near(t, "tool", "109")])
    if task == "T3":
        return all([near(t, "max_depth", "2"), near(t, "max_cost_usd", r"1(?:\.0+)?"), near(t, "max_concurrent", "4")])
    if task == "T4":
        return "127.0.0.1:7777" in t
    raise ValueError(task)


def cost(calls):
    return sum(c["in"] * PRICE["in"] + c["cache_write"] * PRICE["cache_write"]
               + c["cache_read"] * PRICE["cache_read"] + c["out"] * PRICE["out"] for c in calls) / 1e6


def jsonl(path):
    out = []
    with open(path, errors="replace") as f:
        for line in f:
            try:
                out.append(json.loads(line))
            except ValueError:
                pass
    return out


def piish(events):
    """pi and Prime Agent share pi-mono's JSON event stream."""
    calls, text = [], ""
    for e in events:
        if e.get("type") == "message_end" and e.get("message", {}).get("role") == "assistant":
            m = e["message"]
            u = m.get("usage") or {}
            calls.append({"in": u.get("input", 0), "out": u.get("output", 0),
                          "cache_read": u.get("cacheRead", 0), "cache_write": u.get("cacheWrite", 0) + u.get("cacheWrite1h", 0)})
            t = "".join(c.get("text", "") for c in m.get("content", []) if c.get("type") == "text")
            if t.strip():
                text = t
    return calls, text


def run_pi(rd, prompt):
    cmd = ["pi", "-p", "--mode", "json", "--no-session", "-ne", "-ns", "-np", "-nc",
           "--provider", "anthropic", "--model", MODEL, prompt]
    return cmd, None, lambda out: piish(jsonl(out))


def run_prime(rd, prompt):
    cmd = [PRIME, "-p", "--mode", "json", "--no-session", "-ne", "-ns", "-np", "-nc", "--offline",
           "--provider", "anthropic", "--model", MODEL, prompt]
    return cmd, None, lambda out: piish(jsonl(out))


def run_opencode(rd, prompt):
    for d in ("cfg", "data", "cache", "state"):
        os.makedirs(os.path.join(rd, "oc-" + d), exist_ok=True)
    # Run mode answers an "ask" with a rejection that ends the run. Deny turns
    # a path outside the workspace into an error the model can recover from,
    # which is what spore's workspace ceiling does.
    os.makedirs(rd + "/oc-cfg/opencode", exist_ok=True)
    with open(rd + "/oc-cfg/opencode/opencode.json", "w") as f:
        json.dump({"$schema": "https://opencode.ai/config.json", "permission": {"external_directory": "deny"}}, f)
    env = {"XDG_CONFIG_HOME": rd + "/oc-cfg", "XDG_DATA_HOME": rd + "/oc-data",
           "XDG_CACHE_HOME": rd + "/oc-cache", "XDG_STATE_HOME": rd + "/oc-state",
           "OPENCODE_DISABLE_CLAUDE_CODE": "1"}
    cmd = ["opencode", "run", "--pure", "--format", "json", "--model", "anthropic/" + MODEL, prompt]

    def parse(out):
        calls, texts = [], []
        for e in jsonl(out):
            p = e.get("part", {})
            if e.get("type") == "step_finish":
                tk = p.get("tokens", {})
                calls.append({"in": tk.get("input", 0), "out": tk.get("output", 0) + tk.get("reasoning", 0),
                              "cache_read": tk.get("cache", {}).get("read", 0), "cache_write": tk.get("cache", {}).get("write", 0)})
            if e.get("type") == "text" and p.get("text"):
                texts.append(p["text"])
        return calls, (texts[-1] if texts else "")
    return cmd, env, parse


def run_spore(rd, prompt):
    port = 7790
    with open(rd + "/config.toml", "w") as f:
        f.write(f'''default_model = "anthropic/{MODEL}"
data_dir      = "{rd}/data"
[providers.anthropic]
kind      = "anthropic"
api_key   = "${{ANTHROPIC_API_KEY}}"
price_in  = 2.0
price_out = 10.0
[daemon]
addr = "127.0.0.1:{port}"
[policy]
workspace = "{rd}/ws"
# Run everything without asking, as the other agents do. Without ask = [],
# spore's built-in ask list (shell_exec, fs_write, ...) still applies.
default   = "allow"
ask       = []
''')
    # spore once starts the daemon itself, so its startup is in the wall time.
    cmd = [SPORE, "-config", rd + "/config.toml", "once", prompt]

    def parse(out):
        import urllib.request
        base = f"http://127.0.0.1:{port}"
        sessions = json.load(urllib.request.urlopen(base + "/api/sessions?children=1"))
        calls, text = [], ""
        for s in sessions:
            d = json.load(urllib.request.urlopen(base + "/api/sessions/" + s["id"]))
            for m in d["messages"]:
                if m.get("role") == "assistant" and (m.get("tokens_in") or m.get("tokens_out")):
                    calls.append({"in": m.get("tokens_in") or 0, "out": m.get("tokens_out") or 0,
                                  "cache_read": m.get("tokens_cache_read") or 0, "cache_write": m.get("tokens_cache_write") or 0})
            if not s.get("parent_id"):
                for m in d["messages"]:
                    if m.get("role") == "assistant":
                        t = "".join(b.get("text", "") for b in m.get("blocks") or [] if b.get("type") == "text")
                        if t.strip():
                            text = t
        return calls, text
    return cmd, None, parse


def stop_spore(rd):
    subprocess.run([SPORE, "-config", rd + "/config.toml", "serve", "--stop"],
                   stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


TOOLS = {"spore": run_spore, "opencode": run_opencode, "pi": run_pi, "prime": run_prime}


def one(tool, task, run):
    rd = tempfile.mkdtemp(prefix="sbench.", dir="/tmp")
    try:
        shutil.copytree(WS, rd + "/ws")
        cmd, extra, parse = TOOLS[tool](rd, TASKS[task])
        env = dict(os.environ, **(extra or {}))
        out = rd + "/out"
        t0 = time.monotonic()
        with open(out, "w") as fo, open(rd + "/err", "w") as fe:
            try:
                rc = subprocess.run(cmd, cwd=rd + "/ws", env=env, stdin=subprocess.DEVNULL,
                                    stdout=fo, stderr=fe, timeout=TIMEOUT).returncode
            except subprocess.TimeoutExpired:
                rc = "timeout"
        wall = time.monotonic() - t0
        try:
            calls, text = parse(out)
        except Exception as e:  # noqa: BLE001 - recorded, not fatal
            calls, text = [], f"<parse error: {e}>"
        if tool == "spore":
            if wall > 60:
                # Keep the evidence of a slow run: the transcript and the daemon log.
                keep = os.path.join(HERE, "slow", f"{task}-run{run}")
                os.makedirs(keep, exist_ok=True)
                import urllib.request
                for s in json.load(urllib.request.urlopen("http://127.0.0.1:7790/api/sessions?children=1")):
                    with open(os.path.join(keep, s["id"] + ".json"), "wb") as f:
                        f.write(urllib.request.urlopen("http://127.0.0.1:7790/api/sessions/" + s["id"]).read())
                shutil.copy(rd + "/data/daemon.log", keep)
            stop_spore(rd)
        rec = {"tool": tool, "task": task, "run": run, "rc": rc, "wall_s": round(wall, 2),
               "calls": len(calls),
               "in": sum(c["in"] for c in calls), "cache_write": sum(c["cache_write"] for c in calls),
               "cache_read": sum(c["cache_read"] for c in calls), "out": sum(c["out"] for c in calls),
               "cost_usd": round(cost(calls), 5), "correct": grade(task, text), "answer": text[-1500:],
               "stderr_tail": open(rd + "/err", errors="replace").read()[-600:]}
        with open(OUT, "a") as f:
            f.write(json.dumps(rec) + "\n")
        print(f"{tool:9} {task} run{run} rc={rc} {wall:6.1f}s calls={rec['calls']:2} ${rec['cost_usd']:.4f} correct={rec['correct']}", flush=True)
    finally:
        shutil.rmtree(rd, ignore_errors=True)


def checkout():
    """Extract WORKSPACE_REF into a temp dir; every run copies it."""
    d = tempfile.mkdtemp(prefix="sbench-ws.", dir="/tmp")
    archive = subprocess.run(["git", "-C", REPO, "archive", WORKSPACE_REF], check=True, capture_output=True).stdout
    subprocess.run(["tar", "-x", "-C", d], input=archive, check=True)
    return d


def main():
    global WS
    # bench.py <first run> <last run> [tool,tool,...]
    first, last = int(sys.argv[1]), int(sys.argv[2])
    WS = checkout()
    only = set(sys.argv[3].split(",")) if len(sys.argv) > 3 else set(TOOLS)
    order = list(TOOLS)
    for run in range(1, last + 1):
        for task in TASKS:
            for tool in order:
                if run >= first and tool in only:
                    one(tool, task, run)
            order = order[1:] + order[:1]  # rotate who goes first
    shutil.rmtree(WS, ignore_errors=True)


if __name__ == "__main__":
    main()
