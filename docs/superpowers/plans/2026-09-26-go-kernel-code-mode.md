# Go kernel and code mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the model finish a task with one Go program run under yaegi in a child process, with every `spore.*` call judged by the existing policy guard, and make that the default "code mode".

**Architecture:** `internal/kernel` owns both sides of a child process: the parent (`kernel.Run`) re-executes the spore binary, streams the program to it and serves helper calls through a `Runner` (the guard); the child (`kernel.ChildMain`) runs yaegi with an allowlisted stdlib and a `spore` package whose functions are RPCs. `internal/tool/gorun` wraps `kernel.Run` as the `go_run` tool. The agent filters its tool specs to `go_run` and adds a generated API reference to the system prompt when `kernel.mode = "code"`.

**Tech Stack:** Go 1.26, `github.com/traefik/yaegi` v0.16.1, existing spore packages (`provider`, `policy`, `tool`, `trace`, `config`, `agent`).

**Spec:** `docs/superpowers/specs/2026-09-26-go-kernel-code-mode-design.md`

## Global Constraints

- The kernel package must not import `internal/policy` or `internal/agent` (it declares its own `Runner`).
- The child env is exactly `SPORE_KERNEL_CHILD=1`; the child never loads config, the store or tracing.
- Stdlib allowlist, verbatim: `bytes`, `encoding/base64`, `encoding/csv`, `encoding/hex`, `encoding/json`, `errors`, `fmt`, `html`, `math`, `math/rand`, `net/url`, `regexp`, `sort`, `strconv`, `strings`, `sync`, `sync/atomic`, `text/tabwriter`, `text/template`, `time`, `unicode`, `unicode/utf8`.
- Config defaults: `mode = "code"`, `timeout_seconds = 60`, `max_timeout_seconds = 300`, `ceiling_seconds = 1800`, `helper_max_bytes = 4194304`.
- Helper call ids: `gorun_<16 hex chars>_<n>`.
- Tools mode must leave every existing test passing untouched.
- Policy tests that assert deny behaviour are built on `config.Load`, never `config.Default()`.

## Review Focus

1. A model-written program that panics in a goroutine, recurses forever, or spins after `main` returns: the daemon must survive and the child must be gone (test: Task 4 crash tests, plus `ps` check in the manual step).
2. A remote-profile config written before this change (explicit allow list without `go_run`): programs must still run without an approval (test: Task 3 `TestKernelToolFallsBackToAllowUnderAnExplicitProfile`).
3. A helper waiting on a human approval longer than the program's budget: the program must not time out (test: Task 4 `TestBudgetPausesWhileAHelperIsInFlight`).
4. A large helper result (e.g. a 200 KB JSON API response): the program must receive it whole, not cut at 30 000 bytes (test: Task 1 limit tests + Task 4 `TestHelperResultIsNotCutToTheModelBudget`).
5. The model importing `os` or `net/http` out of habit: the error must tell it what to use instead and must not mention GOPATH (test: Task 4 `TestForbiddenImportsAreRefusedWithAUsefulMessage`).

---

### Task 1: Output limit on the context

**Files:**
- Modify: `internal/tool/tool.go` (add `WithOutputLimit`, `OutputLimit`; use in `Registry.Run`)
- Modify: `internal/tool/web/fetch.go` (LimitReader honours the context limit)
- Modify: `internal/tool/shell/shell.go` (capWriter limit honours the context limit)
- Test: `internal/tool/tool_test.go`, `internal/tool/web/web_test.go`, `internal/tool/shell/shell_test.go`

**Interfaces:**
- Produces: `func WithOutputLimit(ctx context.Context, n int) context.Context`, `func OutputLimit(ctx context.Context, def int) int` (returns `def` when no positive limit is attached).

- [ ] **Step 1: Failing tests.** In `tool_test.go`: a tool returning 100 bytes under a registry with `maxOutput` 10 is truncated without a limit and returned whole under `WithOutputLimit(ctx, 1000)`. In `web_test.go`: an httptest server serving 5000 bytes of `text/plain` to a fetch tool built with `maxBytes` 100 returns 100 bytes without a limit and 5000 with `WithOutputLimit(ctx, 10000)`. In `shell_test.go`: `head -c 5000 /dev/zero | tr '\0' a` under `maxOutput` 100 returns ≤ 100+marker bytes without a limit and 5000 `a`s with the limit.
- [ ] **Step 2:** `go test ./internal/tool/...` — the new tests fail (undefined `WithOutputLimit`).
- [ ] **Step 3: Implement.**

