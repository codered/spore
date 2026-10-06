#!/usr/bin/env python3
"""Summarise results.jsonl: per task and overall, per tool."""
import json, os, statistics as st, sys

HERE = os.path.dirname(os.path.abspath(__file__))
rows = [json.loads(l) for l in open(os.path.join(HERE, sys.argv[1] if len(sys.argv) > 1 else "results.jsonl"))]
tools = ["spore", "opencode", "pi", "prime"]
tasks = sorted({r["task"] for r in rows})


def pick(tool, task=None):
    return [r for r in rows if r["tool"] == tool and (task is None or r["task"] == task)]


def tin(r):
    return r["in"] + r["cache_write"] + r["cache_read"]


print("## per task (median wall, mean of the rest)\n")
print("| Task | Tool | Wall s | LLM calls | Input tok (cached %) | Output tok | Cost $ | Correct |")
print("|---|---|--:|--:|--:|--:|--:|:-:|")
for task in tasks:
    for tool in tools:
        rs = pick(tool, task)
        if not rs:
            continue
        tot_in = sum(tin(r) for r in rs)
        cached = sum(r["cache_read"] for r in rs) / tot_in * 100 if tot_in else 0
        print(f"| {task} | {tool} | {st.median(r['wall_s'] for r in rs):.1f} | {st.mean(r['calls'] for r in rs):.1f} | "
              f"{st.mean(tin(r) for r in rs)/1000:.1f}k ({cached:.0f}%) | {st.mean(r['out'] for r in rs):.0f} | "
              f"{st.mean(r['cost_usd'] for r in rs):.4f} | {sum(r['correct'] for r in rs)}/{len(rs)} |")

print("\n## overall\n")
print("| Tool | Runs | Total cost $ | Cost vs spore | Total wall s | LLM calls | Input tok | Output tok | Correct |")
print("|---|--:|--:|--:|--:|--:|--:|--:|:-:|")
base = sum(r["cost_usd"] for r in pick("spore"))
for tool in tools:
    rs = pick(tool)
    if not rs:
        continue
    c = sum(r["cost_usd"] for r in rs)
    print(f"| {tool} | {len(rs)} | {c:.3f} | {c/base:.2f}x | {sum(r['wall_s'] for r in rs):.0f} | "
          f"{sum(r['calls'] for r in rs)} | {sum(tin(r) for r in rs)/1000:.0f}k | {sum(r['out'] for r in rs)/1000:.1f}k | "
          f"{sum(r['correct'] for r in rs)}/{len(rs)} |")

print("\n## failures\n")
for r in rows:
    if not r["correct"] or r["rc"] != 0:
        print(f"- {r['tool']} {r['task']} run{r['run']} rc={r['rc']}: {r['answer'][-300:]!r}")
