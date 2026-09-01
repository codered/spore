# spore

A personal AI agent in a single Go binary: your providers, your tools, your
policy. Built to run as an always-on daemon on your own machine.

Status: **Plan 3 (daemon and web UI)** — everything in Plans 1 and 2, plus an
HTTP + SSE API, multi-client sessions, an embedded web UI, and the scheduler.
Tool calls resolve to allow, ask or deny on the tool and its arguments;
approvals suspend the turn, persist to SQLite, and survive a restart. The
Discord bridge, MCP, and Weaviate recall land in Plans 4–5.

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
