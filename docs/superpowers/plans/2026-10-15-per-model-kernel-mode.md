# Per-Model Kernel Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a weak model (LFM2.5-24B-A2B) run in tools mode, with every tool offered as a direct function call, while every other model stays in code mode.

**Architecture:** A new `[kernel] tools_models` regexp is matched against the turn's `provider/model` ref. A match runs that turn in tools mode, whatever `kernel.mode` says. The mode is resolved from the session's model in one helper, and both places that read the mode today use it: `Snapshot` (the go_run reference in the prompt, and the size that compaction measures) and the request builder (the tool list).

**Tech Stack:** Go 1.2x, `regexp`, BurntSushi TOML config, SQLite store, the `provider.Script` test double.

**Spec:** No separate spec. The design is the "Design" section below. It comes from the 2026-10-15 investigation of LFM2.5 failing to spawn subagents (branch `local-model-tool-args`):
- LFM wrote `agent_spawn(...)` as a bare Go function.
- When told the `spore.Call` form, it then invented `spore.AgentResult`.
- It looped on placeholder programs for 10+ rounds before calling `spore.Call`.
- It calls a single native tool (`go_run`) correctly. Direct tools are the reliable path for it.

## Design

- **Setting.** `[kernel] tools_models = "<regexp>"`, matched with `regexp.MatchString` against the full ref, e.g. `studio/unsloth/LFM2.5-24B-A2B-GGUF`. Unset (`""`) matches nothing. An invalid regexp is a load error that names `kernel.tools_models`. This follows the convention of `cache_control_models`: a regexp string, not a list.
- **Precedence.** A match forces tools mode. With no match, `kernel.mode` decides as it does today. There is no reverse setting (forcing code mode under `mode = "tools"`): YAGNI.
- **Which ref.** It is the ref `modelFor(ctx, sessionID, router.SiteChat)` returns: the session's `/model` choice, otherwise the chat route. Chat and sub-agent turns resolve the same way today, so a child session inherits the mode of its own model.
- **Consistency.** `Snapshot` and the request builder must agree on the mode within one round. Compaction calls `Snapshot` to measure size, so it measures the prompt the model is actually sent.

## Global Constraints

- Build and test with `-tags sqlite_fts5`; the store needs FTS5.
- The CI gate is all of: `go vet -tags sqlite_fts5 ./...`, `make fmtcheck`, `make lint`, `make vulncheck`, `make tidycheck`, `go test -tags sqlite_fts5 -race -timeout 10m ./...`. `make lint` does not run gofmt; `make fmtcheck` does.
- Comments, error messages and commit messages use ASD-STE100 style (short sentences, active voice, one instruction per sentence), as the surrounding code does.
- Never run a second daemon on the Discord bot token. A live check uses an isolated `HOME` with `[daemon] addr = "127.0.0.1:7788"` and no `[bridge.discord]`.
- Do not restart the user's daemon (pid in `~/.spore/spore.pid`) unless the user asks.

## Review Focus

1. **`/model` switch mid-session between modes.** History made in tools mode holds direct `tool_use` blocks (for example `agent_spawn`) that code mode does not offer, and the reverse. The next turn must still build and send a request; it must not drop or rewrite history. Task 2 pins this.
2. **Sub-agent on a different model than its parent.** `agent_spawn(model: ...)` stores a `chat_model` on the child. The child's mode must follow its own model, not its parent's. Task 2 pins this through a session with its own `chat_model`.
3. **Compaction sizing disagrees with the request.** If `Snapshot` used the global mode while the request used tools mode, the go_run reference (about 2-4k tokens) would be counted but not sent. Task 2 asserts that `Snapshot.Kernel` is empty for a matching session.
4. **A regexp that matches too much.** `tools_models = "."` or `"studio"` puts every model in tools mode. This is allowed; the README must show an anchored model-name example.
5. **A model ref that changes in Studio.** If Studio renames the model (another quant, say), the regexp stops matching and LFM silently returns to code mode. The README example matches on `LFM2\.5`, not the full id.

---

### Task 1: `kernel.tools_models` config field

**Files:**
- Modify: `internal/config/config.go` (`KernelConfig` near line 431; `Validate` kernel block near line 1136)
- Test: `internal/config/kernel_test.go`

**Interfaces:**
- Produces: `config.KernelConfig.ToolsModels string` (TOML `tools_models`), and `func (k KernelConfig) ModeFor(ref string) string`. `ModeFor` returns `KernelModeTools` when `ToolsModels` is non-empty and matches `ref`, otherwise `k.Mode`. Validation guarantees the regexp compiles, so `ModeFor` treats a compile error as no match.

