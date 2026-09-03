# spore

A personal AI agent in a single Go binary: your providers, your tools, your
policy. Built to run as an always-on daemon on your own machine.

Status: **Plan 4a (Discord bridge)** — everything in Plans 1–3, plus the
Discord bridge. Messages in Discord channels open threads and sessions; replies
continue them. A DM is one rolling session. Approvals arrive as button presses.
Sessions run under the `remote` trust profile, so you can hold them to stricter
rules than the web UI. The pattern-button is absent for calls with no
path-shaped argument to generalise to. MCP and Weaviate recall land in Plans 4b–5.

## Build

Every build and test needs the FTS5 tag:

    make build    # go build -tags sqlite_fts5 -o spore ./cmd/spore
    make test
    make vet

## Configure

spore reads `~/.spore/config.toml` and keeps everything else in
`~/.spore/spore.db`. Secrets are interpolated from the environment with
`${VAR}` and never stored in the file.

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

Anthropic requests carry no workspace by default, so the API acts in the
key's default workspace. An identity-linked key spanning several workspaces
rejects that; spore then adopts the default workspace the API names in the
response and retries. To pin one explicitly, set `workspace_id` on the
provider (or export `ANTHROPIC_WORKSPACE_ID`) to the `wrkspc_...` value from
the Console workspace URL.

Routing rules match a **call site** — `chat`, `compaction`, `title`, or
`classify` — so mechanical work runs on a cheap local model while
conversation runs on the good one.

## Discord

spore can be driven from Discord. Create an application and bot at
<https://discord.com/developers/applications>, enable the **Message Content**
privileged intent under Bot → Privileged Gateway Intents, and invite it to a
server only you are in with the `bot` scope and the Send Messages, Create
Public Threads, Send Messages in Threads, Read Message History and Embed Links
permissions.

    [bridge.discord]
    enabled     = true
    token       = "${DISCORD_BOT_TOKEN}"
    guild_id    = "your server id"
    channel_ids = ["the channel spore listens in"]
    user_ids    = ["your user id"]
    allow_dms   = true

`guild_id`, `channel_ids` and `user_ids` are an allowlist, not a filter:
anything not named is dropped without a reply. Turn on Discord's Developer
Mode (Settings → Advanced) to copy ids.

A message in an allowlisted channel opens a thread and a session; replies in
that thread continue it. A DM is one rolling session, reset with `/new`.
Approvals arrive as buttons.

Discord sessions run under the `remote` trust profile, so you can hold them to
a stricter ruleset than the local web UI:

    [policy.profile.remote]
    default = "ask"
    allow   = ["fs_read", "fs_list", "fs_glob", "fs_grep"]

## MCP servers

spore hosts MCP servers declared in its config and offers their tools to the
model as `mcp__<server>__<tool>`.

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

Declaring a server is the authorization to run it, so keep the file to servers
you trust. The child process gets only what you list: `env` verbatim, the
names in `inherit`, and `PATH`. Your provider API keys are not visible to it.
Its working directory is `policy.workspace`.

Tool calls are subject to the same policy as everything else — `mcp__*` is
asked by default, and denied outright for the `remote` trust profile, so a
Discord user cannot reach your servers. A server that fails to start is logged
and retried; its tools are simply absent until it comes back.

Run `spore mcp list` to see what each server contributed, and why a tool is
missing.

## Tools and policy

spore ships six filesystem tools (`fs_read`, `fs_write`, `fs_edit`,
`fs_list`, `fs_glob`, `fs_grep`), `shell_exec`, and `web_fetch` —
plus `web_search` when a search key is configured.

Every call is checked before it runs:

    [policy]
    workspace = "~/dev"       # filesystem tools may not leave this tree
    default   = "ask"
    allow     = ["fs_read", "fs_list", "fs_glob", "fs_grep", "web_*"]
    ask       = ["fs_write", "fs_edit", "shell_exec", "mcp__*"]
    deny      = ["shell_exec(matches terraform destroy)"]

    [web]
    brave_api_key = "${BRAVE_API_KEY}"

