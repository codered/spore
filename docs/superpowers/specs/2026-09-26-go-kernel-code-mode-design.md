# spore — Go kernel and code mode

**Date:** 2026-09-26
**Status:** approved (brainstorming dialogue); user asked to write, commit and
implement without further gates. §2.1 records one change made after approval.

## 1. What this adds

Today the model acts one tool call at a time: fetch a URL, read the result,
decide, call the next tool. A question like "what is the weather in London?"
costs three or four model round trips.

This spec adds a **Go kernel**: the model writes one complete Go program that
finishes the task, spore runs it under the
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
| Runtime | yaegi with a curated stdlib allowlist; not `go run` |
| State across `go_run` calls | None: a fresh interpreter per call |
| Policy | Every `spore.*` call goes through the existing `policy.Guard`, unchanged |

### Out of scope

- Persistent kernels (Jupyter-style state across calls).
- Third-party imports, and any import outside the allowlist in §3.2.
- A memory cap on the kernel process. A runaway program is bounded by its
  timeout; the OS may kill it earlier. It cannot take the daemon with it.
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

### 2.1 The interpreter runs in a child process

The approved design ran yaegi in-process. Probing yaegi v0.16.1 while
writing this spec showed that it cannot be contained in-process:

- A panic in a goroutine the program starts is re-panicked on a native
  goroutine and **terminates the host process**.
- Unbounded recursion is a Go `fatal error: stack overflow`, which no
  `recover` catches.
- Goroutines left running after `main` returns keep running; cancelling
  the context after `EvalWithContext` has returned does not stop them.
- A blocked native call (`time.Sleep`, `sync.WaitGroup.Wait`) outlives
  cancellation.

Any of these in the daemon would kill every session and the Discord bridge.
So the kernel runs each program in a **child process**: the spore binary
re-executed with `SPORE_KERNEL_CHILD=1`. The child runs yaegi and nothing
else. Every `spore.*` call is a request over a pipe to the parent, where the
guard judges it. The security property is unchanged: the child can reach
only the allowlisted stdlib and `spore.*`. A crash, a timeout or a leftover
goroutine ends with the child, which the parent kills outright.

## 3. Components

### 3.1 `internal/kernel` — parent side

```go
type Options struct {
    Timeout   time.Duration // interpreter time budget, see §4.2
    Ceiling   time.Duration // wall-clock hard stop, see §4.2
    MaxOutput int           // program output byte cap
    HelperMax int           // byte cap for one helper result, see §3.3
}

type Call struct {
    Tool    string
    Outcome string // "ok" or "error"
}

type Result struct {
    Output    string // program stdout and stderr, in write order
    Truncated bool   // output passed MaxOutput
    Calls     []Call // every spore.* call, in order of completion
}

// Runner is the one method of policy.Guard the kernel needs. Declared here
// so kernel does not import policy.
type Runner interface {
    Run(ctx context.Context, call provider.Block) provider.Block
}

func Run(ctx context.Context, src string, r Runner, opt Options) (Result, error)
```

`Run`:

1. Parses `src` with `go/parser`. A parse error is returned as is (it
   carries `line:col`). A program whose package is not `main` or that has
   no `func main()` is refused with a message saying so.
2. Starts the child: `os.Executable()`, env `SPORE_KERNEL_CHILD=1` only,
   two extra pipes (fd 3: parent→child, fd 4: child→parent), stderr
   captured to a 64 KiB tail buffer, stdin and stdout set to nothing.
3. Sends the `run` message, then serves `call` messages until `done`, a
   child exit, or a time bound. Each `call` is served on its own goroutine
   through `Runner.Run`, so a program's goroutines can call helpers
   concurrently.
4. Kills the child on every path out (`Process.Kill`, then `Wait`).

When the child exits without `done`, the error is
`kernel crashed: <last 2 KiB of stderr>` and the output so far is kept.

### 3.2 `internal/kernel` — child side

`kernel.IsChild()` reports `SPORE_KERNEL_CHILD=1`. `cmd/spore`'s `main`
calls `os.Exit(kernel.ChildMain())` before anything else when it is true,
so a kernel child never loads config, opens the store, or starts tracing.

`ChildMain` reads the `run` message and builds a fresh `interp.New` with:

- `Stdout` and `Stderr` both going to a writer that sends each write as an
  `out` message, stopping after `max_output` bytes and flagging truncation.
  Streaming means a program that times out or crashes still returns what
  it printed.
