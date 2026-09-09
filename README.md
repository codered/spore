# spore

> A personal AI agent in a single Go binary: your providers, your tools, your policy.

![spore mascot](assets/spore.png)

[![Go](https://img.shields.io/badge/go-1.26+-blue)](https://golang.org)
[![Build](https://img.shields.io/github/actions/workflows/status/codered/spore/main.svg?branch=master)]()
[![Release](https://img.shields.io/github/v/release/codered/spore)]()
[![License: MPL-2.0](https://img.shields.io/badge/license-MPL--2.0-green.svg)](LICENSE)

**spore** runs as an always-on daemon on your own machine — a personal AI agent
that uses **your** providers, **your** tools, and enforces **your** policy.
Everything stays local; secrets are interpolated from the environment and never
stored in config files.

## Quick Start

```bash
# Build & install to ~/.local/bin (or set PREFIX)
make build && make install

# Point at your model provider
cat > ~/.spore/config.toml << 'EOF'
default_model = "anthropic/claude-opus-5"

[providers.anthropic]
kind      = "anthropic"
api_key   = "${ANTHROPIC_API_KEY}"
price_in  = 5.0
price_out = 25.0
EOF

# One-shot query
spore once "what is this repo?"

# Interactive chat (runs a daemon automatically)
spore chat
```

See [Installation](#installation) and [Configuration](#configure) for details.

---

## Contents

- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
  - [Chat interface](#the-chat-interface)
  - [Slash commands](#slash-commands)
  - [Skills](#skills)
  - [One-shot queries](#one-shot-queries)
  - [Sessions](#sessions)
  - [Scheduled jobs](#scheduled-jobs)
  - [Discord bridge](#discord-bridge)
- [Configuration](#configuration)
  - [Providers & routing](#providers--routing)
  - [Policy & tools](#policy--tools)
  - [MCP servers](#mcp-servers)
  - [Memory & recall](#memory--recall)
  - [Semantic search](#semantic-search)
  - [Tracing](#tracing)
- [Daemon](#daemon)
- [Web UI](#web-ui)
- [Design](#design)

---

## Features

| Feature | Description |
| --- | --- |
| **Multi-provider** | Anthropic, OpenAI-compatible (Ollama, etc.), with per-call routing |
| **Policy engine** | Fine-grained allow/deny/ask rules; baseline deny is always enforced |
| **Workspace ceiling** | Filesystem tools are confined to a configurable workspace tree |
| **MCP hosting** | Declare MCP servers; their tools are offered to the model as `mcp__<server>__<tool>` |
| **Discord bridge** | Drive spore from Discord with thread-per-session and approval buttons |
| **Memory & recall** | Hand-written facts + keyword search (always on); optional Weaviate for semantic search |
| **Skills** | `SKILL.md` procedures the model loads on demand; installing one always asks |
| **Slash commands** | `/clear`, `/compact`, `/context`, `/usage` in the chat interface |
| **Tracing** | Optional OpenTelemetry spans via Phoenix UI (`spore trace setup`) |
| **Scheduled jobs** | Cron-based or one-shot prompts that fire new sessions |
| **Single binary** | No build step, no dependencies — just `go build` and you're in |

---

## Installation

Every build needs the FTS5 tag:

```bash
make build    # go build -tags sqlite_fts5 -o spore ./cmd/spore
make test
make vet
```

`make install` puts the binary in `$(PREFIX)/bin` — `~/.local/bin` by default.

---

## Usage

### One-shot queries

```bash
spore once "what is this repo?"
```

### Sessions

```bash
spore session list
spore session show <id>
```

### The chat interface

`spore chat` runs a full-screen-free interface: the prompt stays at the bottom,
finished replies scroll away above it in your normal scrollback, and assistant
prose is rendered as markdown.

| Key | Action |
| --- | --- |
| `enter` | send |
| `ctrl+j` / `alt+enter` | newline in the message |
| `↑` / `↓` | previous and next message you sent |
| `y` `n` `s` `p` | answer an approval: once, deny, this session, always |
| `ctrl+c` | quit (a turn already running finishes in the daemon) |

Messages typed while a turn is running are queued and sent when it ends.
With stdin or stdout redirected, `spore chat` falls back to a plain
line-at-a-time loop, so pipes and scripts behave as they always did.

#### Slash commands

Type `/` to see them. They are handled by the chat interface itself, so they
are not available in the plain fallback loop, the web UI, or Discord.

| Command | What it does |
| --- | --- |
| `/clear` | Start fresh: moves the summary boundary to the newest message. Nothing is deleted — the transcript stays whole, and recall still finds it. |
| `/compact` | Fold the older messages into a summary now, without waiting for the automatic threshold. Reports how many messages were folded. |
| `/context` | What is in the prompt right now: system, environment, facts, skills, summary and live messages, each with its token estimate. |
| `/usage` | Tokens and cost, for this session and across every session. |

### Skills

A skill is a markdown document of instructions for one kind of work —
a release checklist, a review procedure, house style for a codebase. Each
lives in its own directory:

```
~/.spore/skills/release-checklist/SKILL.md
```

```markdown
---
name: release-checklist
description: How to cut a spore release
---

Tag from master only. Run make test and make vet first...
```

Every prompt carries an index of the names and descriptions, and the model
pulls a body in with `skill_load` when it needs one — so an unused skill costs
one line, and you can see in the transcript when one was loaded.

The skills directory sits outside the workspace ceiling, so the filesystem
tools cannot reach it. The only way in is the `skill_install` tool, which asks
for approval every time: the "always allow this pattern" answer is not offered
for it, because a skill written once shapes every later conversation. Discord
sessions cannot install one at all.

```toml
[skills]
scope = "global"           # or "workspace"; global is the default
dir   = "~/.spore/skills"  # optional; ignored under workspace scope
```

`workspace` scope reads `.spore/skills` under each session's own root instead,
so a project's skills travel with it. Be deliberate about that one: a session
rooted at a repository you cloned will read skills written by whoever wrote the
repository.

```toml
[subagents]
max_depth      = 2    # how deep the tree may go
max_cost_usd   = 1.00 # ceiling for the whole tree
max_concurrent = 4    # how many children may run at once
```

`max_depth` limits tree depth: the default 2 means a top-level session spawns
children and those children may not spawn. `max_cost_usd` is the cost ceiling
for a whole tree summing every agent in it; depth alone does not see a wide
flat fan-out. `max_concurrent` bounds how many children may run under one root,
since an unbounded spawn batch reaches provider rate limits before the cost
ceiling.

### Scheduled jobs

A job is a prompt plus a schedule — a five-field cron expression (UTC) or an
RFC3339 instant for a one-off. Each firing starts a **new** session, so a
recurring job never grows one unbounded thread, and policy applies to it
exactly as it does to a turn you typed — a job that trips an `ask` rule
suspends and waits for you.

```bash
curl -s localhost:7777/api/jobs \
  -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'
```

The model can manage jobs itself through `schedule_create`, `schedule_list`
and `schedule_cancel`, which are in the default `ask` list.

If the daemon was down when a job was due, it fires once on the next start.
Missed runs are never backfilled.

### Discord bridge

spore can be driven from Discord. Create an application and bot at
<https://discord.com/developers/applications>, enable the **Message Content**
privileged intent under Bot → Privileged Gateway Intents, and invite it to a
server only you are in with the `bot` scope and the Send Messages, Create
Public Threads, Send Messages in Threads, Read Message History and Embed Links
permissions.

```toml
[bridge.discord]
enabled     = true
token       = "${DISCORD_BOT_TOKEN}"
guild_id    = "your server id"
channel_ids = ["the channel spore listens in"]
user_ids    = ["your user id"]
allow_dms   = true
```

`guild_id`, `channel_ids` and `user_ids` are an allowlist, not a filter:
anything not named is dropped without a reply. Turn on Discord's Developer
Mode (Settings → Advanced) to copy ids.

A message in an allowlisted channel opens a thread and a session; replies in
that thread continue it. A DM is one rolling session, reset with `/new`.
Approvals arrive as buttons.

Discord sessions run under the `remote` trust profile, so you can hold them to
a stricter ruleset than the local web UI:

```toml
[policy.profile.remote]
default = "ask"
allow   = ["fs_read", "fs_list", "fs_glob", "fs_grep"]
```

---

## Configuration

spore reads `~/.spore/config.toml` and keeps everything else in
`~/.spore/spore.db`. Secrets are interpolated from the environment with
`${VAR}` and never stored in the file.

```toml
default_model = "anthropic/claude-opus-5"
show_cost     = false   # true appends " · $0.0038" to each turn footer

[providers.anthropic]
kind      = "anthropic"
api_key   = "${ANTHROPIC_API_KEY}"
price_in  = 5.0
price_out = 25.0

[providers.ollama]
kind     = "openai"
base_url = "http://localhost:11434/v1"

[[route]]
when  = "compaction|title|classify"
model = "ollama/qwen3:8b"
```

Anthropic requests carry no workspace by default, so the API acts in the
key's default workspace. An identity-linked key spanning several workspaces
rejects that; spore then adopts the default workspace the API names in the
response and retries. To pin one explicitly, set `workspace_id` on the
provider (or export `ANTHROPIC_WORKSPACE_ID`) to the `wrkspc_...` value from
the Console workspace URL.

Routing rules match a **call site** — `chat`, `compaction`, `title`, or
`classify` — so mechanical work runs on a cheap local model while
conversation runs on the good one.

### Providers & routing

Declare named providers and route call sites to specific ones:

```toml
[providers.anthropic]
kind      = "anthropic"
api_key   = "${ANTHROPIC_API_KEY}"

[[route]]
when  = "compaction|title|classify"
model = "ollama/qwen3:8b"
```

### Policy & tools

spore ships six filesystem tools (`fs_read`, `fs_write`, `fs_edit`,
`fs_list`, `fs_glob`, `fs_grep`), `shell_exec`, and `web_fetch` —
plus `web_search` when a search key is configured.

Every call is checked before it runs:

```toml
[policy]
workspace = "~/dev"       # filesystem tools may not leave this tree
default   = "ask"
allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"]
ask       = ["fs_write", "fs_edit", "shell_exec", "mcp__*"]
deny      = ["shell_exec(matches terraform destroy)"]

[web]
brave_api_key = "${BRAVE_API_KEY}"
```

`[policy] workspace` is a **ceiling**, not a working directory. Each session
records the directory it is rooted at: `spore chat` and `spore once` send the
directory you ran them in, and a creator with no directory of its own — the
web UI, the scheduler, the Discord bridge — gets `~/.spore/sessions/<id>`,
created on that session's first turn. A session rooted outside the ceiling is
refused at creation. `--workspace <dir>` roots a new session elsewhere and,
on a resume, re-roots an existing one.

Rules are `tool` or `tool(predicate)`, where a predicate is
`path outside workspace`, `path matches <globs>`, or `matches <text>`. Tool
globs accept `fs.read` and `fs_read` interchangeably.

**Deny is checked first and is absolute.** A baseline deny list — paths
outside the workspace, `.env`, `.ssh`, private keys, and the usual
destructive shell forms — is always in force and is not opt-out. No approval
answer can override it.

An `ask` decision suspends the turn and prompts:

```
allow? [y]es once  [n]o  [s]ession  [p]attern
```

`s` remembers the answer for the rest of the session; `p` writes a rule into
a marked block at the end of `config.toml`, which you can edit or delete.
An approval nobody answers within `approval_timeout` (default 5m) is denied.

Check a ruleset without running anything:

```bash
spore policy check fs_write '{"path":"/etc/hosts"}'
```

### MCP servers

spore hosts MCP servers declared in its config and offers their tools to the
model as `mcp__<server>__<tool>`.

```toml
[[mcp.server]]
name      = "notion"
transport = "stdio"
command   = "npx"
args      = ["-y", "@notionhq/notion-mcp-server"]
env       = { NOTION_TOKEN = "${NOTION_TOKEN}" }
inherit   = ["HOME"]

[[mcp.server]]
name      = "docs"
transport = "http"
url       = "https://mcp.example.com/mcp"
```

Declaring a server is the authorization to run it, so keep the file to servers
you trust. The child process gets only what you list: `env` verbatim, the
names in `inherit`, and `PATH`. Your provider API keys are not visible to it.
Its working directory is `policy.workspace` — the ceiling, not any one
session's root. One MCP host process is shared by every session. Unlike
the filesystem tools, MCP tool path arguments are not currently checked
against the calling session's workspace.

Tool calls are subject to the same policy as everything else — `mcp__*` is
asked by default, and denied outright for the `remote` trust profile, so a
Discord user cannot reach your servers. A server that fails to start is logged
and retried; its tools are simply absent until it comes back.

Run `spore mcp list` to see what each server contributed, and why a tool is
missing.

### Memory & recall

spore keeps two kinds of long-term memory: **facts**, hand-written notes about
you and your projects, and a **keyword index** over everything spore has
said and read.

Facts live one-per-file under `<data_dir>/memory/*.md`, a plain Markdown file
with YAML-shaped frontmatter for three fixed keys, parsed by a small
hand-written reader rather than a general YAML library:

```toml
---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Gofmt defaults, tabs, no line-length limit.
```

`name`, `description` and `type` are required; `type` is one of `user`,
`feedback`, `project` or `reference`. The file is the source of truth — spore
never stores a fact anywhere else — so you can write, edit or delete one by
hand, and put the directory under version control if you want history. The
model can also write facts through the `memory` tool.

Every fact is inlined into the system prompt on every turn, up to
`[context] fact_budget` estimated tokens (default 2000). A fact that would
push the section over budget is not dropped: it falls back to a one-line
`name: description` entry, and the model can pull the full body back with
`recall_search`.

```toml
[context]
fact_budget = 2000
```

`memory` (write and delete a fact — there is no read operation, since every
fact is already inlined into the prompt) is `ask` by default, and denied
outright to the `remote` trust profile: a fact written once shapes every
later turn of every session, so a single prompt-injected instruction over
Discord would otherwise plant permanent context. `recall_search` (read-only
keyword search) is allowed by default; for a `remote` session it is
additionally confined in the tool itself, not by policy, to that session's
own messages and summaries, with facts excluded entirely.

These CLI verbs give you, the operator, the same index unscoped:

```bash
spore recall search <query>     # search messages, summaries and facts
spore recall status             # backend name, indexed counts, degradation
spore recall reindex            # rebuild from spore.db and the fact files
spore recall setup              # provision the vector store and backfill it
spore recall teardown           # stop it and return to keyword search

$ spore recall search backoff
message  482  2026-08-30
    ...tried exponential backoff and jitter before...

$ spore recall status
backend: sqlitefts
KIND     INDEXED
fact     3
message  482
summary  11

$ spore recall reindex
reindexed 482 messages and summaries, 3 facts
```

#### Semantic search

Keyword search (SQLite FTS5) needs nothing and is always on. For semantic
search:

```bash
spore recall setup
```

That writes `~/.spore/weaviate/compose.yml`, starts Weaviate and a small
embedding container on loopback, backfills your history, and switches
`recall.backend` to `weaviate`. Restart the daemon afterwards. It needs
Docker and nothing else: no Ollama, no embedding API key. Vectors are
computed by the sidecar, because no Weaviate vectorizer runs in-process and
the alternative would be spore holding a key.

Already run Weaviate yourself? Set `recall.url` and skip setup entirely.

```toml
[recall]
backend = "weaviate"
url = "http://box.local:8080"
```

Weaviate being down is never fatal. Search falls back to the keyword index
and the turn continues:

```bash
$ spore recall status
backend: sqlitefts
degraded: weaviate at 127.0.0.1:8080: dial tcp: connect: connection refused
```

The fallback needs no repair afterwards, because the keyword index was never
behind: it is written inside the same transaction as the message it indexes.
Weaviate is a mirror, caught up from a watermark a few seconds later, so a
new message is searchable by keyword immediately and semantically shortly
after.

`spore recall teardown` stops the containers and goes back to keyword
search, keeping the data volume unless you pass `--purge`.

### Tracing

Off by default. To see turns, LLM calls, tool calls and retrievals as spans:

```bash
spore trace setup
```

This writes `~/.spore/phoenix/compose.yml`, starts Phoenix on loopback, waits
for it, and sets `trace.enabled = true`. Restart the daemon afterwards. The UI
is at http://localhost:6006.

`spore trace status` reports the configuration and whether the collector is
answering; `spore trace teardown` stops it and turns tracing back off, keeping
the data volume unless you pass `--purge`.

Prompts and completions are recorded in full, **including messages that
arrived over a bridge** — a Discord user's text is stored in the container's
volume along with everything else. Set `redact = true` under `[trace]` to keep
span shapes, token counts and costs while dropping the text.

If you already run a collector, point `trace.endpoint` at it and skip setup
entirely. Export failures never block a turn.

---

## Daemon

```bash
spore serve                  # HTTP API, web UI and scheduler on 127.0.0.1:7777
spore serve --status         # is one running?
spore serve --stop           # stop it
```

`spore chat` and `spore once` are thin clients against that API — the same
path the web UI uses. If nothing is listening they start a daemon themselves
and leave it running, so scheduled jobs keep firing and an approval you have
not answered yet survives closing the terminal. Its log is at
`~/.spore/daemon.log` and its pidfile at `~/.spore/spore.pid`.

The daemon binds loopback and has no authentication: spore serves one person
on one machine. A non-loopback `addr` is rejected at load.

```toml
[daemon]
addr = "127.0.0.1:7777"
tick_seconds = 30
```

---

## Web UI

`http://127.0.0.1:7777/` — session list, transcript with collapsible tool
calls, inline approval buttons, and the model and cost for each turn. It is
served out of the binary; there is no build step and nothing to install.

---

## Design

`docs/superpowers/specs/2026-08-29-spore-design.md`