```go
type outputLimitKey struct{}

// WithOutputLimit raises (or lowers) the byte cap for tool results produced
// under ctx. The Go kernel uses it so a helper call hands the program the
// whole result: the default cap exists to protect the model's context
// window, and a helper's result never reaches the model directly.
func WithOutputLimit(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, outputLimitKey{}, n)
}

// OutputLimit returns the cap attached by WithOutputLimit, or def.
func OutputLimit(ctx context.Context, def int) int {
	if n, ok := ctx.Value(outputLimitKey{}).(int); ok && n > 0 {
		return n
	}
	return def
}
```

In `Registry.Run` replace `r.maxOutput` with `limit := OutputLimit(ctx, r.maxOutput)`. In `fetchTool.Call` use `int64(tool.OutputLimit(ctx, f.maxBytes))`. In `execTool.Call` build `capWriter{limit: tool.OutputLimit(ctx, t.maxOutput)}` wherever `t.maxOutput` is used.

- [ ] **Step 4:** `go test ./internal/tool/...` passes.
- [ ] **Step 5:** Commit `tool: let the context raise the output cap for one call`.

### Task 2: `[kernel]` configuration

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces: `type KernelConfig struct { Mode string; TimeoutSeconds, MaxTimeoutSeconds, CeilingSeconds, HelperMaxBytes int }` with toml keys `mode`, `timeout_seconds`, `max_timeout_seconds`, `ceiling_seconds`, `helper_max_bytes`; `Config.Kernel KernelConfig` (`toml:"kernel"`); constants `KernelModeCode = "code"`, `KernelModeTools = "tools"`.

- [ ] **Step 1: Failing tests.** `Load` on a minimal config yields the five defaults; `[kernel] mode = "tools"` round-trips; `mode = "python"` makes `Validate` fail with a message naming `kernel.mode`; `timeout_seconds = -1` fails `Validate`.
- [ ] **Step 2:** Run, see failures.
- [ ] **Step 3: Implement.** Add the struct and field; set defaults in `Default()`; in `Load` fill each zero value from `Default().Kernel`; in `Validate`:

```go
switch c.Kernel.Mode {
case KernelModeCode, KernelModeTools:
default:
	return fmt.Errorf("kernel.mode must be %s or %s, got %q", KernelModeCode, KernelModeTools, c.Kernel.Mode)
}
if c.Kernel.TimeoutSeconds < 0 || c.Kernel.MaxTimeoutSeconds < 0 || c.Kernel.CeilingSeconds < 0 || c.Kernel.HelperMaxBytes < 0 {
	return fmt.Errorf("kernel: timeouts and helper_max_bytes must not be negative")
}
```

(Empty `Mode` is only possible when a caller builds a Config by hand without `Default()`; `Load` fills it, so the switch also rejects "".)

- [ ] **Step 4:** `go test ./internal/config/...` passes.
- [ ] **Step 5:** Commit `config: add the [kernel] section`.

### Task 3: `go_run` falls back to allow

**Files:**
- Modify: `internal/policy/engine.go`
- Test: `internal/policy/engine_test.go`

**Interfaces:**
- Produces: `const KernelTool = "go_run"` in package policy.

- [ ] **Step 1: Failing tests** (all through `config.Load` on a temp file):
  - `TestKernelToolAllowedByDefault`: minimal config, local profile → allow, rule `policy.kernel`.
  - `TestKernelToolFallsBackToAllowUnderAnExplicitProfile`: `[policy.profile.remote] default="ask"`, `allow=["fs_read"]` → remote `go_run` allow.
  - `TestExplicitRulesForKernelToolStillWin`: `[policy] ask=["go_run"]` → ask; `deny=["go_run"]` → deny.
- [ ] **Step 2:** Run, see failures.
- [ ] **Step 3: Implement** — replace the final `return` in `Evaluate`:

```go
	// go_run has no effect of its own: everything a program does arrives
	// as a separate call through the guard and is judged there. Falling
	// back to the profile default would put an approval on every program
	// under any profile that lists its own allow rules.
	if c.Tool == KernelTool {
		return Result{Decision: DecisionAllow, Rule: "policy.kernel"}
	}
	return Result{Decision: rs.fallback, Rule: "policy.default"}
```

- [ ] **Step 4:** `go test ./internal/policy/...` passes.
- [ ] **Step 5:** Commit `policy: let go_run fall back to allow`.

### Task 4: `internal/kernel` — protocol, child and parent