- `GoPath` set to a path that does not exist and `SourcecodeFilesystem` set
  to an empty `fstest.MapFS`, so no import can load source from disk.
- `Env` empty, `Args` `["main"]`, `Unrestricted` false.
- `Use` given only the allowlisted stdlib symbols and the `spore` package.

It evaluates the program, sends `done` with any error, and returns 0.

**Stdlib allowlist** (keys of `stdlib.Symbols`, without the `/name`
suffix): `bytes`, `encoding/base64`, `encoding/csv`, `encoding/hex`,
`encoding/json`, `errors`, `fmt`, `html`, `math`, `math/rand`, `net/url`,
`regexp`, `sort`, `strconv`, `strings`, `sync`, `sync/atomic`,
`text/tabwriter`, `text/template`, `time`, `unicode`, `unicode/utf8`.

**Error messages.** A refused import produces yaegi's message, which
mentions GOPATH. The child rewrites any `import "X" error` to:
`package "X" is not available in go_run; allowed: <list>. Use spore.* for files, shell and network.`
Compile errors and panics keep yaegi's `line:col` prefix.

### 3.3 The `spore` package

Built in the child. Each helper sends
`{"type":"call","id":n,"tool":…,"args":{…}}` and blocks for the matching
`result`. In the parent, the call becomes
`provider.Block{Type: BlockToolUse, ID: "gorun_<run>_<n>", Name: tool, Input: args}`
through `Runner.Run`, where `<run>` is 8 random hex bytes per run so ids
never repeat within a session. An error result (policy deny, declined
approval, tool failure) becomes a Go `error` whose text is the result
content.

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

Empty optional arguments are omitted (`count` 0, `glob` "", `path` "" for
`List`). The parent refuses `go_run` as a helper tool with an error result;
nested kernels buy nothing and would break the time accounting.

In the parent, each helper call opens a child span with
`sporetrace.StartTool` on the run context, so a trace reads
`go_run → web_fetch → …`, and appends `Call{Tool, Outcome}`.

**Helper results are not cut to the model's 30 000-byte budget**: the data
is for the program, not the context window. The parent puts a limit on the
helper's context with `tool.WithOutputLimit(ctx, HelperMax)`, and the three
places that cap output honour it: `Registry.Run`'s truncation, `web_fetch`'s
body `LimitReader`, and `shell_exec`'s capWriter. Without a limit on the
context each behaves exactly as today.

### 3.4 Wire protocol

Newline-delimited JSON, one object per line, `type` first:

| Direction | `type` | Fields |
|---|---|---|
| parent→child | `run` | `code`, `max_output` |
| child→parent | `out` | `data` |
| child→parent | `call` | `id`, `tool`, `args` (object) |
| parent→child | `result` | `id`, `content`, `is_error` |
| child→parent | `done` | `error` (empty on success), `truncated` |

Writers on either side hold a mutex per line.

### 3.5 `internal/tool/gorun` — the `go_run` tool

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

The returned content is the program output, then a footer when any helper
ran:

```
--- calls: web_fetch ok · fs_write error ---
```

On a parse, compile or runtime error, a crash or a timeout, the tool returns
an error whose text is the kernel's message followed by the output so far
and the footer, so the model can fix the program and retry in one hop.

### 3.6 Configuration and policy

```toml
[kernel]
mode = "code"            # "code" | "tools"
timeout_seconds = 60     # default interpreter budget per go_run
max_timeout_seconds = 300
ceiling_seconds = 1800   # wall-clock hard stop, approvals included
helper_max_bytes = 4194304
```

Defaults are as shown; `config.Load` fills zero values from them.
`Validate` rejects any `mode` other than `code` or `tools` and negative
numbers.

**`go_run`'s fallback decision is allow.** `go_run` has no effect of its
own; everything it does is judged per helper. So when no allow or ask rule
matches `go_run`, `Engine.Evaluate` returns allow (rule
`policy.kernel`) instead of the profile default. Deny rules and explicit
allow/ask rules for `go_run` still win. This matters because a profile that
names its own allow list (such as a remote profile written before this
change) would otherwise put an approval on every program.

### 3.7 Agent: code mode

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

### 3.8 The prompt block (`kernel.Reference`)

A `## Acting through go_run` section with:

1. The contract: write one complete `package main` program that does the
   whole task, including every lookup, and prints the final result.
   Output is all you see. Prefer one program over several; if a program
   fails, fix it and run it again.