Rules are `tool` or `tool(predicate)`, where a predicate is
`path outside workspace`, `path matches <globs>`, or `matches <text>`. Tool
globs accept `fs.read` and `fs_read` interchangeably.

**Deny is checked first and is absolute.** A baseline deny list — paths
outside the workspace, `.env`, `.ssh`, private keys, and the usual
destructive shell forms — is always in force and is not opt-out. No approval
answer can override it.

An `ask` decision suspends the turn and prompts:

    allow? [y]es once  [n]o  [s]ession  [p]attern

`s` remembers the answer for the rest of the session; `p` writes a rule into
a marked block at the end of `config.toml`, which you can edit or delete.
An approval nobody answers within `approval_timeout` (default 5m) is denied.

Check a ruleset without running anything:

    spore policy check fs_write '{"path":"/etc/hosts"}'

## Memory and recall

spore keeps two kinds of long-term memory: **facts**, hand-written notes about
you and your projects, and a **keyword index** over everything spore has
said and read.

Facts live one-per-file under `<data_dir>/memory/*.md`, a plain Markdown file
with YAML-shaped frontmatter for three fixed keys, parsed by a small
hand-written reader rather than a general YAML library:

    ---
    name: prefers-tabs
    description: How the user wants Go code formatted
    type: user
    ---

    Gofmt defaults, tabs, no line-length limit.

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

    [context]
    fact_budget = 2000

`memory` (write and delete a fact — there is no read operation, since every
fact is already inlined into the prompt) is `ask` by default, and denied
outright to the `remote` trust profile: a fact written once shapes every
later turn of every session, so a single prompt-injected instruction over
Discord would otherwise plant permanent context. `recall_search` (read-only
keyword search) is allowed by default; for a `remote` session it is
additionally confined in the tool itself, not by policy, to that session's
own messages and summaries, with facts excluded entirely.

Three CLI verbs give you, the operator, the same index unscoped:

    spore recall search <query>     # keyword search over messages, summaries and facts
    spore recall status             # backend name and indexed counts per kind
    spore recall reindex            # rebuild from spore.db and the fact files

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

Recall is keyword-only (SQLite FTS5) in this release. Semantic search arrives
alongside the Weaviate backend, set up with `spore recall setup`.

## Running as a daemon

    spore serve                  # HTTP API, web UI and scheduler on 127.0.0.1:7777
    spore serve --status         # is one running?
    spore serve --stop           # stop it

`spore chat` and `spore once` are thin clients against that API — the same
path the web UI uses. If nothing is listening they start a daemon themselves
and leave it running, so scheduled jobs keep firing and an approval you have
not answered yet survives closing the terminal. Its log is at
`~/.spore/daemon.log` and its pidfile at `~/.spore/spore.pid`.

The daemon binds loopback and has no authentication: spore serves one person
on one machine. A non-loopback `addr` is rejected at load.

    [daemon]
    addr = "127.0.0.1:7777"
    tick_seconds = 30

## Web UI

`http://127.0.0.1:7777/` — session list, transcript with collapsible tool
calls, inline approval buttons, and the model and cost for each turn. It is
served out of the binary; there is no build step and nothing to install.

## Scheduled jobs

A job is a prompt plus a schedule: a five-field cron expression (UTC) or an
RFC3339 instant for a one-off. Each firing starts a **new** session, so a
recurring job never grows one unbounded thread, and policy applies to it
exactly as it does to a turn you typed — a job that trips an `ask` rule
suspends and waits for you.

    curl -s localhost:7777/api/jobs \
      -d '{"spec":"0 9 * * 1-5","prompt":"summarise yesterday'\''s commits"}'

The model can manage jobs itself through `schedule_create`, `schedule_list`
and `schedule_cancel`, which are in the default `ask` list.

If the daemon was down when a job was due, it fires once on the next start.
Missed runs are never backfilled.

## Use

    spore once "what is this repo?"
    spore chat
    spore session list
    spore session show <id>

## Design

`docs/superpowers/specs/2026-08-29-spore-design.md`