**Files:**
- Create: `internal/kernel/protocol.go` (message type, line codec)
- Create: `internal/kernel/child.go` (`IsChild`, `ChildMain`, surface, `spore` package, import-error rewrite)
- Create: `internal/kernel/kernel.go` (`Options`, `Result`, `Call`, `Runner`, `Run`, budget clock)
- Test: `internal/kernel/kernel_test.go` (with `TestMain`)
- Modify: `go.mod`, `go.sum` (`go get github.com/traefik/yaegi@v0.16.1`)

**Interfaces:**
- Consumes: `tool.WithOutputLimit` (Task 1), `sporetrace.StartTool`, `sporetrace.RecordToolResult`, `provider.Block`.
- Produces: `kernel.Run(ctx, src string, r Runner, opt Options) (Result, error)`, `kernel.IsChild() bool`, `kernel.ChildMain() int`, `kernel.Allowed []string` (the allowlist, sorted), `kernel.Helpers` (typed helper table used by `Reference`), `kernel.ToolName = "go_run"`.

Protocol message (both directions):

```go
type msg struct {
	Type      string          `json:"type"`
	Code      string          `json:"code,omitempty"`
	MaxOutput int             `json:"max_output,omitempty"`
	Data      string          `json:"data,omitempty"`
	ID        int64           `json:"id,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Error     string          `json:"error,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
}
```

Budget clock (parent):

```go
// clock measures interpreter time: it runs only while no helper call is in
// flight, so a program is not charged for a human reading an approval.
type clock struct {
	mu       sync.Mutex
	inFlight int
	used     time.Duration
	since    time.Time
}

func (c *clock) enter() {
	c.mu.Lock(); defer c.mu.Unlock()
	if c.inFlight == 0 { c.used += time.Since(c.since) }
	c.inFlight++
}

func (c *clock) exit() {
	c.mu.Lock(); defer c.mu.Unlock()
	c.inFlight--
	if c.inFlight == 0 { c.since = time.Now() }
}

func (c *clock) elapsed() time.Duration {
	c.mu.Lock(); defer c.mu.Unlock()
	if c.inFlight > 0 { return c.used }
	return c.used + time.Since(c.since)
}
```

A watchdog goroutine ticks every 10 ms and cancels the run with cause `errBudget` when `elapsed() > Timeout`; the ceiling is `context.WithTimeoutCause(ctx, Ceiling, errCeiling)`.

- [ ] **Step 1: Add yaegi** — `go get github.com/traefik/yaegi@v0.16.1`.
- [ ] **Step 2: Failing tests** in `kernel_test.go` (package `kernel`), with:

```go
func TestMain(m *testing.M) {
	if IsChild() {
		os.Exit(ChildMain())
	}
	os.Exit(m.Run())
}
```

and a `fakeRunner` recording calls and answering from a `func(provider.Block) provider.Block`. Tests:
  - `TestProgramOutputIsReturned` — `fmt.Println("hi")` → `Output == "hi\n"`.
  - `TestForbiddenImportsAreRefusedWithAUsefulMessage` — for each of `os`, `os/exec`, `net`, `net/http`, `unsafe`, `syscall`, `reflect`: error contains `package "<p>" is not available in go_run` and not `GOPATH`.
  - `TestSourceImportsCannotBeLoaded` — `t.Setenv("GOPATH", dir)` with `dir/src/github.com/x/y/y.go` present; importing it fails.
  - `TestProgramMustBePackageMainWithMain` — `package foo` and a main-less program return an error before any child starts; a syntax error's text contains `:` line info.
  - `TestInfiniteLoopTimesOut` — `for {}` with `Timeout: 300ms` returns an error containing `timed out`, within 3 s.
  - `TestCrashesDoNotKillTheParent` — goroutine panic, `func f() int { return f() + 1 }`, and a nil-map write each return an error; the test keeps running.
  - `TestOutputIsCappedAndKeptOnTimeout` — printing 10 000 bytes with `MaxOutput: 100` → `Truncated`, `len(Output) <= 100`; a program that prints `before` then loops forever returns `before` with its timeout error.
  - `TestBudgetPausesWhileAHelperIsInFlight` — runner sleeps 2 s; `Timeout: 1s`, `Ceiling: 10s`; program calls `spore.Fetch` then prints → no error.
  - `TestCeilingStopsAHelperThatNeverReturns` — runner blocks on `ctx.Done()`; `Ceiling: 1s` → error containing `ceiling` within 3 s.
  - `TestCancelledContextKillsTheChild` — cancel the parent context after 200 ms of `for {}` → returns within 2 s.
  - `TestHelpersSendTheRightCalls` — table over each typed helper and `Call`: the runner sees the tool name and args JSON exactly as in the spec table (empty optionals omitted).
  - `TestHelperErrorsReachTheProgram` — runner returns `IsError: true, Content: "denied by rule x"`; program prints `err.Error()` → output contains `denied by rule x`; `Result.Calls[0].Outcome == "error"`.
  - `TestKernelToolIsRefusedAsAHelper` — `spore.Call("go_run", nil)` → program sees an error; runner never called.
  - `TestHelperResultIsNotCutToTheModelBudget` — runner asserts `tool.OutputLimit(ctx, 0) == opt.HelperMax` and returns 200 000 bytes; program prints `len(s)` → `200000`.
  - `TestConcurrentHelpers` — ten goroutines (`sync.WaitGroup`) each call `spore.Fetch`; ten calls recorded; run under `-race`.
  - `TestCallIDsAreUniqueAcrossRuns` — two runs; all ids distinct and match `^gorun_[0-9a-f]{16}_\d+$`.
- [ ] **Step 3:** `go test ./internal/kernel/` fails to compile.
- [ ] **Step 4: Implement** `protocol.go`, `child.go`, `kernel.go` per spec §3.1–3.4 using the message and clock above. Child specifics:
  - Surface: `for _, p := range Allowed { key := p + "/" + path.Base(p); ex[key] = stdlib.Symbols[key] }`, plus `ex["spore/spore"] = sporeSymbols(...)`.
  - Import rewrite regexp: `import "([^"]+)" error: [^\n]*`.
  - `ChildMain` reads fd 3 via `os.NewFile(3, "in")`, writes fd 4 via `os.NewFile(4, "out")`; a reader goroutine routes `result` messages to per-id channels.
  - Parent `exec.Cmd`: `ExtraFiles = []*os.File{childIn, childOut}`, `Env = []string{"SPORE_KERNEL_CHILD=1"}`, `Stderr` = tail buffer; always `Process.Kill()` + `Wait()` on return.
  - Parent validation with `go/parser.ParseFile(fset, "main.go", src, 0)`: package name `main`, a top-level `FuncDecl` named `main` with no receiver.
