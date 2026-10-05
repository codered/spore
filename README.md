<div align="center">

<img src="assets/spore.png" alt="spore" width="640">

# spore

**An always-on personal AI agent in a single Go binary.**<br>
Your models, your tools, your machine. Every action passes a policy engine you control.

[![CI](https://github.com/codered/spore/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/codered/spore/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/go-1.26+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![License: MPL-2.0](https://img.shields.io/badge/license-MPL--2.0-brightgreen.svg)](LICENSE)
[![MCP](https://img.shields.io/badge/MCP-stdio%20%2B%20http-8A2BE2)](#mcp-servers)
[![Platform](https://img.shields.io/badge/platform-linux%20%7C%20macOS-lightgrey)](#installation)

[Quick start](#-quick-start) ·
[Why spore](#-why-spore) ·
[Comparison](#-spore-vs-opencode-pi-prime-agent-and-other-agents) ·
[Features](#-feature-tour) ·
[Configuration](#%EF%B8%8F-configuration) ·
[Architecture](#-architecture)

</div>

---

spore is a personal agent that **lives on your machine as a daemon**, not in one
terminal tab. You talk to the same agent from a full-screen TUI, a browser, a
pipe, or your phone through Discord. It keeps running while you are away: it
fires scheduled jobs, holds approvals until you answer them, and learns from
its own conversations.

It is built for one person who wants an agent that is **capable and contained
at the same time**. The model can write and run whole programs, call MCP
servers and spawn sub-agents. Every one of those actions is still checked,
one call at a time, against rules that you wrote and that no prompt can talk
its way past.

<div align="center">

<img src="assets/tui-demo.gif" alt="spore chat: the model writes Go programs to inspect the repo, asks before writing .editorconfig, and continues once the write is allowed" width="900">

<sub>The model inspects the repo with <code>go_run</code> programs, stops to ask before it writes a file, and finishes once you press <kbd>alt</kbd>+<kbd>y</kbd>.</sub>

</div>

## ✨ Why spore

<table>
<tr>
<td width="50%" valign="top">

### 🛡️ Policy that the model cannot argue with
Every tool call goes through an `allow` / `ask` / `deny` engine before it
runs. A **baseline deny list** (credential files, paths outside the
workspace, destructive shell forms) is always on, and no approval, learned
rule or profile can remove it.

</td>
<td width="50%" valign="top">

### 🧠 Code mode, without giving up control
The model writes **one Go program per step** instead of a dozen round trips.
In our measurements this used **43% fewer input tokens**. Each `spore.*` call
inside the program is still judged by the policy engine as a separate call.

</td>
</tr>
<tr>
<td valign="top">

### 🌙 Always on
A loopback daemon runs the HTTP API, the web UI and the scheduler. Cron jobs
fire while your laptop lid is shut. An approval you have not answered
survives when you close the terminal.

</td>
<td valign="top">

### 📱 Reach it from anywhere
Use the **full-screen TUI**, the **web UI**, `spore once` in scripts, or
**Discord**: thread-per-session, approval buttons, and a stricter `remote`
trust profile for anything that arrives over the network.

</td>
</tr>
<tr>
<td valign="top">

### 🔁 It gets better on its own
**Refinement** reviews finished conversations and records what it learned as
memory facts and project notes. Every edit is in a ledger and can be rolled
back. Untrusted sources (Discord, scheduled jobs) only *propose* edits.

</td>
<td valign="top">

### 🔍 Memory you can read with `cat`
Facts are plain Markdown files. Keyword recall (SQLite FTS5) over everything
spore said and read is always on. With `spore recall setup` you also get
**semantic search** (Weaviate) in one command.

</td>
</tr>
<tr>
<td valign="top">

### 💸 Route by job, pay less
Send conversation to a frontier model and mechanical work (compaction,
titles, classification, refinement) to a **cheap local model**. Prompt
caching is on for Anthropic. Per-turn cost appears in the TUI.

</td>
<td valign="top">

### 📦 One binary, no runtime
No Node, no Python, no virtualenv. `make install` builds one static binary
with the TUI, web UI, MCP host, Go interpreter and Discord bridge in it.
Docker is needed only for the optional add-ons.

</td>
</tr>
</table>

## 🚀 Quick start

```bash
git clone https://github.com/codered/spore && cd spore
make install                       # builds with FTS5, installs to ~/.local/bin

mkdir -p ~/.spore
cat > ~/.spore/config.toml <<'EOF'
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind    = "anthropic"
api_key = "${ANTHROPIC_API_KEY}"   # read from the environment, never stored

[policy]
workspace = "~/dev"                # the tree that filesystem tools may touch
EOF

spore once "what is this repo?"    # one turn, printed to stdout
spore chat                         # full-screen TUI (starts the daemon for you)
open http://127.0.0.1:7777         # the same sessions in your browser
```

> [!TIP]
> Put secrets in `~/.spore/env` (`ANTHROPIC_API_KEY=...`). spore reads that file
> as a fallback for `${VAR}`, so the daemon has your keys even when it was not
> started from an interactive shell.

## 🥊 spore vs opencode, pi, Prime Agent and other agents

[opencode](https://opencode.ai), [pi](https://github.com/badlogic/pi-mono) and
[Prime Agent](https://primeintellect.ai) are excellent **terminal coding
harnesses**. opencode is the most complete of them: it has a permission
system, MCP, sub-agents, LSP, and a client/server design with a web UI. pi is
deliberately minimal: it has no MCP, no sub-agents and no permission prompts,
and you add those with extensions. Prime Agent is a hard fork of pi built
around a persistent Python REPL kernel. spore has a different goal: a
**personal agent that runs continuously, can be reached from anywhere, and is
safe to leave running**.

| | **spore** | **opencode** | **pi** | **Prime Agent** |
| --- | :---: | :---: | :---: | :---: |
| Distribution | Single Go binary | Single binary (Bun) | Node.js package | Node.js app + Python kernel |
| Built-in permission engine | ✅ allow / ask / deny | ✅ allow / ask / deny (most tools allowed by default) | ❌ by design (extensions) | ❌ (extensions) |
| Baseline deny that no rule or agent can override | ✅ | ❌ | ❌ | ❌ |
| Per-action policy inside code execution | ✅ every `spore.*` call is judged | — | — | ❌ the kernel has the user's OS permissions |
| Code-as-action ("CodeAct") | ✅ Go, interpreter in a child process | ❌ | ❌ | ✅ Python (IPython) |
| MCP servers | ✅ stdio + HTTP, tools offered directly | ✅ local + remote, tools offered directly | ❌ by design (extensions) | ⚠️ HTTP only, through Python skill packages |
| MCP path arguments held inside the workspace | ✅ | ❌ | — | ❌ |
| Sub-agents | ✅ depth, cost and concurrency limits | ✅ step limit per agent | ❌ by design | ✅ |
| Always-on daemon + scheduled jobs | ✅ | ⚠️ headless server, no scheduler | ❌ | ✅ |
| Chat-app bridge | ✅ Discord, with approval buttons | ⚠️ GitHub issues and PRs only | ❌ | ❌ |
| Web UI | ✅ in the binary | ✅ `opencode web` | ❌ (HTML export) | ❌ |
| Separate trust profile for remote input | ✅ `local` / `remote` | ❌ | ❌ | ❌ |
| Self-refinement of memory, with rollback | ✅ ledger + `/refine rollback` | ❌ | ❌ | ✅ |
| Semantic recall over your history | ✅ FTS5 always, Weaviate optional | ❌ | ❌ | ❌ |
| Per-call-site model routing | ✅ `[[route]]` | ⚠️ `small_model` for titles | ❌ | ❌ |
| OpenTelemetry tracing | ✅ Phoenix in one command | ❌ | ❌ | ❌ |
| LSP code intelligence | ❌ | ✅ | ❌ | ❌ |
| Breadth of providers and subscription logins | Anthropic + any OpenAI-compatible | ✅✅ 75+ | ✅✅ many | ✅✅ many |
| Extension ecosystem | Skills + MCP | ✅✅ plugins, agents, skills | ✅✅ TypeScript extensions, packages | ✅✅ extensions, packages, Python skills |

<sub>Comparison made against opencode 1.18's docs, pi-mono's README and Prime Agent 0.9.6's docs as of October 2026. ❌ means "not built in". Each project may add the feature through a plugin or extension. Corrections are welcome.</sub>

**Use spore when you want:**

- an agent you can **leave running** that does work on a schedule and waits for your approval, rather than one that exists only while a terminal is open;
- to let a model **write and run code** while every file read, shell command, fetch and MCP call is still checked against your rules;
- **guardrails that hold even when you misconfigure something**: the baseline deny list is not a default you can switch off;
- to reach your agent **from your phone** without exposing your machine. Discord input runs under its own stricter policy profile, and the daemon binds to loopback only;
- **local models** for the inexpensive work and a frontier model only for the conversation;
- one auditable, self-contained binary with **no package manager in the trust chain**.

**Pick opencode, pi or Prime Agent when** you live in a terminal coding
session and want the biggest provider list, subscription logins, LSP-aware
editing (opencode), session branching, or a large plugin ecosystem that you
can change freely.

**Compared with IDE and cloud coding agents** (Claude Code, Codex, Cursor and
others): those are tuned for the edit-test loop inside one repository. spore
is the agent around that work: it summarises yesterday's commits at 9 a.m.,
answers from Discord, remembers your preferences across projects, and keeps
the whole history searchable on your own disk.

## 🧭 Feature tour

<details open>
<summary><b>Full-screen TUI</b>: sessions, sub-agents, approvals and eight resource views</summary>

<br>

`spore chat` opens a full-screen interface: a session sidebar, the transcript
with collapsible tool calls, and approvals inline. With stdin or stdout
redirected it falls back to a plain line-at-a-time loop, so pipes and scripts
work.

| Key | Action |
| --- | --- |
| `enter` / `ctrl+j` | send / newline |
| `tab` | move focus between sidebar and chat (`j` / `k` move within a pane) |
| `y` `n` `s` `p` | answer an approval: once, deny, this session, always (`s` and `p` ask you to confirm) |
| `n` · `d` · `b` | new session · delete · jump to the next session blocked on you |
| `:` or `S` `A` `U` `J` `R` `C` `P` `M` | open a view: skills, agents, usage, jobs, refinements, MCP, policy, memory |
| `ctrl+b` · `?` · `q` | toggle sidebar · help · quit |

**Slash commands**: `/clear`, `/compact`, `/context` (token breakdown of the
prompt), `/usage`, `/agents`, `/skills`, `/refine [focus]`, `/refine rollback`.

</details>

<details open>
<summary><b>Web UI</b>: the same sessions in your browser, served from the binary</summary>

<br>

Open `http://127.0.0.1:7777/` while the daemon runs. Nothing needs to be
built or installed. It has the session list, transcripts with collapsible tool
calls, the model and cost of each step, scheduled jobs, and approvals with a
countdown to the automatic deny.

<table>
<tr>
<td width="50%"><img src="assets/web-transcript.png" alt="Web UI transcript: one go_run program counts Go lines per package, and the answer lists the five largest packages"></td>
<td width="50%"><img src="assets/web-approval.png" alt="Web UI approval: an fs_write from inside a go_run program waits for Allow once, Deny or This session"></td>
</tr>
<tr>
<td align="center"><sub>One <code>go_run</code> program answers a question that needs dozens of file reads.</sub></td>
<td align="center"><sub>A file write from inside a program waits for your answer.</sub></td>
</tr>
</table>

</details>

<details>
<summary><b>Policy engine</b>: allow / ask / deny, workspace ceiling, learned rules</summary>

<br>

```toml
[policy]
workspace = "~/dev"       # filesystem tools may not leave this tree
default   = "ask"
allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"]
ask       = ["fs_write", "fs_edit", "shell_exec", "mcp__*"]
deny      = ["shell_exec(matches terraform destroy)"]

[policy.profile.remote]   # applies to everything that arrives over Discord
default = "ask"
allow   = ["fs_read", "fs_list", "fs_glob", "fs_grep"]
```

- **Deny is checked first and is absolute.** The baseline deny list (paths
  outside the workspace, `.env`, `.ssh`, private keys, common destructive shell
  forms, MCP path arguments outside the workspace) cannot be turned off.
- Rules are `tool` or `tool(predicate)`, where a predicate is
  `path outside workspace`, `path matches <globs>` or `matches <text>`.
- `ask` suspends the turn. **s** remembers the answer for this session. **p**
  writes a rule with an absolute path into a marked block of `config.toml`,
  and the rule takes effect without a restart.
- An approval that nobody answers within `approval_timeout` (default 5m) is denied.
- `[policy] workspace` is a **ceiling**. Each session is rooted at the
  directory you started it in, and a root outside the ceiling is refused.

Test a rule without running anything:

```bash
spore policy check fs_write '{"path":"/etc/hosts"}'
```

</details>

<details>
<summary><b>Code mode</b>: the model writes Go, and spore judges every action in it</summary>

<br>

By default the model is offered one tool, `go_run`. It sends a complete
program that does the whole job:

```go
package main

import ("encoding/json"; "fmt"; "spore")

func main() {
	body, _ := spore.Fetch("https://wttr.in/London?format=j1")
	var w struct{ Current []struct{ TempC string `json:"temp_C"` } `json:"current_condition"` }
	json.Unmarshal([]byte(body), &w)
	fmt.Println(w.Current[0].TempC + "°C")
}
```

The `spore` package exposes every tool: `Fetch`, `Search`, `ReadFile`,
`WriteFile`, `EditFile`, `List`, `Glob`, `Grep`, `Shell`, `Recall`, and
`Call(tool, args)` for anything else, MCP tools included.

**The sandbox.** Programs run under [yaegi](https://github.com/traefik/yaegi)
**in a child process**. `os`, `net`, `os/exec`, `syscall`, `unsafe` and
`reflect` cannot be imported, so the only way a program can affect the machine
is through `spore.*`. Each of those calls is checked by the policy engine
exactly like a direct call. An `ask` rule prompts you with the real tool and
arguments while the program waits. A crash or a runaway loop ends the child
process, never the daemon.

**Measured** (local Qwen3.8-27B, 4 tasks × 3 runs per mode, 24 runs in total):

| | code mode | tools mode |
| --- | :---: | :---: |
| Input tokens | **−43%** | baseline |
| LLM calls | **−21%** | baseline |
| Total wall time | **−10%** | baseline |
| Correct | **12 / 12** | 11 / 12 |
| "3 largest Go files" (28 files) | **53 s**, 4.3 calls | 207 s, 10.3 calls |

Code mode wins by a large margin when a task needs many tool calls. For one
or two lookups, tools mode is faster. Switch with `[kernel] mode = "tools"`.
These are small samples from one model, so read them as an indication only.

```toml
[kernel]
mode                = "code"   # or "tools"
timeout_seconds     = 60       # time spent waiting on an approval is not counted
max_timeout_seconds = 300
ceiling_seconds     = 1800     # wall-clock limit, approvals included
```

</details>

<details>
<summary><b>MCP servers</b>: stdio and HTTP, with a restricted environment and path containment</summary>

<br>

```toml
[[mcp.server]]
name      = "notion"
transport = "stdio"
command   = "npx"
args      = ["-y", "@notionhq/notion-mcp-server"]
env       = { NOTION_TOKEN = "${NOTION_TOKEN}" }
inherit   = ["HOME"]

[[mcp.server]]
name        = "repos"
transport   = "http"
url         = "https://mcp.example.com/mcp"
local_paths = false   # its "paths" are repository paths, not files on this machine
```

- Tools appear as `mcp__<server>__<tool>`, are `ask` by default, and are
  **denied outright** for the `remote` profile.
- A child process gets only `env`, the variables named in `inherit`, and
  `PATH`. **Your provider keys are not visible to it.**
- Path-like arguments (by name or by shape, at any depth) are checked against
  the session's workspace by a baseline rule that no approval can override.
- A server that fails is retried, and its tools are absent until it returns.
  `spore mcp list` shows what each server provides.

</details>

<details>
<summary><b>Sub-agents</b>: delegate work with depth, cost and concurrency limits</summary>

<br>

| Tool | Use |
| --- | --- |
| `agent_run` | Run a sub-agent and wait for its answer. |
| `agent_spawn` | Start one in the background and continue. |
| `agent_result` | Read its state, and its answer when it has finished. |

A sub-agent inherits the trust profile and workspace of its parent, **never
more**. Its approvals go to the human at the top of the tree, and a sub-agent
cannot approve its own request or a sibling's. Only a human can stop one.

```toml
[subagents]
max_depth      = 2      # a top-level session can spawn, its children cannot
max_cost_usd   = 1.00   # limit for the whole tree
max_concurrent = 4
```

</details>

<details>
<summary><b>Discord bridge</b>: your agent on your phone</summary>

<br>

```toml
[bridge.discord]
enabled     = true
token       = "${DISCORD_BOT_TOKEN}"
guild_id    = "your server id"
channel_ids = ["the channel spore listens in"]
user_ids    = ["your user id"]
allow_dms   = true
```

- The ids form an **allowlist**. Messages from anyone else are dropped without a reply.
- A message in a channel opens a thread and a session. A DM is one ongoing
  session, and `/new` resets it.
- 👀 means the message was picked up and ✅ means the turn was answered. A
  turn's tool calls collapse into one line (`⚙ fs_read · shell_exec (2 tools)`).
  **Show details** displays the arguments and results, visible only to you.
- Approvals arrive as buttons. Sessions run under the `remote` profile, which
  cannot install skills or write memory facts.

Setup: create a bot at the
[Discord developer portal](https://discord.com/developers/applications), enable
the **Message Content** intent, and invite it with the `bot` scope and the
permissions Send Messages, Create Public Threads, Send Messages in Threads,
Read Message History, Embed Links and Add Reactions.

</details>

<details>
<summary><b>Scheduled jobs</b>: cron or one-off prompts that run while you are away</summary>

<br>

```bash
curl -s localhost:7777/api/jobs \
  -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'
```

A schedule is a five-field cron expression (UTC) or an RFC3339 timestamp. Each
run starts a **new** session, and policy applies as usual: a job that reaches
an `ask` rule waits for you. The model can manage jobs itself with
`schedule_create`, `schedule_list` and `schedule_cancel`, which are `ask` by
default. A job missed while the daemon was down runs once at the next start.
Missed runs are not backfilled.

</details>

<details>
<summary><b>Memory, recall and persona</b>: Markdown facts, full-history search, soul.md</summary>

<br>

**Facts** are one Markdown file each under `~/.spore/memory/`. They are the
source of truth, so you can edit them, delete them or keep them in git:

```markdown
---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Gofmt defaults, tabs, no line-length limit.
```

Facts are added to the prompt up to `[context] fact_budget` tokens. A fact over
the budget is reduced to a one-line index entry that the model can expand with
`recall_search`.

**Persona.** `~/.spore/soul.md` holds personality and global instructions.
`<workspace>/.spore/agent.md` holds standing instructions for one project.

**Recall** indexes every message, summary and fact:

```bash
spore recall search backoff     # keyword search (SQLite FTS5), always on
spore recall setup              # Weaviate + embedding sidecar on loopback (needs Docker)
spore recall status             # backend, counts, and whether it is degraded
spore recall teardown [--purge] # return to keyword-only search
```

The keyword index is written in the same transaction as each message.
Weaviate is a mirror that catches up a few seconds later. If Weaviate is down,
search falls back to keywords and the turn continues.

</details>

<details>
<summary><b>Refinement</b>: spore reviews its own conversations</summary>

<br>

A review runs when a session goes idle (`[refine] idle_minutes`, default 10),
when compaction folds old messages, when the model asks for one, or when you
type `/refine [focus]`. It produces small create, update and delete edits to
facts and project notes, each based on evidence from the conversation.

- Chat sessions apply edits immediately. **Discord and scheduled-job sessions
  only propose them**: accept (`a`) or reject (`r`) them in the `R` view.
- Every edit is in a ledger. `/refine rollback` undoes the last round.
- The reviewer never sees tool output, only what you and spore said.
- Route it to an inexpensive model with `[[route]] when = "refinement"`.

</details>

<details>
<summary><b>Skills</b>: on-demand procedures that only you can install</summary>

<br>

A skill is a `SKILL.md` file (a release checklist, a review procedure, house
style) in `~/.spore/skills/<name>/`. Each prompt includes only the skill names
and descriptions, and the model loads a full skill with `skill_load` when it
needs one.

The skills directory is outside the workspace, so filesystem tools cannot
write to it. `skill_install` asks you **every time**, with no "always allow"
option, because a skill shapes every later conversation. Discord sessions
cannot install skills.

```toml
[skills]
scope = "global"   # or "workspace": read <root>/.spore/skills, which then travel with the repo
```

</details>

<details>
<summary><b>Tracing</b>: every turn, LLM call, tool call and retrieval as OpenTelemetry spans</summary>

<br>

```bash
spore trace setup      # starts Phoenix on loopback; UI at http://localhost:6006
spore trace status
spore trace teardown [--purge]
```

Prompts and completions are recorded in full, including those from Discord.
`[trace] redact = true` keeps span shapes, token counts and costs but drops the
text. To use an existing collector, set `trace.endpoint`. Export failures never
block a turn.

</details>

## ⚙️ Configuration

spore reads `~/.spore/config.toml` and stores everything else in
`~/.spore/spore.db`. Secrets are interpolated from the environment, or from
`~/.spore/env`, with `${VAR}`. They are never written to the config file.

```toml
default_model = "anthropic/claude-opus-5"
show_cost     = true

[providers.anthropic]
kind      = "anthropic"
api_key   = "${ANTHROPIC_API_KEY}"
price_in  = 5.0
price_out = 25.0
# workspace_id = "wrkspc_..."   # only for keys that span several workspaces

[providers.ollama]
kind     = "openai"             # any OpenAI-compatible endpoint
base_url = "http://localhost:11434/v1"

# Call sites: chat, compaction, title, classify, refinement
[[route]]
when  = "compaction|title|classify|refinement"
model = "ollama/qwen3:8b"

[web]
brave_api_key = "${BRAVE_API_KEY}"   # enables web_search

[daemon]
addr = "127.0.0.1:7777"   # loopback only; a non-loopback address is rejected
```

**Built-in tools:** `fs_read`, `fs_write`, `fs_edit`, `fs_list`, `fs_glob`,
`fs_grep`, `shell_exec`, `web_fetch`, `web_search`, `go_run`, `memory`,
`recall_search`, `skill_load`, `skill_install`, `agent_run`, `agent_spawn`,
`agent_result`, `schedule_*`, `refine`, plus every tool from your MCP servers.

## 🖥️ CLI

```text
spore once <prompt>                 one turn in a new session, reply on stdout
spore chat [session-id]             full-screen TUI (resumes when given an id)
spore serve [--status|--stop]       daemon: HTTP API, web UI, scheduler
spore session list [--all]          recent sessions (--all includes sub-agents)
spore session show <id>             print a transcript
spore session delete <id>... | --all [--discord] [--yes]
spore policy check <tool> [json]    show the decision a call would get
spore mcp list                      connect to MCP servers and list their tools
spore recall search|status|reindex|setup|teardown
spore trace setup|status|teardown

flags:  -config <path>   --workspace <dir>
```

`spore chat` and `spore once` are thin clients of the daemon. If no daemon is
listening, they start one and leave it running. The daemon log is
`~/.spore/daemon.log`.

## 🏗️ Architecture

```text
 TUI ─┐                       ┌───────────── spore daemon (127.0.0.1) ─────────────┐
 Web ─┼── HTTP / SSE ────────▶│  agent loop ─▶ router ─▶ providers (Anthropic, OAI) │
 CLI ─┤                       │      │                                             │
 Discord bridge ──────────────▶│      ▼                                             │
                              │  policy engine ──▶ tools · MCP host · sub-agents    │
                              │      ▲                                             │
                              │      └── go_run child process (yaegi) ── spore.*    │
                              │                                                    │
                              │  scheduler · refinement · recall (FTS5 / Weaviate)  │
                              │  SQLite store · OpenTelemetry                       │
                              └────────────────────────────────────────────────────┘
```

| Package | Role |
| --- | --- |
| `internal/agent` | turn loop, context assembly, compaction |
| `internal/policy` | rule engine, baseline deny, trust profiles |
| `internal/kernel` | `go_run`: yaegi interpreter in a child process, `spore.*` bridge |
| `internal/mcp` | MCP host (stdio + HTTP) |
| `internal/subagent` | sub-agent tree, limits, approval routing |
| `internal/recall` | FTS5 index, Weaviate mirror |
| `internal/refine` | self-review and edit ledger |
| `internal/daemon` · `internal/scheduler` | HTTP API, SSE, cron |
| `internal/tui` · `web/` | full-screen terminal UI, embedded web UI |
| `internal/bridge/discord` | Discord bridge |

Design notes are in [`docs/superpowers/specs/`](docs/superpowers/specs/).

## 🔒 Security model

spore serves **one person on one machine**. The daemon has no authentication,
so it binds to loopback and refuses any other address. Inside that boundary,
it is designed so that a prompt injection cannot do more than you allowed:

- the baseline deny list cannot be overridden by any answer, rule or profile;
- network-originated input (Discord) runs under the `remote` profile: no MCP,
  no memory writes, no skill installs, and recall limited to its own session;
- code runs in a child process that can act only through policy-checked calls;
- MCP servers do not inherit your environment or your provider keys;
- skills and memory facts, which shape every later prompt, always require a
  human to approve them.

## 📦 Installation

Requires Go 1.26+ and a C compiler (for SQLite). Linux and macOS are
supported. On Windows, use `[kernel] mode = "tools"`.

```bash
make build      # go build -tags sqlite_fts5 -o spore ./cmd/spore
make install    # to $(PREFIX)/bin, default ~/.local/bin
```

## 🤝 Contributing

```bash
make test       # go test -tags sqlite_fts5 ./...
make vet
make fmtcheck
make lint       # pinned golangci-lint, the same as CI
```

CI also runs `govulncheck` and `go mod tidy` checks. Issues and pull requests
are welcome.

After a UI change, `make demo` re-records `assets/tui-demo.gif` from
[`assets/demo/tui.tape`](assets/demo/tui.tape). It needs
[vhs](https://github.com/charmbracelet/vhs) and `ANTHROPIC_API_KEY`, and runs
against a separate demo daemon that it removes afterwards. If Chromium reports
"No usable sandbox", run `VHS_NO_SANDBOX=true make demo`.

## 📄 License

[MPL-2.0](LICENSE)