- [ ] **Step 1: Write the failing tests** (append to `internal/config/kernel_test.go`)

```go
func TestKernelToolsModelsForcesToolsModeForAMatch(t *testing.T) {
	cfg := loadTestConfig(t, "[kernel]\ntools_models = \"LFM2\\\\.5\"\n")
	if got := cfg.Kernel.ModeFor("studio/unsloth/LFM2.5-24B-A2B-GGUF"); got != KernelModeTools {
		t.Errorf("ModeFor(LFM) = %q, want %q", got, KernelModeTools)
	}
	if got := cfg.Kernel.ModeFor("studio/ISTA-DASLab/Qwen3.8-27B-GSQ-RCO-GGUF"); got != KernelModeCode {
		t.Errorf("ModeFor(Qwen) = %q, want %q", got, KernelModeCode)
	}
}

func TestKernelToolsModelsUnsetLeavesModeAlone(t *testing.T) {
	cfg := loadTestConfig(t, "[kernel]\nmode = \"tools\"\n")
	if got := cfg.Kernel.ModeFor("any/model"); got != KernelModeTools {
		t.Errorf("ModeFor = %q, want the configured mode %q", got, KernelModeTools)
	}
	cfg = loadTestConfig(t, "")
	if got := cfg.Kernel.ModeFor("any/model"); got != KernelModeCode {
		t.Errorf("ModeFor = %q, want the default mode %q", got, KernelModeCode)
	}
}

func TestKernelRejectsABadToolsModelsRegexp(t *testing.T) {
	_, err := loadTestConfigErr(t, "[kernel]\ntools_models = \"(\"\n")
	if err == nil || !strings.Contains(err.Error(), "kernel.tools_models") {
		t.Fatalf("err = %v, want one naming kernel.tools_models", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/config -run 'ToolsModels'`
Expected: build failure, `cfg.Kernel.ModeFor undefined`.

- [ ] **Step 3: Implement**

In `KernelConfig`, after `Mode`:

```go
	Mode string `toml:"mode"`
	// ToolsModels is a regexp over provider/model refs. A turn on a
	// matching model runs in tools mode, whatever Mode says. Use it for a
	// model that calls tools well but writes go_run programs badly. Unset
	// matches no model.
	ToolsModels string `toml:"tools_models"`
```

After the `KernelConfig` type:

```go
// ModeFor returns the kernel mode for a turn on the model ref. Validate
// has compiled ToolsModels already, so a compile error here means no match.
func (k KernelConfig) ModeFor(ref string) string {
	if k.ToolsModels != "" {
		if ok, err := regexp.MatchString(k.ToolsModels, ref); err == nil && ok {
			return KernelModeTools
		}
	}
	return k.Mode
}
```

In `Validate`, after the `switch c.Kernel.Mode` block:

```go
	if c.Kernel.ToolsModels != "" {
		if _, err := regexp.Compile(c.Kernel.ToolsModels); err != nil {
			return fmt.Errorf("kernel.tools_models: %w", err)
		}
	}
```

`regexp` is already imported (it is used for `cache_control_models`). `fillKernelDefaults` needs no change: an empty string is the correct default.

- [ ] **Step 4: Run the config tests**

Run: `go test -tags sqlite_fts5 ./internal/config`
Expected: PASS. This includes `TestKernelDefaultsFillIn`, which compares the whole struct; the zero `ToolsModels` keeps it equal.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/kernel_test.go
git commit -m "config: kernel.tools_models puts matching models in tools mode"
```

### Task 2: The agent resolves the mode per session model

**Files:**
- Modify: `internal/agent/agent.go`:
  - `Snapshot`, near line 195: `if a.Tools != nil && a.Cfg.Kernel.Mode == config.KernelModeCode`
  - `loop`, near lines 350-362: the tool list is narrowed before `modelFor` is called.
- Test: `internal/agent/kernel_test.go`

**Interfaces:**
- Consumes: `config.KernelConfig.ModeFor(ref string) string` (Task 1); the existing `func (a *Agent) modelFor(ctx context.Context, sessionID, site string) (ref string, chosen bool, err error)`; `store.SetSessionModel(ctx, id, "chat", ref)`.
- Produces: `func (a *Agent) kernelMode(ctx context.Context, sessionID string) (string, error)`. It returns the mode for the session's chat model.

- [ ] **Step 1: Write the failing tests** (append to `internal/agent/kernel_test.go`)

The harness registers the provider `test`. Its default model is `test/model-a`.

```go
// runOnceOn runs one turn in code mode, with tools_models set to pattern,
// on a session whose chat model is ref ("" keeps the default).
func runOnceOn(t *testing.T, pattern, ref string) (provider.Request, *Agent, string) {
	t.Helper()
	script := provider.NewScript(provider.ScriptTurn{Text: "done"})
	a, st := harness(t, script, kernelTools{})
	a.Cfg.Kernel.Mode = config.KernelModeCode
	a.Cfg.Kernel.ToolsModels = pattern
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	if ref != "" {
		if err := st.SetSessionModel(ctx, sid, "chat", ref); err != nil {
			t.Fatal(err)
		}
	}
	ch, err := a.Run(ctx, sid, "weather in London?")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times", len(reqs))
	}
	return reqs[0], a, sid
}

