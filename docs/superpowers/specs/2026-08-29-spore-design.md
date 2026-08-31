# spore — design

**Date:** 2026-08-29
**Status:** approved (brainstorming dialogue), pending implementation plans

## 1. What spore is

spore is a personal AI agent: a single static Go binary that runs as an
always-on daemon on your own machine, reachable from a local web UI and from a
chat client (Telegram first). It owns its agent loop, tool dispatch, memory, and
model routing rather than delegating them to a framework.

Inspiration: [picoclaw](https://github.com/sipeed/picoclaw) — one small Go
binary, many providers, MCP support, real tools. spore takes that shape and
optimises for a different point: a daily driver the author fully controls,
where usefulness beats byte-shaving (though a Go single binary stays small and
cross-compiles to ARM/RISC-V for free).

### Goals

- A daily-driver agent, controlled end to end: own prompts, own tools, own policy.
- Always available: background daemon, browser UI, chat from a phone.
- Safe enough to expose to a chat app: deny rules that no prompt can talk past.
- Cheap to run: rule-based routing sends mechanical calls to a local model.
- Observable: every turn traceable in Phoenix.

### Non-goals (v1)

- Multi-user or multi-tenant operation. spore serves one person.
- A hosted service, auth system, or public endpoint.
- A heavyweight frontend toolchain. No Node build step.
- Sub-agents / agent teams. Deferred until the single-agent loop is solid.
- Voice, image generation, or vision input. Deferred.

## 2. Architecture

One binary, one process, one SQLite file.

```
spore (binary)
├── cmd/spore              CLI: serve, chat, once, session, recall, trace, mcp, doctor
├── internal/agent         the loop: turn execution, tool dispatch, compaction
├── internal/provider      anthropic/, openaicompat/ (+ shared Message/Tool types)
├── internal/router        rule-based model selection per call site
├── internal/tool          registry + builtins: fs, shell, web, schedule, memory, recall
├── internal/mcp           MCP client host (stdio + HTTP servers)
├── internal/policy        approval rules, pattern matching, decision persistence
├── internal/store         SQLite: sessions, messages, summaries, facts, jobs, approvals
├── internal/recall        Recall interface; sqlitefts and weaviate backends
├── internal/trace         OpenTelemetry + OpenInference span helpers
├── internal/daemon        HTTP + SSE API, supervision of bridges and scheduler
├── internal/bridge        telegram/ (discord/ later) — supervised goroutines
└── web/                   UI assets, go:embed'd into the binary
```

### Turn data flow

A client posts a message to the session API → the agent core loads session
history plus memory facts (and recalled snippets, if recall is enabled) → the
router picks a model for the call site → the provider streams a response → tool
calls are dispatched through the registry, each checked by the policy engine
(which may suspend the turn awaiting approval) → results append to the
transcript in SQLite → deltas stream to every client attached to that session
over SSE.

### Invariants

1. **The core never imports a transport.** `agent.Run(ctx, sessionID, input)`
   returns an event channel. HTTP, bridges and the CLI are consumers of that
   channel. This keeps an out-of-process client split available as a later
   refactor rather than a rewrite.
2. **A session is a database row, not a process.** Multiple clients may attach
   to one conversation; a turn survives a client disconnecting.
3. **Suspension is a first-class persisted state.** A turn awaiting approval is
   written to SQLite; the daemon can restart mid-approval and resume the turn.

### Alternatives considered

- **Out-of-process clients over JSON-RPC.** Better isolation and third-party
  clients, but pays protocol-versioning and supervision costs immediately for
  isolation not needed on a single personal box. Invariant 1 keeps it reachable.
- **Assemble from an existing Go agent framework.** Rejected: the loop,
  compaction and dispatch are the parts worth owning. An exception is made for
  MCP, where a maintained SDK is plumbing, not design.

## 3. The agent loop

```
load session → assemble context → route → provider call (streaming)
  → emit deltas → if tool calls: policy check → dispatch → append results → repeat
  → else: finalize turn, persist, emit done
```

### Context assembly

Fixed order, each part with a token budget:

1. System prompt (from config).
2. Memory facts (fact files; plus recalled snippets when recall is enabled).
3. Compaction summary, if the session has one.
4. The live message tail.

Assembly is a pure function over a session snapshot — testable with no network.

### Compaction

Triggers at a configured fraction of the active model's context window. Messages
older than a protected recent window are summarised by the cheap model into a
summary record attached to the session. Originals remain in SQLite permanently
and are only dropped from the assembled prompt, never deleted. Compaction is a
normal tool-less model call at call site `compaction`.

### Tool dispatch

Calls emitted in one assistant message run concurrently only when every call in
the batch is declared read-only; if any call mutates, the batch runs
sequentially in order. Each result carries an explicit truncation marker when it
exceeds its output budget, so the model can distinguish "empty" from "clipped".

### Errors

- Provider 429/5xx: retry with exponential backoff and jitter; other failures
  surface as a turn-level error event.
- A tool that panics is recovered and returned to the model as a tool error, so
  the agent can choose another path instead of losing the turn.
- Any sidecar (Weaviate, Phoenix) being unreachable degrades gracefully and
  never fails a turn.

## 4. Providers and routing

### Providers

Two adapters behind one interface:

- `anthropic` — native Messages API: streaming, tool use, prompt caching.
- `openaicompat` — one adapter covering OpenAI, DeepSeek, Groq, OpenRouter,
  vLLM, LM Studio, Ollama (via its OpenAI-compatible endpoint), and eventually
  geode's own server.

Ollama's native `/api/chat` is explicitly *not* implemented in v1; its
OpenAI-compatible endpoint is sufficient. Gemini's native API is deferred.

Models are referenced as `provider/model` (e.g. `anthropic/claude-opus-5`,
`ollama/qwen3:8b`), with provider endpoints and keys declared in config.

### Routing

Routing is config rules, not a model decision. Every LLM call in the codebase
must name its **call site**: `chat`, `compaction`, `title`, `classify`. There is
no `embed` call site — embeddings are computed Weaviate-side (section 5) and
never pass through the router.
Rules match on call site and optionally on estimated input size or whether tools
are required; first match wins; unmatched call sites fall through to
`default_model`.

```toml
[[route]]
when  = "compaction|title|classify"
model = "ollama/qwen3:8b"

[[route]]
when  = "chat"
model = "anthropic/claude-opus-5"
```

The call-site discipline is what makes per-session cost attribution possible.

## 5. Memory

Three layers with distinct jobs.

### Sessions — SQLite, always on

One file at `~/.spore/spore.db`. Tables: `sessions`, `messages` (role, content
blocks as JSON, token counts, call site, model, cost), `summaries`, `facts`,
`jobs`, `approvals`. Every message ever exchanged is retained; this is the
archive behind `spore session list|show|resume|export`. FTS5 over message text
provides keyword search across all history with no vector store present.

Build discipline: FTS5 requires `-tags sqlite_fts5` on every build and test.

### Facts — files, human-editable

`~/.spore/memory/*.md`, one fact per file with YAML frontmatter (`name`,
`description`, `type`). Loaded and budgeted during context assembly. spore
writes them via the `memory` tool; the user edits them directly in an editor or
git. The file is the source of truth.

### Recall — optional, Weaviate

```go
type Recall interface {
    Index(ctx context.Context, chunks []Chunk) error
    Search(ctx context.Context, query string, k int) ([]Hit, error)
    Status(ctx context.Context) (Status, error)
}
```

Backends: `sqlitefts` (default, zero setup, keyword) and `weaviate` (semantic).

Indexed content is curated: fact files, compaction summaries, and user messages.
Tool output is deliberately excluded — bulky and quick to go stale.

Vectors are computed Weaviate-side via `text2vec-ollama` pointed at the local
Ollama, so spore ships no embedding model and holds no embedding API key.

**Self-provisioning.** `recall_setup` is exposed both as `spore recall setup`
and as an agent tool, so "set up my vector store" is something spore can do when
asked. It: checks for Docker; writes `~/.spore/weaviate/compose.yml` pinned to an
exact Weaviate version; starts the container bound to localhost only; waits for
readiness; creates the collection with the Ollama vectorizer configured;
backfills from SQLite in batches emitting progress events; sets
`recall.backend = "weaviate"` in config. `spore recall status` reports container
health, object count and backfill lag; `spore recall teardown` reverses it.
Because it starts a container, provisioning passes through the policy engine.

**Degradation.** If Weaviate is configured but unreachable, recall falls back to
the `sqlitefts` backend and the turn continues with a warning event.

## 6. Tools, MCP, and policy

### Builtins

Each builtin is a small package implementing one interface (`Name`, `Schema`,
`ReadOnly`, `Call`):

| Tool | Operations |
|---|---|
| `fs` | read, write, edit, list, glob, grep |
| `shell` | exec with timeout and output cap |
| `web` | search, fetch-to-markdown |
| `schedule` | create, list, cancel background jobs |
| `memory` | fact create, update, delete, list |
| `recall` | search over history and facts |

The `memory` and `recall` builtins depend on subsystems that land in Plan 5 and
ship with it, and `schedule` ships in Plan 3 with the scheduler it drives; the
rest ship in Plan 2 (section 11).

Web search sits behind a provider interface with Brave as the first
implementation (clean paid API, no scraping fragility); Tavily and DDG are
later drop-ins.

### MCP

The official Go MCP SDK hosts client connections to servers declared in config
(stdio or HTTP). Their tools merge into the registry namespaced
`mcp__<server>__<tool>`. A server that fails to start is logged and skipped,
never fatal. This is the integration path for mycelium.

### Policy engine

Every tool call resolves to `allow`, `ask`, or `deny` by first match against
ordered rules, evaluated on the tool *and its arguments*:

```toml
[policy]
workspace = "~/dev"
default   = "ask"

allow = [ "fs.read", "fs.list", "fs.glob", "fs.grep", "web.*", "recall.*", "memory.*" ]
ask   = [ "fs.write", "fs.edit", "shell.exec", "schedule.*", "mcp__*" ]
deny  = [
  "fs.*(path outside workspace)",
  "fs.*(path matches **/.env, **/.ssh/**, **/*_rsa)",
  "shell.exec(matches rm -rf /, sudo, curl|sh, git push --force)",
]
```

- **Deny is checked first and is absolute.** No approval can override it. This
  is the barrier against prompt injection talking its way to `sudo`.
- **`ask` suspends the turn.** The pending call is persisted, an
  approval-request event goes to every attached client, and the first response
  wins. Answers are *once*, *always this session*, or *always this pattern* —
  the last writes a rule into a marked section of the config file, keeping
  policy readable and editable rather than an opaque cache.
- **Unanswered approvals deny** after a timeout and report back to the model, so
  a turn started from a phone cannot sit half-executed indefinitely.
- **Trust profiles.** Clients carry a profile (`local`, `remote`) and rulesets
  may differ per profile: the chat bridge can be strictly `ask` on writes while
  localhost is not.

Every tool action emits a structured event, so the UI can render tool calls
inline and the SQLite transcript is a complete audit log.

## 7. Tracing

OpenTelemetry Go SDK exporting OTLP/HTTP to Phoenix (default
`http://localhost:6006/v1/traces`). Span attributes follow the **OpenInference**
semantic conventions so Phoenix renders LLM, tool, and retriever spans natively.
OpenInference's Go instrumentation is thinner than its Python counterpart, so
attribute names are set by a small `internal/trace` helper package — one place
for convention drift to live.

Span tree per turn:

```
turn (session id, client, trust profile)
├── llm (call site, model, provider, prompt/completion, tokens in/out, cost, latency)
├── tool fs.read (args, result size, policy decision)
├── tool shell.exec (args, exit code, policy decision, approval latency)
├── retriever recall.search (query, backend, k, hit ids + scores)
└── llm (…)
```

Compaction and routing decisions are recorded as span events.

Config: `[trace] enabled` (off by default), `endpoint`, `sample_rate`, and
`redact` — when redacting, span shapes and token counts are kept and
prompt/completion text is dropped. `spore trace setup` writes a pinned Phoenix
compose file, starts it on localhost and flips the config, with the same
`status`/`teardown` verbs and policy gate as recall. Export failures are
non-fatal and never block a turn.

## 8. Clients

### Daemon

`spore serve` binds loopback only and carries no authentication — multi-user
operation and public endpoints are non-goals (section 1), so the trust boundary
is the machine. The API is small: list, create and show sessions; post a message
to a session; an SSE stream per session carrying turn deltas and tool-call
events; resolve a pending approval; and job create, list and cancel.

Multi-client behaviour falls out of that surface. One turn's events fan out to
every client attached to the session, and a client disconnecting never cancels
the turn — a turn's lifetime belongs to the daemon, not to the connection that
started it (invariant 2).

### Scheduler

A job is a prompt plus either a cron expression or a one-shot time. Firing opens
a **fresh session** and runs the prompt through the normal agent loop, so a job's
output is a session that can be opened in the UI and the policy engine applies
unchanged — a job that trips an `ask` rule suspends and waits exactly as a human
turn does. A job whose time passed while the daemon was down fires once on the
next start; missed runs are never backfilled.

### Web UI

Server-rendered HTML plus vanilla JS over the SSE stream, `go:embed`ed into the
binary. No Node toolchain and no build step is a deliberate requirement. Scope:
session list; transcript rendering tool calls as collapsible blocks; inline
approval prompts; per-turn model and cost readout. Anything beyond that is a
separate decision.

### Bridges

Telegram first, chosen because long polling requires no public endpoint or TLS
and therefore works from a machine behind NAT. One Telegram chat maps to one
spore session; approvals are inline keyboard buttons. Discord is the same
interface implemented again and is deferred.

### CLI

`spore serve | chat | once | session | recall | trace | mcp | doctor`.

`chat` and `once` are thin clients against the daemon's HTTP API — the same path
the web UI uses, so only one code path stays warm. When no daemon is listening
they start one: a detached `spore serve` that outlives the CLI invocation, so
scheduled jobs keep firing and a turn suspended awaiting approval can still be
resolved after the terminal closes. A pidfile makes that daemon discoverable, and
`spore serve --status | --stop` reports and stops it. `doctor` validates config,
provider keys, MCP servers, Docker, and sidecar health in one pass.

## 9. Configuration

One TOML file at `~/.spore/config.toml`, with `${ENV_VAR}` interpolation so
secrets live in the environment or the systemd unit and never in the file.
Policy rules written back by "always allow this pattern" land in a marked
section of the same file.

Deployment is `scp` plus a systemd unit; cross-compilation targets linux/amd64,
linux/arm64, and linux/riscv64.

## 10. Testing

The seams are chosen so the interesting parts test offline:

- Context assembly and the router are pure functions over session snapshots.
- Policy matching is table-tested, including an adversarial-path suite: `../`
  escapes, symlinks leaving the workspace, `.env` variants, quoted and chained
  shell forms.
- The agent loop runs against a **scripted fake provider** replaying canned
  tool-call sequences, giving golden-transcript tests for multi-step turns,
  compaction, and approval suspend/resume across a simulated restart.
- Providers get contract tests against recorded HTTP fixtures.
- One end-to-end test boots the daemon with the fake provider and drives it over
  HTTP.

No live API calls in CI.

## 11. Implementation staging

Five plans, each independently useful. Each plan is written only once its
predecessor completes.

1. **Core** — store and schema, config, provider interface with `anthropic` and
   `openaicompat`, the agent loop, compaction, router, `spore once` and `chat`
   against a local core. A thin OTel span layer lands here so later plans debug
   with traces rather than print statements.
2. **Tools and policy** — registry, `fs`/`shell`/`web` builtins, the policy
   engine, approval suspend/resume persisted across a restart.
3. **Daemon and web UI** — HTTP + SSE API, multi-client sessions, embedded UI,
   scheduler and background jobs.
4. **MCP and Telegram** — MCP client host, namespaced tools, the bridge with the
   `remote` trust profile and inline approvals.
5. **Memory and recall** — fact files, FTS search, the `Recall` interface, the
   Weaviate backend with `recall setup`, and `trace setup` for Phoenix.
