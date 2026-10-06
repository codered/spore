#!/usr/bin/env python3
"""Summarise sessions.jsonl from session.py."""
import json, os, statistics as st, sys

HERE = os.path.dirname(os.path.abspath(__file__))
rows = [json.loads(l) for l in open(os.path.join(HERE, sys.argv[1] if len(sys.argv) > 1 else "sessions.jsonl"))]
tools = [t for t in ["spore", "opencode", "pi", "prime"] if any(r["tool"] == t for r in rows)]
P = {"in": 2.00, "cache_write": 2.50, "cache_read": 0.20, "out": 10.00}


def of(tool):
    return [r for r in rows if r["tool"] == tool]


base = st.mean(r["cost_usd"] for r in of("spore")) if of("spore") else None

print("## per session (mean of runs)\n")
print("| Tool | Sessions | Cost | Cost vs spore | Turn 1 | Turns 2-10 | Wall | Correct |")
print("|---|--:|--:|--:|--:|--:|--:|:-:|")
for t in tools:
    rs = of(t)
    c = st.mean(r["cost_usd"] for r in rs)
    first = st.mean(r["turns"][0]["cost_usd"] for r in rs)
    rest = st.mean(sum(x["cost_usd"] for x in r["turns"][1:]) for r in rs)
    d = c - base
    vs = "baseline" if t == "spore" else f"{'+' if d >= 0 else '−'}${abs(d):.3f} ({'+' if d >= 0 else '−'}{abs(d)/base*100:.0f}%)"
    print(f"| {t} | {len(rs)} | ${c:.3f} | {vs} | ${first:.3f} | ${rest:.3f} | {st.mean(r['wall_s'] for r in rs):.0f} s | "
          f"{sum(r['correct'] for r in rs)}/{sum(len(r['turns']) for r in rs)} |")

print("\n## cost by token type (all sessions)\n")
print("| Tool | Cache write | Output | Cache read | Uncached input | Cached share of input |")
print("|---|--:|--:|--:|--:|--:|")
for t in tools:
    s = {k: sum(x[k] for r in of(t) for x in r["turns"]) for k in P}
    cost = {k: s[k] * P[k] / 1e6 for k in P}
    tot = sum(cost.values())
    inp = s["in"] + s["cache_write"] + s["cache_read"]
    print(f"| {t} | ${cost['cache_write']:.3f} ({cost['cache_write']/tot*100:.0f}%) | ${cost['out']:.3f} ({cost['out']/tot*100:.0f}%) | "
          f"${cost['cache_read']:.3f} ({cost['cache_read']/tot*100:.0f}%) | ${cost['in']:.3f} ({cost['in']/tot*100:.0f}%) | {s['cache_read']/inp*100:.0f}% |")

print("\n## per turn (mean cost, all runs)\n")
print("| Turn | " + " | ".join(tools) + " |")
print("|---" + "|--:" * len(tools) + "|")
for i in range(max(len(r["turns"]) for r in rows)):
    cells = []
    for t in tools:
        xs = [r["turns"][i] for r in of(t) if len(r["turns"]) > i]
        cells.append(f"${st.mean(x['cost_usd'] for x in xs):.4f}" + ("" if all(x["correct"] for x in xs) else " ✗"))
    print(f"| {i+1} | " + " | ".join(cells) + " |")

print("\n## wrong answers\n")
for r in rows:
    for x in r["turns"]:
        if not x["correct"]:
            print(f"- {r['tool']} run{r['run']} turn{x['turn']}: {x['answer'][-250:]!r}")
