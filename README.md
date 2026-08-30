# spore

A personal AI agent in a single Go binary: your providers, your tools, your
policy. Built to run as an always-on daemon on your own machine.

Status: **Plan 1 (core)** — config, SQLite store, Anthropic and
OpenAI-compatible providers, call-site model routing, context assembly,
compaction, the agent loop, and OpenTelemetry tracing, driven by a CLI.
Tools, the daemon and web UI, MCP, Telegram, and Weaviate recall land in
Plans 2–5.

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

## Use

    spore once "what is this repo?"
    spore chat
    spore session list
    spore session show <id>

## Design

`docs/superpowers/specs/2026-08-29-spore-design.md`