// LFM2.5 calls a native tool correctly but writes go_run programs that
// call tools as bare functions. A model that tools_models matches must get
// every tool directly and no go_run reference; any other model keeps code
// mode.
func TestToolsModelsPutsAMatchingSessionModelInToolsMode(t *testing.T) {
	req, a, sid := runOnceOn(t, "model-b", "test/model-b")
	if len(req.Tools) != 3 {
		t.Fatalf("tools = %+v, want all three", req.Tools)
	}
	if strings.Contains(systemText(req.System), "## Acting through go_run") {
		t.Error("a tools-mode turn carries the go_run section")
	}
	// Compaction measures Snapshot, so it must leave the reference out too.
	snap, err := a.Snapshot(context.Background(), sid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kernel != "" {
		t.Error("Snapshot carries the go_run reference that the request leaves out")
	}
}

func TestToolsModelsLeavesOtherModelsInCodeMode(t *testing.T) {
	req, _, _ := runOnceOn(t, "model-b", "")
	if len(req.Tools) != 1 || req.Tools[0].Name != "go_run" {
		t.Fatalf("tools = %+v, want only go_run", req.Tools)
	}
}

// /model can move a session between modes. History from the other mode
// holds tool_use blocks for tools the new mode does not offer. The turn
// must still send that history, unchanged.
func TestSwitchingModeKeepsHistoryFromTheOtherMode(t *testing.T) {
	script := provider.NewScript(
		provider.ScriptTurn{ToolCalls: []provider.Block{
			{Type: provider.BlockToolUse, ID: "direct", Name: "schedule_list", Input: json.RawMessage(`{}`)},
		}},
		provider.ScriptTurn{Text: "listed"},
		provider.ScriptTurn{Text: "done"},
	)
	a, st := harness(t, script, kernelTools{})
	a.Cfg.Kernel.Mode = config.KernelModeCode
	a.Cfg.Kernel.ToolsModels = "model-b"
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	if err := st.SetSessionModel(ctx, sid, "chat", "test/model-b"); err != nil {
		t.Fatal(err)
	}
	ch, err := a.Run(ctx, sid, "list jobs")
	if err != nil {
		t.Fatal(err)
	}
	collect(t, ch)

	if err := st.SetSessionModel(ctx, sid, "chat", ""); err != nil {
		t.Fatal(err)
	}
	ch, err = a.Run(ctx, sid, "again")
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range collect(t, ch) {
		if ev.Type == EvError {
			t.Fatalf("the code-mode turn failed: %v", ev.Err)
		}
	}
	reqs := script.Requests()
	last := reqs[len(reqs)-1]
	if len(last.Tools) != 1 || last.Tools[0].Name != "go_run" {
		t.Fatalf("tools = %+v, want only go_run after the switch", last.Tools)
	}
	found := false
	for _, m := range last.Messages {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolUse && b.Name == "schedule_list" {
				found = true
			}
		}
	}
	if !found {
		t.Error("the direct schedule_list call from tools mode is missing from history")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent -run 'ToolsModels|SwitchingMode'`
Expected: `TestToolsModelsPutsAMatchingSessionModelInToolsMode` FAILS with `want all three`. The other two may already pass: they pin behavior that must not regress.

- [ ] **Step 3: Implement**

Add, after `modelFor` in `internal/agent/agent.go`:

```go
// kernelMode returns the kernel mode for the session's turns: tools mode
// when kernel.tools_models matches the session's model, otherwise
// kernel.mode. Snapshot and the request builder both use it, so the size
// compaction measures is the size the model is sent.
func (a *Agent) kernelMode(ctx context.Context, sessionID string) (string, error) {
	if a.Cfg.Kernel.ToolsModels == "" {
		return a.Cfg.Kernel.Mode, nil
	}
	ref, _, err := a.modelFor(ctx, sessionID, router.SiteChat)
	if err != nil {
		return "", err
	}
	return a.Cfg.Kernel.ModeFor(ref), nil
}
```

The early return keeps today's behavior, and today's store reads, for every config without `tools_models`.

In `Snapshot`, replace:

```go
	if a.Tools != nil && a.Cfg.Kernel.Mode == config.KernelModeCode {
		snap.Kernel = kernel.Reference(a.Tools.Specs())
	}
```

with:

```go
	if a.Tools != nil {
		mode, err := a.kernelMode(ctx, sessionID)
		if err != nil {
			return Snapshot{}, err
		}
		if mode == config.KernelModeCode {
			snap.Kernel = kernel.Reference(a.Tools.Specs())
		}
	}
```

In `loop`, replace:

```go
			if a.Cfg.Kernel.Mode == config.KernelModeCode {
				req.Tools = onlyKernel(req.Tools)
			}
```

with:

```go
			mode, err := a.kernelMode(ctx, sessionID)
			if err != nil {
				return err
			}
			if mode == config.KernelModeCode {
				req.Tools = onlyKernel(req.Tools)
			}
```

- [ ] **Step 4: Run the agent tests, then the whole suite**

Run: `go test -tags sqlite_fts5 ./internal/agent`
Expected: PASS, including the existing `TestCodeModeOffersOnlyGoRun` and `TestToolsModeSendsEveryToolAndNoReference`.

Run: `go test -tags sqlite_fts5 -race -timeout 10m ./...`
Expected: PASS. `TestWatchStallLetsASteadyBodyRunLong` and `TestStreamOutlivesTheStallLimitWhileItSends` can flake under full-suite load. If only those fail, rerun them with `-count=3` in isolation, and report both results.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/kernel_test.go
git commit -m "agent: resolve the kernel mode from the session's model"
```

### Task 3: README, gates, live check

**Files:**
- Modify: `README.md`, the `[kernel]` TOML block near line 481, and the sentence before it.

- [ ] **Step 1: Document the setting**

After "Switch with `[kernel] mode = "tools"`." at README.md line 477, add:

```markdown
Some small models call tools well but write go_run programs badly: they call
a tool as a Go function, or invent spore helpers. Put those models in tools
mode alone with `tools_models`, a regexp over `provider/model` refs.
```

In the TOML block, after the `mode` line, add:

```toml
tools_models        = "LFM2\\.5"   # these models get every tool directly
```

- [ ] **Step 2: Run the whole CI gate**

Run: `go vet -tags sqlite_fts5 ./... && make fmtcheck && make lint && make vulncheck && make tidycheck && go test -tags sqlite_fts5 -race -timeout 10m ./...`
Expected: every step passes. Report each step's result, not a summary.

- [ ] **Step 3: Live check on LFM with an isolated daemon**

1. Build the branch binary: `go build -tags sqlite_fts5 -o <scratch>/spore-branch ./cmd/spore`.
2. Make `<scratch>/home/.spore/config.toml` with these parts:
   - `default_model = "studio/unsloth/LFM2.5-24B-A2B-GGUF"`
   - the `[providers.studio]` block from `~/.spore/config.toml`, including its `api_key` line (an `${...}` reference; do not copy the secret)
   - `[daemon] addr = "127.0.0.1:7788"`
   - `[policy] workspace = "<repo>"`
   - `[policy.learned] allow = ["agent_spawn", "agent_run", "agent_result"]`
   - `[kernel] tools_models = "LFM2\\.5"`
3. Load the env vars: `set -a; . ~/.spore/env; set +a`. Then run the subagent prompt from the investigation 3 times with `HOME=<scratch>/home <scratch>/spore-branch once "<prompt>"`.
4. Pass: in each run, `subagent_runs` in `<scratch>/home/.spore/spore.db` gains two rows with state `done`, and the reply reports 91, 11 and 102. Record every run, pass or fail.
5. Stop the isolated daemon with `kill $(cat <scratch>/home/.spore/spore.pid)`. Confirm that `~/.spore/spore.pid` is unchanged.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "README: document kernel.tools_models"
```

- [ ] **Step 5: Hand the user's config change to the user**

Do not edit `~/.spore/config.toml` without asking. Propose this addition to its `[kernel]` section (add the section if it is missing), with a dated backup first:

```toml
[kernel]
tools_models = "LFM2\\.5"
```

The running daemon reads it only after a restart. Restart only when asked.