2. The stdlib allowlist, as a list.
3. The typed `spore.*` signatures from §3.3, one line each.
4. A generated `spore.Call` catalogue: every spec from the runner except
   `go_run` and the tools the typed helpers cover, with its name,
   description and schema (compact JSON). MCP tools from the dynamic source
   are included because they come from `Specs()`.

The output is deterministic (specs arrive sorted by name), so the cached
prefix is byte-stable while the tool set is unchanged.

## 4. Behaviour

### 4.1 One turn in code mode

1. The model sees the system prompt with the `go_run` section, and the tool
   list `[go_run]`.
2. It emits `go_run{code: …}`. The loop runs it through the guard (allowed),
   emits `tool_call`, and opens the `go_run` span.
3. The kernel starts a child and sends the program. `spore.Fetch` arrives
   as a `call`; the parent sends `web_fetch` through the guard (allowed by
   `web_*`) and returns the result.
4. The program prints; the child sends `done`; `go_run` returns the output
   and the footer.
5. The model answers.

### 4.2 Time

- **Budget** (`timeout_seconds`, default 60, capped at
  `max_timeout_seconds`): counts only time with no helper call in flight.
  The parent pauses the budget clock while at least one call is being
  served and resumes it when none is. Helpers are bounded by their own
  tools: `shell_exec` by its timeout, `web_fetch` by its 30 s client
  timeout, an `ask` by the approval timeout. This keeps a 60-second
  program alive while the user reads a Discord approval.
- **Ceiling** (`ceiling_seconds`, default 1800): wall clock, no pauses.

When either bound passes, or the turn's context ends, the parent cancels
the in-flight helper calls and kills the child. The error is
`go_run timed out after <budget>` (or `… hit the <ceiling> ceiling`).

### 4.3 Approvals mid-program

A helper whose tool resolves to `ask` blocks in the guard exactly as a
normal call does. The request shows the real tool and arguments. The
program continues when the user answers; a decline returns an `error` to
the program.

## 5. Testing

The kernel tests re-execute the test binary as the child: `TestMain` calls
`os.Exit(kernel.ChildMain())` when `kernel.IsChild()`.

**`internal/kernel`**
- A program that prints returns its output; stderr is captured too.
- Each of `os`, `os/exec`, `net`, `unsafe`, `syscall`, `reflect` is refused
  with the rewritten message, which does not contain "GOPATH".
- A source import (`github.com/x/y`) fails with `$GOPATH` pointing at a
  directory that holds that package's source.
- `package foo` and a program with no `main` are refused before a child
  starts; a syntax error returns its line.
- `for {}` times out at the budget.
- A goroutine panic, unbounded recursion and a nil-map write each return
  an error, and the test process survives.
- Output past `MaxOutput` is truncated and flagged; output printed before
  a timeout is returned.
- Budget pause: a helper that blocks 2 s under a 1 s budget completes.
- Ceiling: the same helper under a 1 s ceiling is cancelled.
- A cancelled parent context kills the child promptly.
- Each typed helper sends the right tool name and JSON; `Call` passes its
  arguments through; an error result is visible to the program as an
  error; `go_run` as a helper tool is refused; helpers from ten goroutines
  complete under `-race`.
- `Reference` golden test, including a synthetic `mcp__` spec.

**Policy**, built on `config.Load` (not `config.Default()`, which lacks the
baseline deny):
- `go_run` is allowed under the local profile and under a remote profile
  whose allow list does not name it; an explicit `ask` or `deny` for
  `go_run` still wins.
- End to end through the real guard: `spore.ReadFile("~/.ssh/id_ed25519")`
  and `spore.Shell("sudo ls")` are denied; `spore.Call("mcp__x__y", …)`
  under the remote profile is denied; `spore.WriteFile` under `ask` reaches
  the approver and the program continues on allow.

**`internal/tool`**: `Registry.Run` honours `WithOutputLimit`; without it the
cut is unchanged. `web_fetch` and `shell_exec` likewise.

**Config**: defaults fill in; an unknown `kernel.mode` fails `Validate`.

**Agent**: in code mode the request offers only `go_run` and the system
prompt carries the section; in tools mode the request is unchanged (the
existing agent tests pass untouched).

**Gates**: `make test`, lint, `-race`, and `go mod tidy` leaving no diff.
**Manual check before merge**: "what is the weather in London?" in the TUI
and through Discord, and one program that writes a file, to see the
approval arrive mid-program.
