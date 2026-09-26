# spore — Go kernel and code mode

**Date:** 2026-09-26
**Status:** approved (brainstorming dialogue); user asked to write, commit and implement without further gates

## 1. What this adds

Today the model acts one tool call at a time: fetch a URL, read the result,
decide, call the next tool. A question like "what is the weather in London?"
costs three or four model round trips.

This spec adds a **Go kernel**: the model writes one complete Go program that
finishes the task, spore runs it in-process under the
[yaegi](https://github.com/traefik/yaegi) interpreter, and the program's
stdout goes back to the model. Inside the program, spore's tools are Go
functions in a package named `spore`. This is the CodeAct pattern that
prime-agent uses with a Python kernel.

In **code mode** (the new default) the model is offered exactly one tool,
`go_run`, and reaches every other tool through `spore.*`. In **tools mode**
the model sees every tool as today, `go_run` included.

The weather question becomes two model calls: one emits a program that calls
`spore.Fetch`, parses the JSON and prints a summary; one turns that output
into the answer.

### Decisions made during brainstorming

| Question | Decision |
|---|---|
| How code relates to existing tools | Code-first (CodeAct), behind a config switch that defaults to code |
| Runtime | yaegi, in-process, curated stdlib allowlist; not `go run` in a subprocess |
| State across `go_run` calls | None: a fresh interpreter per call |
| Policy | Every `spore.*` call goes through the existing `policy.Guard`, unchanged |

### Out of scope

- Persistent kernels (Jupyter-style state across calls).
- Third-party imports, and any import outside the allowlist in §3.
- A memory cap on a running program. The run timeout and the output cap are
  the only bounds in v1; a program can allocate until the daemon's memory
  limit. This is a known gap.
- A `spore.Parallel` helper. Goroutines and channels work inside programs.

## 2. Why yaegi and not `go run`

The policy engine judges a call by its tool name and its arguments. It can
judge `fs_write(path=~/.ssh/config)` but it cannot judge a Go program: a
program calling `os.RemoveAll` would pass every rule. With a real `go run`
the only safe policy is "ask before every program", which puts an approval
on the weather question and makes the deny baseline unenforceable.

Under yaegi the program can only reach what spore hands it. The stdlib is an
allowlist with no `os`, `os/exec`, `net`, `syscall`, `unsafe` or `reflect`.
The outside world is reachable only through `spore.*`, and every `spore.*`
call becomes a synthetic tool call through the same guard as a normal call.
Deny stays absolute; ask still prompts in the TUI and Discord with the real
tool name and arguments; audit rows and trace spans stay per action.

## 3. Components

### 3.1 `internal/kernel` — run one program

```go
type Options struct {
    Timeout   time.Duration // interpreter time budget, see §4.2
    Ceiling   time.Duration // wall-clock hard stop, see §4.2
    MaxOutput int           // stdout+stderr byte cap
}

type Result struct {
    Stdout    string
    Truncated bool     // stdout+stderr passed MaxOutput
    Calls     []Call   // every spore.* call, in order, for the footer
}

func Run(ctx context.Context, src string, r Runner, opt Options) (Result, error)
```

`Run` builds a fresh `interp.New` for each call with:

- `Stdout` and `Stderr` both going to one capped writer. It keeps the first
  `MaxOutput` bytes and counts the rest, the same shape as `shell.capWriter`.
- `GoPath` set to a path that does not exist and `SourcecodeFilesystem` set
  to an empty `fstest.MapFS`, so no import can load source from disk
  regardless of `$GOPATH`.
- `Env` empty and `Args` `["main"]`. Unrestricted is false.
- `Use` given only the allowlisted stdlib symbols plus the `spore` package.

**Stdlib allowlist** (keys of `stdlib.Symbols`): `bytes`, `encoding/base64`,
`encoding/csv`, `encoding/hex`, `encoding/json`, `errors`, `fmt`, `html`,
`math`, `math/rand`, `net/url`, `regexp`, `sort`, `strconv`, `strings`,
`text/tabwriter`, `text/template`, `time`, `unicode`, `unicode/utf8`.

`time` is copied with `Sleep`, `AfterFunc`, `After`, `Tick`, `NewTimer` and
`NewTicker` removed. A blocked native call does not stop when the run is
cancelled: probed on yaegi v0.16.1, `EvalWithContext` returns at the
deadline but the goroutine inside `time.Sleep` lives until the sleep ends.
`sync` is excluded for the same reason (`WaitGroup.Wait` and `Mutex.Lock`
block natively). Programs use channels and `select`, which are interpreted
and cancel cleanly, and `spore.Sleep`, which honours the run context.

**Errors.** A refused import produces yaegi's message, which mentions GOPATH.
`Run` rewrites any `import "X" error` to:
`package "X" is not available in go_run; allowed: <list>. Use spore.* for files, shell and network.`
Compile errors and panics are returned with yaegi's line:column prefix.
A run that passes its budget returns `go_run timed out after <budget>`
together with whatever stdout it produced.

### 3.2 `internal/kernel` — the `spore` package

Built per run by a bridge that holds the run context and a `Runner`:

```go
// Runner is the one method of policy.Guard the kernel needs. Declared here
// so kernel does not import policy.
type Runner interface {
    Run(ctx context.Context, call provider.Block) provider.Block
}
```

Each helper marshals its arguments to JSON, builds a
`provider.Block{Type: BlockToolUse, ID: "gorun-<n>", Name: tool, Input: args}`,
sends it through `Runner.Run`, and returns `(string, error)`. An error
result (policy deny, declined approval, tool failure) becomes a Go `error`
whose text is the result content.

| Helper | Tool | Arguments |
|---|---|---|
| `Fetch(url string) (string, error)` | `web_fetch` | `url` |
| `Search(query string, count int) (string, error)` | `web_search` | `query`, `count` |
| `ReadFile(path string) (string, error)` | `fs_read` | `path` |
| `WriteFile(path, content string) error` | `fs_write` | `path`, `content` |
| `EditFile(path, old, new string) error` | `fs_edit` | `path`, `old`, `new` |
| `List(path string) (string, error)` | `fs_list` | `path` |
| `Glob(pattern string) (string, error)` | `fs_glob` | `pattern` |
| `Grep(pattern, glob string) (string, error)` | `fs_grep` | `pattern`, `glob` |
| `Shell(command string) (string, error)` | `shell_exec` | `command` |
| `Recall(query string) (string, error)` | `recall_search` | `query` |
| `Call(tool string, args map[string]any) (string, error)` | any | passed through |
| `Sleep(d time.Duration) error` | none | honours the run context |

Empty optional arguments are omitted from the JSON (`count` 0, `glob` "").
`Call("go_run", …)` is refused inside the kernel with an error; nested
kernels buy nothing and would break the time accounting.

Each helper call opens a child span with `sporetrace.StartTool` on the run
context, so a trace reads `go_run → web_fetch → …`, and appends a `Call`
record `{Tool, Outcome}` where Outcome is `ok` or `error`. A policy deny
is an `error`; its text reaches the program, so the footer need not
distinguish it.

Helper results are **not** cut to the model's 30 000-byte budget: the data
is for the program, not the context window. The helper puts an output limit
on the context (`tool.WithOutputLimit(ctx, kernel.helper_max_bytes)`), and
the three places that cap output honour it: `Registry.Run`'s truncation,
`web_fetch`'s body `LimitReader`, and `shell_exec`'s capWriter. Without a
limit on the context they behave exactly as today.

### 3.3 `internal/tool/gorun` — the `go_run` tool

```json
{"type":"object","properties":{
 "code":{"type":"string","description":"A complete Go program: package main, imports, func main. Print the result to stdout."},
 "timeout_seconds":{"type":"integer","description":"Interpreter time budget; default 60, maximum 300."}},
 "required":["code"]}
```

`ReadOnly()` is false. The tool holds its `Runner` through `Bind(r)`, called
in `buildTools` once the guard exists (the guard wraps the registry that
holds the tool, so construction order cannot close the cycle). An unbound
tool returns an error.

The returned content is stdout, then a footer when any helper ran:

```
--- calls: web_fetch ok · fs_write error ---
```

On a compile error, panic or timeout, the tool returns an error whose text
is the kernel's message followed by any stdout produced, so the model can
fix the program and retry in one hop.

### 3.4 Configuration

```toml
[kernel]
mode = "code"            # "code" | "tools"
timeout_seconds = 60     # default interpreter budget per go_run
max_timeout_seconds = 300
ceiling_seconds = 1800   # wall-clock hard stop, approvals included
helper_max_bytes = 4194304
```

Defaults are as shown. `config.Load` rejects any `mode` other than `code`
or `tools`, and non-positive numbers are replaced by the defaults.
`go_run` joins the default `Allow` list.

### 3.5 Agent: code mode

- **Tool specs.** In `Agent.loop`, when `Cfg.Kernel.Mode == "code"`, the
  request's tools are filtered to the `go_run` spec only. `Agent.Tools`
  stays the guard, so `buildServer`'s `*policy.Guard` assertion and every
  other user of the guard are untouched.
- **Prompt.** `Snapshot` gains a `Kernel string` field. In code mode,
  `Agent.Snapshot` fills it with `kernel.Reference(a.Tools.Specs())`, and
  `Assemble` adds it to the system blocks after the skills index and before
  the facts, inside the cached prefix.
- **Sub-agents** run through the same `Agent`, so they inherit the mode.
- **Profiles.** The session (profile and workspace) is already on the
  context, so every helper call is judged under the caller's profile: a
  Discord session calling `spore.Call("mcp__…")` hits the remote deny.

### 3.6 The prompt block (`kernel.Reference`)

A `## Acting through go_run` section with:

1. The contract: write one complete `package main` program that does the
   whole task, including every lookup, and prints the final result.
   Stdout is all you see. Prefer one program over several; if a program
   fails, fix it and run it again.
2. The stdlib allowlist, as a list.
3. The typed `spore.*` signatures from §3.2, one line each.
4. A generated `spore.Call` catalogue: for every spec from the runner other
   than `go_run` and the tools the typed helpers already cover, its name,
   description and schema (compact JSON). MCP tools from the dynamic source
   are included because they come from `Specs()`.

The output is deterministic (specs are already sorted by name), so the
cached prefix is byte-stable across turns while the tool set is unchanged.

## 4. Behaviour

### 4.1 One turn in code mode

1. The model sees the system prompt with the `go_run` section, and the tool
   list `[go_run]`.
2. It emits `go_run{code: …}`. The loop runs it through the guard (allowed),
   emits `tool_call`, and opens the `go_run` span.
3. The kernel runs the program. `spore.Fetch` sends a `web_fetch` call
   through the guard (allowed by `web_*`).
4. The program prints; `go_run` returns stdout and the footer.
5. The model answers.

### 4.2 Time

The run has two bounds:

- **Budget** (`timeout_seconds`, default 60, capped at
  `max_timeout_seconds`): counts only time with no `spore.*` helper in
  flight. A watchdog pauses the budget clock while at least one helper is
  running and resumes it when none is. Helpers are bounded by their own
  tools: `shell_exec` by its timeout, `web_fetch` by its 30 s client
  timeout, and an `ask` by the approval timeout. This is what keeps a
  60-second program alive while the user reads a Discord approval.
- **Ceiling** (`ceiling_seconds`, default 1800): wall clock, no pauses. It
  guards against a program that keeps one goroutine in a helper forever
  while another spins.

When either bound passes, the run context is cancelled, which stops the
interpreter and every in-flight helper.

### 4.3 Approvals mid-program

A helper whose tool resolves to `ask` blocks in the guard exactly as a
normal call does. The request shows the real tool and arguments. The
program continues when the user answers; a decline returns an `error` to
the program.

## 5. Testing

**`internal/kernel`**
- Print-and-return; stdout and stderr both captured.
- Each of `os`, `os/exec`, `net`, `unsafe`, `syscall`, `reflect`, `sync`
  is refused with the rewritten message, and the message does not contain
  "GOPATH".
- A source import (`github.com/x/y`) fails with `$GOPATH` set to a
  directory that contains that package's source.
- `for {}` and a goroutine spinning under `select{}` time out; afterwards
  `runtime.NumGoroutine` returns to its baseline.
- `time.Sleep` is undefined in a program.
- Output past `MaxOutput` is truncated and flagged.
- A panic returns an error carrying its line.
- Budget pause: a helper that blocks 2 s under a 1 s budget completes.
- Ceiling: the same helper under a 1 s ceiling is cancelled.
- Bridge: each typed helper sends the right tool name and JSON; `Call`
  passes arguments through; a deny result becomes a program-visible error;
  `Call("go_run")` is refused; concurrent helpers from goroutines pass
  `-race`.
- `Reference` golden test, including a synthetic `mcp__` spec.

**Policy integration**, built on `config.Load` (not `config.Default()`,
which lacks the baseline deny): with the real guard and a stub registry,
`spore.ReadFile("~/.ssh/id_ed25519")` and `spore.Shell("sudo ls")` are
denied; `spore.Call("mcp__x__y", …)` under the remote profile is denied;
`spore.WriteFile` under `ask` reaches the approver and continues on allow.

**`internal/tool`**: `Registry.Run` honours `WithOutputLimit`; without it the
cut is unchanged. `web_fetch` and `shell_exec` likewise.

**Config**: defaults; an unknown `kernel.mode` fails `Load`; `go_run` is in
the default allow list.

**Agent**: in code mode the request offers only `go_run` and the system
prompt carries the section; in tools mode the request is unchanged (the
existing agent tests pass untouched). End-to-end with a fake provider that
emits one `go_run` whose program calls a stubbed `web_fetch` and prints:
the turn ends after two provider calls.

**Gates**: `make test`, lint, `-race`, and `go mod tidy` leaving no diff.
**Manual check before merge**: "what is the weather in London?" in the TUI
and through Discord, and one program that writes a file, to see the
approval arrive mid-program.