- [ ] **Step 5:** `go test -race ./internal/kernel/` passes.
- [ ] **Step 6:** Commit `kernel: run Go programs under yaegi in a child process`.

### Task 5: `kernel.Reference`

**Files:**
- Create: `internal/kernel/reference.go`
- Test: `internal/kernel/reference_test.go`, `internal/kernel/testdata/reference.golden`

**Interfaces:**
- Consumes: `Allowed`, `Helpers` (Task 4), `provider.ToolSpec`.
- Produces: `func Reference(specs []provider.ToolSpec) string`.

- [ ] **Step 1: Failing test** — `Reference` over specs `{fs_read, go_run, mcp__gh__list_prs, schedule_list}` matches the golden file (regenerate with `-update`); asserts it contains `mcp__gh__list_prs` and `schedule_list`, and that the `spore.Call` catalogue lists neither `go_run` nor `fs_read`.
- [ ] **Step 2:** Run, fail.
- [ ] **Step 3: Implement** the four parts of spec §3.8; schemas compacted with `json.Compact`.
- [ ] **Step 4:** Pass.
- [ ] **Step 5:** Commit `kernel: generate the go_run prompt reference`.

### Task 6: `go_run` tool and wiring

**Files:**
- Create: `internal/tool/gorun/gorun.go`
- Test: `internal/tool/gorun/gorun_test.go` (own `TestMain` re-exec, as Task 4)
- Modify: `cmd/spore/main.go` (child hook first thing in `main`)
- Modify: `cmd/spore/wire.go` (register, `Bind(guard)`)

**Interfaces:**
- Consumes: `kernel.Run`, `kernel.Options`, `config.KernelConfig`.
- Produces: `func New(cfg config.KernelConfig, maxOutput int) *Tool`, `func (t *Tool) Bind(r kernel.Runner)`.

- [ ] **Step 1: Failing tests** — unbound tool errors; a bound tool running `fmt.Println("ok")` returns `ok\n`; a program calling `spore.Fetch` gets footer `--- calls: web_fetch ok ---`; `timeout_seconds: 9999` is clamped to `MaxTimeoutSeconds` (assert via a 1 s max and a `for {}` program returning within 4 s); a compile error returns an error that contains the program output so far.
- [ ] **Step 2:** Run, fail.
- [ ] **Step 3: Implement.** Content is `res.Output` + footer; on error, `fmt.Errorf("%v\n%s%s", err, res.Output, footer)`. In `main.go`:

```go
func main() {
	if kernel.IsChild() {
		os.Exit(kernel.ChildMain())
	}
	...
```

In `buildTools`: `gr := gorun.New(cfg.Kernel, cfg.Policy.MaxOutput)`, append to `tools`, and after `g := policy.NewGuard(...)`: `gr.Bind(g)`.
- [ ] **Step 4:** `go test ./internal/tool/gorun/ ./cmd/spore/` passes.
- [ ] **Step 5:** Commit `gorun: add the go_run tool and wire it to the guard`.

### Task 7: Agent code mode

**Files:**
- Modify: `internal/agent/context.go` (`Snapshot.Kernel`, `Assemble`)
- Modify: `internal/agent/agent.go` (spec filter in `loop`, `Snapshot` fills `Kernel`)
- Test: `internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `kernel.Reference`, `kernel.ToolName`, `config.KernelModeCode`.

- [ ] **Step 1: Failing tests.** `harness` uses `config.Default()` (mode code), so existing tests that assert tool specs need tools mode: set `cfg.Kernel.Mode = config.KernelModeTools` inside `harness` so the existing suite runs unchanged, and add a `codeHarness` variant. New tests:
  - `TestCodeModeOffersOnlyGoRun` — fake tools with specs `{fs_read, go_run}`; request `Tools` is exactly `[go_run]`; system text contains `## Acting through go_run` and `fs_read` (in the reference).
  - `TestToolsModeSendsEveryToolAndNoReference` — same fake tools, tools mode; both specs sent, no reference section.
- [ ] **Step 2:** Run, fail.
- [ ] **Step 3: Implement.** In `loop`:

```go
if a.Tools != nil {
	req.Tools = a.Tools.Specs()
	if a.Cfg.Kernel.Mode == config.KernelModeCode {
		req.Tools = onlyKernel(req.Tools)
	}
}
```

with `onlyKernel` keeping specs named `kernel.ToolName`. In `Snapshot`, when code mode and `a.Tools != nil`, `snap.Kernel = kernel.Reference(a.Tools.Specs())`. In `Assemble`, `add(snap.Kernel)` after `skillsSection`.
- [ ] **Step 4:** `go test ./internal/agent/...` passes (all pre-existing tests included).
- [ ] **Step 5:** Commit `agent: offer only go_run in code mode and describe the spore API`.

### Task 8: Policy end to end through the kernel

**Files:**
- Create: `internal/kernel/policy_test.go` (package `kernel_test`, reuses `TestMain` from Task 4 by living in the same directory — `TestMain` must be in an `_test.go` file of package `kernel`, which applies to the whole test binary)

**Interfaces:**
- Consumes: `config.Load`, `policy.NewEngine`, `policy.NewGuard`, `policy.WithSession`, `store.Open`, `kernel.Run`.

- [ ] **Step 1: Tests** with a real guard around a stub inner runner (records names, answers `ran <name>`) and a scripted approver:
  - `spore.ReadFile("<home>/.ssh/id_ed25519")` → program sees an error; inner never ran `fs_read`.
  - `spore.Shell("sudo ls")` with `ask=["shell_exec"]` → error, approver never asked.
  - Remote profile, `spore.Call("mcp__x__y", map[string]any{})` → error.
  - `ask=["fs_write"]`, approver allows → program prints `wrote`, approver asked once.
- [ ] **Step 2:** `go test -race ./internal/kernel/` passes (these assert existing guard behaviour, so they pass on first run; a failure is a real bug to fix before continuing).
- [ ] **Step 3:** Commit `kernel: prove helper calls meet the real policy guard`.

### Task 9: Gates and manual check

- [ ] `go mod tidy && git diff --exit-code go.mod go.sum`
- [ ] `make test` (or `go test -race ./...`), `make lint` — read the Makefile for the exact targets.
- [ ] `go build -o /tmp/.../spore ./cmd/spore`, then with a scratch config: `spore once "what is the weather in London UK?"` (check `spore` usage for the one-shot command) and confirm one `go_run` call, a formatted answer, and no leftover `SPORE_KERNEL_CHILD` process (`pgrep -f` on the env is not possible; use `ps -eo pid,args | grep "[s]pore"` before and after).
- [ ] TUI and Discord checks need the user's running daemon and token (memory: do not kill their daemon) — hand these to the user.
- [ ] Final whole-branch review by a sonnet reviewer.
