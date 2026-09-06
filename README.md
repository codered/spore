# spore

**Your personal AI agent in a single Go binary.** Providers, tools, policy — all yours. Runs as an always-on daemon on your own machine.

<!-- Badges -->
[![Go](https://img.shields.io/badge/Go-1.26+-blue.svg)](https://go.dev/)
[![Build](https://img.shields.io/github/actions/svg/codered/spore?branch=master)](https://github.com/codered/spore)
[![Release](https://img.shields.io/github/v/release/codered/spore)](https://github.com/codered/spore/releases)
[![License](https://img.shields.io/github/license/codered/spore)](LICENSE)

---

## Quick start

```bash
# Build from source
make build && make install        # installs to ~/.local/bin/spore by default

# Run a one-shot query
spore once "what is this repo?"

# Start an interactive chat
spore chat

# Or run as a background daemon (web UI at http://localhost:7777/)
spore serve
```

> [!TIP]
> See the full [configuration guide](#configure) below for model providers, routing, policy, and integrations.

---

## Table of contents

- [Features](#features)
- [Installation](#installation)
- [Usage](#usage)
  - [The chat interface](#the-chat-interface)
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

| Area | What it does |
| --- | --- |
| **Multi-provider** | Anthropic, Ollama, or any OpenAI-compatible backend — configured in one TOML file |
| **Model routing** | Route mechanical work (compaction, titles, classification) to cheap local models while keeping conversation on the good one |
| **Policy engine** | Fine-grained `allow` / `ask` / `deny` per tool, with a baseline deny list you can't override and predicate-based rules (`path outside workspace`, `matches <text>`) |
| **MCP servers** | Host stdio or HTTP MCP servers; their tools surface as `mcp__<server>__<tool>` and are subject to the same policy |
| **Discord bridge** | Drive spore from Discord — channels become threads, DMs become rolling sessions, approvals arrive as buttons |
| **Memory & recall** | Hand-written facts live one-per-file under `~/.spore/memory/`; a keyword index (FTS5) covers everything spore has said and read; optional Weaviate for semantic search |
| **Scheduled jobs** | Cron or one-shot prompts that spawn new sessions each firing, with the same policy and approval flow as a user-typed turn |
| **Tracing** | Optional OpenTelemetry collector (Phoenix) for turns, tool calls, retrievals, and costs |

---

## Installation

```bash
# Build from source
make build
make install          # PREFIX=~/.local by default

# Or grab the binary from Releases
curl -L https://github.com/codered/spore/releases/latest/download/spore-linux-x64 \
  -o spore && chmod +x spore
```

---

## Usage

### The chat interface

`spore chat` runs a line-buffered REPL — the prompt stays at the bottom, finished replies scroll above.

| Key | Action |
| --- | --- |
| `Enter` | Send |
| `Ctrl+J` / `Alt+Enter` | Newline in message |
| `↑` / `↓` | Previous / next message you sent |
| `y` · `n` · `s` · `p` | Answer an approval: once, deny, this session, always |
| `Ctrl+C` | Quit (a running turn finishes in the daemon) |

With stdin or stdout redirected, `spore chat` falls back to a plain line-at-a-time loop.

### One-shot queries

```bash
spore once "summarise the last five commits"
```

No session persistence — just a single prompt and response.

### Sessions

```bash
spore session list
spore session show <id>
```

`spore chat` and `spore once` root a session at the directory you ran them in, so two sessions in two projects each see their own files. A creator with no directory of its own — the web UI, the scheduler, the Discord bridge — gets `~/.spore/sessions/<id>`.

### Scheduled jobs

```bash
curl -s localhost:7777/api/jobs \
  -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'
```

Each firing starts a **new** session. If the daemon was down when a job was due, it fires once on the next start — missed runs are never backfilled.

### Discord bridge

Enable in config (see below) and drive spore from your own server. A message in an allowlisted channel opens a thread and a session; replies in that thread continue it. A DM is one rolling session, reset with `/new`. Approvals arrive as buttons.

Discord sessions run under the `remote` trust profile by default:

```toml
[policy.profile.remote]
default = "ask"
allow   = ["fs_read", "fs_list", "fs_glob", "fs_grep"]
```

---

## Configuration

spore reads `~/.spore/config.toml` and keeps everything else in `~/.spore/spore.db`. Secrets are interpolated from the environment with `${VAR}` and never stored in the file.

### Providers & routing

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

# Route mechanical work to a cheap local model
[[route]]
when  = "compaction|title|classify"
model = "ollama/qwen3:8b"
```

Anthropic requests carry no workspace by default. An identity-linked key spanning several workspaces rejects that; spore then adopts the default workspace the API names in the response and retries. To pin one explicitly, set `workspace_id` on the provider (or export `ANTHROPIC_WORKSPACE_ID`) to the `wrkspc_...` value from the Console workspace URL.

### Policy & tools

```toml
[policy]
workspace = "~/dev"       # filesystem tools may not leave this tree
default   = "ask"
allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"]
ask       = ["fs_write", "fs_edit", "shell_exec", "mcp__*"]
deny      = ["shell_exec(matches terraform destroy)"]
```

Rules are `tool` or `tool(predicate)`, where a predicate is `path outside workspace`, `path matches <globs>`, or `matches <text>`. Tool globs accept `fs.read` and `fs_read` interchangeably.

**Deny is checked first and is absolute.** A baseline deny list — paths outside the workspace, `.env`, `.ssh`, private keys, and the usual destructive shell forms — is always in force and is not opt-out. No approval answer can override it.

An `ask` decision suspends the turn and prompts:

```
allow? [y]es once  [n]o  [s]ession  [p]attern
```

`s` remembers the answer for the rest of the session; `p` writes a rule into a marked block at the end of `config.toml`, which you can edit or delete. An approval nobody answers within `approval_timeout` (default 5m) is denied.

Check a ruleset without running anything:

```bash
spore policy check fs_write '{"path":"/etc/hosts"}'
```

### MCP servers

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

Declaring a server is the authorization to run it, so keep the file to servers you trust. The child process gets only what you list: `env` verbatim, the names in `inherit`, and `PATH`. Its working directory is `policy.workspace` — the ceiling, not any one session's root. One MCP host process is shared by every session.

Tool calls are subject to the same policy as everything else — `mcp__*` is asked by default, and denied outright for the `remote` trust profile. A server that fails to start is logged and retried; its tools are simply absent until it comes back.

Run `spore mcp list` to see what each server contributed, and why a tool is missing.

### Memory & recall

Facts live one-per-file under `<data_dir>/memory/*.md`:

```markdown
---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Gofmt defaults, tabs, no line-length limit.
```

`name`, `description`, and `type` are required; `type` is one of `user`, `feedback`, `project`, or `reference`. The file is the source of truth — spore never stores a fact anywhere else — so you can write, edit, or delete one by hand, and put the directory under version control if you want history. The model can also write facts through the `memory` tool.

Every fact is inlined into the system prompt on every turn, up to `[context] fact_budget` estimated tokens (default 2000). A fact that would push the section over budget falls back to a one-line `name: description` entry; the model can pull the full body back with `recall_search`.

```bash
spore recall search <query>     # search messages, summaries, and facts
spore recall status             # backend name, indexed counts, degradation
spore recall reindex            # rebuild from spore.db and the fact files
spore recall setup              # provision the vector store and backfill it
spore recall teardown           # stop it and return to keyword search
```

### Semantic search

Keyword search (SQLite FTS5) needs nothing and is always on. For semantic search:

```bash
spore recall setup
```

That writes `~/.spore/weaviate/compose.yml`, starts Weaviate and a small embedding container on loopback, backfills your history, and switches `recall.backend` to `weaviate`. Restart the daemon afterwards. It needs Docker and nothing else — no Ollama, no embedding API key. Vectors are computed by the sidecar, because no Weaviate vectorizer runs in-process and the alternative would be spore holding a key.

Already run Weaviate yourself? Set `recall.url` and skip setup entirely:

```toml
[recall]
backend = "weaviate"
url     = "http://box.local:8080"
```

Weaviate being down is never fatal — search falls back to the keyword index and the turn continues. The fallback needs no repair afterwards, because the keyword index was never behind: it is written inside the same transaction as the message it indexes. Weaviate is a mirror, caught up from a watermark a few seconds later, so a new message is searchable by keyword immediately and semantically shortly after.

### Tracing

Off by default. To see turns, LLM calls, tool calls, and retrievals as spans:

```bash
spore trace setup
```

This writes `~/.spore/phoenix/compose.yml`, starts Phoenix on loopback, waits for it, and sets `trace.enabled = true`. Restart the daemon afterwards. The UI is at `http://localhost:6006`.

`spore trace status` reports the configuration and whether the collector is answering; `spore trace teardown` stops it and turns tracing back off, keeping the data volume unless you pass `--purge`.

Prompts and completions are recorded in full, **including messages that arrived over a bridge** — a Discord user's text is stored in the container's volume along with everything else. Set `redact = true` under `[trace]` to keep span shapes, token counts, and costs while dropping the text.

---

## Daemon

```bash
spore serve                  # HTTP API, web UI, and scheduler on 127.0.0.1:7777
spore serve --status         # is one running?
spore serve --stop           # stop it
```

`spore chat` and `spore once` are thin clients against that API — the same path the web UI uses. If nothing is listening they start a daemon themselves and leave it running, so scheduled jobs keep firing and an approval you have not answered yet survives closing the terminal. Its log is at `~/.spore/daemon.log` and its pidfile at `~/.spore/spore.pid`.

The daemon binds loopback and has no authentication — spore serves one person on one machine. A non-loopback `addr` is rejected at load.

```toml
[daemon]
addr         = "127.0.0.1:7777"
tick_seconds = 30
```

---

## Web UI

`http://localhost:7777/` — session list, transcript with collapsible tool calls, inline approval buttons, and the model and cost for each turn. It is served out of the binary; there is no build step and nothing to install.

---

## Design

See `docs/superpowers/specs/2026-08-29-spore-design.md` for the full design document.
