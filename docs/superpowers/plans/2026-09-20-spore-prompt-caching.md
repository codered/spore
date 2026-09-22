# Prompt Caching Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** cut cost and latency on every turn by caching the stable prefix of the request, which means reordering the prompt so there is a stable prefix to cache.

**Architecture:** one new field, `provider.Block.CacheBreak`, expresses a cache breakpoint. `Request.System` becomes `[]Block` so breakpoints can be placed on it. `agent.Assemble` orders the system side by stability and moves the per-turn environment section to the tail of the final user message, where it invalidates nothing. The Anthropic adapter renders `cache_control` on marked blocks; every other adapter ignores the field.

**Tech Stack:** Go, SQLite (`-tags sqlite_fts5`), the Anthropic Messages API over hand-rolled HTTP (no SDK), `make` for every gate.

**Spec:** `docs/superpowers/specs/2026-09-20-spore-prompt-caching-design.md`. Read it before Task 1. It carries the reasoning; this plan carries the steps.

## Global Constraints

- **Every `go` command carries `-tags sqlite_fts5`.** `make test` already does. Without it the suite fails with `no such module: fts5`.
- **Tests first.** Write the failing test, run it, watch it fail for the right reason, then implement.
- **`gofmt -l .` must be empty** before every commit. CI runs `make fmtcheck` and fails on any file.
- **`make lint` must pass** — golangci-lint v2.13.2, 8 linters. A lint finding about an unused value in this codebase is not automatically cosmetic: check whether the expression writes to the store before deleting it.
- **A migration must be wired into `Open` at the call site.** A migration function that exists and is never called is the exact defect that shipped on the last change; `CREATE TABLE IF NOT EXISTS` does not add columns to an existing table.
- **Comments carry reasoning, not restatement.** Match the voice of the file you are editing; `internal/recall/mirror/mirror.go` and `internal/agent/context.go` are the models.
- **Do not widen the change.** The spec's section 5 lists what is out of scope: sub-agent prefix sharing, `role: "system"` messages, a 1-hour TTL knob, per-model minimum-prefix guards, `openaicompat` caching, and `compact.go`'s own request.
- **One commit per task**, subject in the sentence style of recent history.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/provider/types.go` | `Block.CacheBreak`, `Request.System []Block`, `Usage` cache counters | 1, 4 |
| `internal/provider/registry.go` | `ProviderPrice` cache rates and `Cost` | 5 |
| `internal/provider/anthropic/anthropic.go` | render `cache_control`; parse cache counters | 3, 4 |
| `internal/provider/openaicompat/openaicompat.go` | join system blocks; ignore `CacheBreak` | 1 |
| `internal/agent/context.go` | prompt order, environment placement, breakpoints | 2 |
| `internal/agent/compact.go` | its own system prompt as a block | 1 |
| `internal/config/config.go` | `cache`, `price_cache_write`, `price_cache_read` | 3, 5 |
| `cmd/spore/wire.go` | pass the new config through | 3, 5 |
| `internal/store/schema.go`, `store.go`, `usage.go` | cache token columns, migration, usage rows | 6 |
| `internal/daemon/sessions.go` | cache counters in message JSON | 6 |
| `cmd/spore/tui.go` | the `/usage` cache-read line | 6 |

---

### Task 1: The type change, with no behavioural change

Make `CacheBreak` and `[]Block` exist and compile everywhere. Nothing emits a breakpoint yet, and the bytes on the wire must be identical to today's. That is what makes this task independently reviewable: a reviewer checks "nothing changed" and can approve it without reasoning about caching at all.

**Files:**
- Modify: `internal/provider/types.go:27-56`
- Modify: `internal/provider/anthropic/anthropic.go:86-95`
- Modify: `internal/provider/openaicompat/openaicompat.go:37-41`
- Modify: `internal/agent/context.go:170-190`
- Modify: `internal/agent/compact.go:101`
- Test: `internal/provider/openaicompat/openaicompat_test.go`

**Interfaces:**
- Produces: `provider.Block.CacheBreak bool`; `provider.Request.System []provider.Block`; `openaicompat.toWire(system []provider.Block, msgs []provider.Message) []map[string]any`.

- [ ] **Step 1: Write the failing test**

Add to `internal/provider/openaicompat/openaicompat_test.go`:

```go
// Several system blocks become one system message. OpenAI-compatible
// endpoints have no client-side cache control and no notion of system
// blocks, so the join is the whole translation.
func TestToWireJoinsSystemBlocks(t *testing.T) {
	out := toWire([]provider.Block{
		{Type: provider.BlockText, Text: "alpha"},
		{Type: provider.BlockText, Text: "beta", CacheBreak: true},
	}, nil)

	if len(out) != 1 {
		t.Fatalf("got %d messages, want one system message: %v", len(out), out)
	}
	if out[0]["role"] != "system" {
		t.Errorf("role = %v, want system", out[0]["role"])
	}
	if out[0]["content"] != "alphabeta" {
		t.Errorf("content = %q, want the blocks joined with no separator", out[0]["content"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags sqlite_fts5 -run TestToWireJoinsSystemBlocks ./internal/provider/openaicompat/`
Expected: FAIL to compile — `cannot use []provider.Block literal as string value` and `unknown field CacheBreak`.

- [ ] **Step 3: Add the field and change the type**

In `internal/provider/types.go`, add to `Block` after `Truncated`:

```go
	// CacheBreak asks the provider to place a cache breakpoint after this
	// block. It is never persisted: where a breakpoint goes is a fact about
	// one request, not a property of a stored message, and a breakpoint
	// baked into stored history would itself change the prefix on replay.
	CacheBreak bool `json:"-"`
```

And change `Request`:

```go
type Request struct {
	Model string
	// System is ordered by stability, most stable first, so a cache
	// breakpoint on a later block has an unchanging prefix in front of it.
	System      []Block
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature float64
}
```

- [ ] **Step 4: Update the four call sites**

`internal/provider/openaicompat/openaicompat.go:37`:

```go
func toWire(system []provider.Block, msgs []provider.Message) []map[string]any {
	out := []map[string]any{}
	var sys strings.Builder
	for _, b := range system {
		sys.WriteString(b.Text)
	}
	if sys.Len() > 0 {
		out = append(out, map[string]any{"role": "system", "content": sys.String()})
	}
```

`internal/provider/anthropic/anthropic.go`, in `Stream`, replace the `if req.System != ""` block with a join that keeps today's bytes exactly:

```go
	// Task 3 replaces this with an array of blocks carrying cache_control.
	// Joining here keeps this task's wire output byte-identical to before.
	var sys strings.Builder
	for _, b := range req.System {
		sys.WriteString(b.Text)
	}
	if sys.Len() > 0 {
		body["system"] = sys.String()
	}
```

`internal/agent/context.go`, at the end of `Assemble`:

```go
	return provider.Request{
		System:    []provider.Block{{Type: provider.BlockText, Text: sys.String()}},
		Messages:  msgs,
		MaxTokens: 4096,
	}
```

`internal/agent/compact.go:101`:

```go
		System:    []provider.Block{{Type: provider.BlockText, Text: compactionPrompt}},
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/provider/... ./internal/agent/...`
Expected: PASS. Existing anthropic wire tests must pass unchanged — if one fails, the join changed the bytes and that is a real failure, not a test to update.

- [ ] **Step 6: Commit**

```bash
gofmt -l .
git add internal/provider internal/agent
git commit -m "Carry the system prompt as blocks rather than one string

A cache breakpoint goes on a content block, so the system side has to be a
list of blocks before any placement can be expressed. Block.CacheBreak is the
one mechanism for both breakpoints; it is never persisted, because where a
breakpoint goes is a fact about one request.

No behavioural change: the adapters join the blocks and put the same bytes on
the wire as before."
```

---

### Task 2: The prompt order and the environment's new home

The core of the change. This is where caching becomes possible.

**Files:**
- Modify: `internal/agent/context.go:170-190`
- Test: `internal/agent/context_test.go`

**Interfaces:**
- Consumes: `provider.Block.CacheBreak`, `provider.Request.System []provider.Block` (Task 1).
- Produces: `Assemble` emitting exactly two breakpoints; an unexported `prefixBytes(provider.Request) string` test helper is **not** produced — the test defines its own, shown below.

- [ ] **Step 1: Write the failing tests**

Add to `internal/agent/context_test.go`:

```go
// renderPrefix is the bytes the provider would cache: every system block, then
// every message block, stopping after the last block marked CacheBreak. Two
// requests whose renderPrefix output matches share a cache entry.
func renderPrefix(req provider.Request) string {
	var b strings.Builder
	for _, blk := range req.System {
		b.WriteString(blk.Text)
		if blk.CacheBreak {
			b.WriteString("|#|")
		}
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			b.WriteString(blk.Text)
			if blk.CacheBreak {
				return b.String()
			}
		}
	}
	return b.String()
}

func countBreaks(req provider.Request) int {
	n := 0
	for _, blk := range req.System {
		if blk.CacheBreak {
			n++
		}
	}
	for _, m := range req.Messages {
		for _, blk := range m.Blocks {
			if blk.CacheBreak {
				n++
			}
		}
	}
	return n
}

func snapWithEnv(env string) Snapshot {
	return Snapshot{
		System:      "SYSTEM",
		Environment: env,
		Summary:     "SUMMARY",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hello"}}},
		},
	}
}

// The property the whole design rests on: the environment changes every turn
// and must change nothing the provider reads from cache.
func TestAssembleKeepsThePrefixStableAcrossEnvironments(t *testing.T) {
	a := Assemble(snapWithEnv("files: one.go"), config.ContextConfig{})
	b := Assemble(snapWithEnv("files: one.go two.go three.go"), config.ContextConfig{})

	if renderPrefix(a) != renderPrefix(b) {
		t.Fatalf("the cached prefix moved when the environment changed:\n a: %q\n b: %q",
			renderPrefix(a), renderPrefix(b))
	}
}

func TestAssembleEmitsTwoBreakpoints(t *testing.T) {
	req := Assemble(snapWithEnv("files: one.go"), config.ContextConfig{})
	if n := countBreaks(req); n != 2 {
		t.Fatalf("got %d breakpoints, want 2 (system prefix and message tail)", n)
	}
}

// The environment rides at the very end, after the moving breakpoint, so it is
// outside every cache entry.
func TestAssemblePutsEnvironmentLastAndUncached(t *testing.T) {
	req := Assemble(snapWithEnv("ENVIRONMENT"), config.ContextConfig{})

	for _, blk := range req.System {
		if strings.Contains(blk.Text, "ENVIRONMENT") {
			t.Fatal("the environment is still in the system block")
		}
	}
	last := req.Messages[len(req.Messages)-1]
	tail := last.Blocks[len(last.Blocks)-1]
	if tail.Text != "ENVIRONMENT" {
		t.Fatalf("last block is %q, want the environment", tail.Text)
	}
	if tail.CacheBreak {
		t.Fatal("the environment block is marked as a breakpoint; it changes every turn")
	}
	if !last.Blocks[len(last.Blocks)-2].CacheBreak {
		t.Fatal("the breakpoint does not sit immediately before the environment")
	}
}

// Assemble is documented as a pure function. copy() duplicates the Message
// values but not their Blocks arrays, so a careless append writes through into
// the caller's snapshot -- and from there into whatever the store hands out
// next.
func TestAssembleDoesNotMutateTheSnapshot(t *testing.T) {
	snap := snapWithEnv("ENVIRONMENT")
	before := len(snap.Messages[0].Blocks)

	Assemble(snap, config.ContextConfig{})

	if got := len(snap.Messages[0].Blocks); got != before {
		t.Fatalf("the snapshot's blocks grew from %d to %d", before, got)
	}
	if snap.Messages[0].Blocks[0].CacheBreak {
		t.Fatal("Assemble marked a breakpoint on the caller's snapshot")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 -run TestAssemble ./internal/agent/`
Expected: FAIL — `got 0 breakpoints, want 2`, and the prefix test fails because the environment is still inside the system string.

- [ ] **Step 3: Rewrite `Assemble`**

Replace the body of `Assemble` in `internal/agent/context.go`:

```go
// Assemble builds the request ordered by stability rather than by topic: the
// system prompt, the skills index, the memory facts and the compaction summary
// all change rarely, so they sit in front of a cache breakpoint. The
// environment section changes every turn and rides at the very end, after the
// moving breakpoint, where it invalidates nothing.
//
// The environment is never stored. It is injected here and here only, so the
// prefix one turn reads is byte-identical to what the turn before it wrote.
func Assemble(snap Snapshot, cfg config.ContextConfig) provider.Request {
	var sys []provider.Block
	add := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		sys = append(sys, provider.Block{Type: provider.BlockText, Text: text})
	}
	add(snap.System)
	add(skillsSection(snap.Skills, cfg.SkillBudget))
	add(factsSection(snap.Facts, cfg.FactBudget))
	if snap.Summary != "" {
		add("\n\n## Earlier in this conversation\n" + snap.Summary + "\n")
	}
	if len(sys) > 0 {
		sys[len(sys)-1].CacheBreak = true
	}

	// Copy so callers cannot alias the snapshot's backing array.
	msgs := make([]provider.Message, len(snap.Messages))
	copy(msgs, snap.Messages)
	msgs = markTailAndAppendEnvironment(msgs, snap.Environment)

	return provider.Request{System: sys, Messages: msgs, MaxTokens: 4096}
}

// markTailAndAppendEnvironment puts the moving breakpoint on the last block of
// the conversation and the environment after it.
//
// It copies the final message's blocks first. copy() above duplicates the
// Message values but not the slices inside them, so writing through
// msgs[last].Blocks would reach into the caller's snapshot -- and Assemble is
// documented as pure.
func markTailAndAppendEnvironment(msgs []provider.Message, env string) []provider.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := len(msgs) - 1
	blocks := make([]provider.Block, len(msgs[last].Blocks), len(msgs[last].Blocks)+1)
	copy(blocks, msgs[last].Blocks)
	if len(blocks) > 0 {
		blocks[len(blocks)-1].CacheBreak = true
	}
	if strings.TrimSpace(env) != "" {
		blocks = append(blocks, provider.Block{Type: provider.BlockText, Text: env})
	}
	msgs[last].Blocks = blocks
	return msgs
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/agent/`
Expected: PASS, including the existing `SnapshotBreakdown` and compaction tests. `Breakdown.Environment` still counts the environment; it is in the request, just in a different place.

- [ ] **Step 5: Commit**

```bash
gofmt -l .
git add internal/agent
git commit -m "Order the prompt by stability and move the environment to the tail

The environment section is rebuilt every turn and sat at position two of the
system block, ahead of the facts, the skills index and the summary. Caching is
a prefix match, so a workspace file changing invalidated everything behind it
-- markers alone would have bought nothing.

It now rides at the end of the final user message, after the moving
breakpoint. It is injected at assembly and never stored, so the prefix one
turn reads is byte-identical to what the turn before wrote; storing it would
drag a stale environment into the prefix and grow it every turn."
```

---

### Task 3: `cache_control` on the wire, and the escape hatch

**Files:**
- Modify: `internal/provider/anthropic/anthropic.go:41-47` (`New`), `:86-113` (`Stream`), `toWire`
- Modify: `internal/config/config.go:117-126`
- Modify: `cmd/spore/wire.go:98-106`
- Test: `internal/provider/anthropic/anthropic_test.go`

**Interfaces:**
- Consumes: `provider.Block.CacheBreak`, `Request.System []Block` (Task 1); two breakpoints per request (Task 2).
- Produces: `anthropic.New(baseURL, apiKey, workspaceID string, cache bool, hc *http.Client) *Client`; `config.ProviderConfig.Cache *bool` with `func (p ProviderConfig) CacheEnabled() bool`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/provider/anthropic/anthropic_test.go`:

```go
// bodyOf runs one Stream against a recording server and returns the decoded
// request body. The wire format is the contract with the API, so it is
// asserted on the bytes rather than on an intermediate struct.
func bodyOf(t *testing.T, cache bool, req provider.Request) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "k", "", cache, nil)
	ch, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	return got
}

func cachedRequest() provider.Request {
	return provider.Request{
		Model:     "claude-opus-5",
		MaxTokens: 100,
		System: []provider.Block{
			{Type: provider.BlockText, Text: "stable"},
			{Type: provider.BlockText, Text: "summary", CacheBreak: true},
		},
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{
			{Type: provider.BlockText, Text: "hello", CacheBreak: true},
			{Type: provider.BlockText, Text: "env"},
		}}},
	}
}

func TestSystemIsBlocksWithCacheControlOnTheMarkedOne(t *testing.T) {
	body := bodyOf(t, true, cachedRequest())

	blocks, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system = %T, want an array of blocks", body["system"])
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d system blocks, want 2", len(blocks))
	}
	if _, marked := blocks[0].(map[string]any)["cache_control"]; marked {
		t.Error("the first system block carries cache_control; only the last should")
	}
	cc, marked := blocks[1].(map[string]any)["cache_control"].(map[string]any)
	if !marked {
		t.Fatal("the last system block carries no cache_control")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control type = %v, want ephemeral", cc["type"])
	}
}

func TestMessageBreakpointRendersAndTheEnvironmentDoesNot(t *testing.T) {
	body := bodyOf(t, true, cachedRequest())

	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if _, marked := content[0].(map[string]any)["cache_control"]; !marked {
		t.Error("the marked message block carries no cache_control")
	}
	if _, marked := content[1].(map[string]any)["cache_control"]; marked {
		t.Error("the environment block carries cache_control; it changes every turn")
	}
}

// The escape hatch exists for a proxy that rejects the field.
func TestCacheDisabledEmitsNoCacheControl(t *testing.T) {
	body := bodyOf(t, false, cachedRequest())

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("cache_control")) {
		t.Fatalf("cache_control was sent with caching off: %s", raw)
	}
	if _, ok := body["system"].([]any); !ok {
		t.Errorf("system = %T, want an array of blocks even with caching off", body["system"])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 -run 'TestSystemIsBlocks|TestMessageBreakpoint|TestCacheDisabled' ./internal/provider/anthropic/`
Expected: FAIL to compile — `too many arguments in call to New`.

- [ ] **Step 3: Add the client flag and render the markers**

In `internal/provider/anthropic/anthropic.go`, add `cache bool` to the `Client` struct and to `New`:

```go
// New builds a client. cache turns prompt-caching breakpoints on; an operator
// sets it false for a proxy that rejects the field, or for a workload whose
// prompt changes from the first byte every turn and would pay the write
// premium for nothing.
func New(baseURL, apiKey, workspaceID string, cache bool, hc *http.Client) *Client {
```

Add the marker helper and use it in `toWire`:

```go
// cacheControl is the marker the API reads. The 5-minute TTL is the default
// and spore does not offer the 1-hour one: a read refreshes the entry's timer
// for free, so turns less than five minutes apart keep it warm indefinitely,
// while the 1-hour TTL doubles the write premium.
var cacheControl = map[string]any{"type": "ephemeral"}
```

`toWire` becomes a method so it can see the flag, and emits blocks as maps:

```go
func (c *Client) toWire(msgs []provider.Message) []map[string]any {
```

Inside the block loop, after building each `wireBlock`, render it into a map and attach the marker when `c.cache && b.CacheBreak`. Keep the existing per-type handling exactly as it is; the only addition is the marker.

In `Stream`, replace the joined system string from Task 1:

```go
	if len(req.System) > 0 {
		blocks := make([]map[string]any, 0, len(req.System))
		for _, b := range req.System {
			blk := map[string]any{"type": "text", "text": b.Text}
			if c.cache && b.CacheBreak {
				blk["cache_control"] = cacheControl
			}
			blocks = append(blocks, blk)
		}
		body["system"] = blocks
	}
```

- [ ] **Step 4: Add the config field**

`internal/config/config.go`, in `ProviderConfig`:

```go
	// Cache turns prompt caching on for an anthropic provider. Unset means
	// true: cheaper and faster is the right default, and an operator should
	// not have to find a flag to get it.
	Cache *bool `toml:"cache"`
```

```go
// CacheEnabled reports whether to send cache breakpoints. Unset is true.
func (p ProviderConfig) CacheEnabled() bool { return p.Cache == nil || *p.Cache }
```

`cmd/spore/wire.go:105`:

```go
			reg.Register(name, anthropic.New(pc.BaseURL, pc.APIKey, ws, pc.CacheEnabled(), nil), price)
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/provider/... ./internal/config/... ./cmd/...`
Expected: PASS. Other `New(` call sites in tests need the extra argument; add `true`.

- [ ] **Step 6: Commit**

```bash
gofmt -l .
git add internal/provider internal/config cmd/spore
git commit -m "Send cache breakpoints to the Anthropic API

A marked block renders cache_control with the default 5-minute TTL: a read
refreshes the entry for free, so turns less than five minutes apart keep it
warm, while the 1-hour TTL doubles the write premium for a window an
interactive loop rarely sits in.

cache = false on a provider block turns the markers off for a proxy that
rejects the field. The prompt is ordered the new way either way; the ordering
is an improvement on its own."
```

---

### Task 4: Read the cache counters back

**Files:**
- Modify: `internal/provider/types.go` (`Usage`)
- Modify: `internal/provider/anthropic/anthropic.go:224-256`
- Test: `internal/provider/anthropic/anthropic_test.go`

**Interfaces:**
- Produces: `provider.Usage.CacheWriteTokens int`, `provider.Usage.CacheReadTokens int`.

- [ ] **Step 1: Write the failing test**

```go
// The counters are the only ground truth that caching works: when it breaks,
// requests keep succeeding and the bill just goes up.
func TestStreamReadsCacheCounters(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"usage":` +
			`{"input_tokens":7,"output_tokens":0,` +
			`"cache_creation_input_tokens":1200,"cache_read_input_tokens":9000}}}` + "\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(srv.Close)

	ch, err := New(srv.URL, "k", "", true, nil).Stream(context.Background(), provider.Request{})
	if err != nil {
		t.Fatal(err)
	}
	var got provider.Usage
	for ev := range ch {
		if ev.Usage != nil {
			got = *ev.Usage
		}
	}

	if got.CacheWriteTokens != 1200 {
		t.Errorf("CacheWriteTokens = %d, want 1200", got.CacheWriteTokens)
	}
	if got.CacheReadTokens != 9000 {
		t.Errorf("CacheReadTokens = %d, want 9000", got.CacheReadTokens)
	}
	if got.InputTokens != 7 {
		t.Errorf("InputTokens = %d, want 7 -- it is the uncached remainder only", got.InputTokens)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -tags sqlite_fts5 -run TestStreamReadsCacheCounters ./internal/provider/anthropic/`
Expected: FAIL — `got.CacheWriteTokens undefined`.

- [ ] **Step 3: Add the fields and parse them**

`internal/provider/types.go`:

```go
type Usage struct {
	// InputTokens is the uncached remainder, not the prompt size: the whole
	// prompt is InputTokens + CacheWriteTokens + CacheReadTokens.
	InputTokens      int
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
}
```

In the SSE event struct in `anthropic.go`, add to the `Message.Usage` anonymous struct:

```go
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
```

And in the `message_start` case:

```go
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
			usage.CacheWriteTokens = ev.Message.Usage.CacheCreationInputTokens
			usage.CacheReadTokens = ev.Message.Usage.CacheReadInputTokens
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/provider/...`
Expected: PASS, existing fixtures included — they carry no cache fields and parse as zero.

- [ ] **Step 5: Commit**

```bash
gofmt -l .
git add internal/provider
git commit -m "Read the cache token counters from the stream

InputTokens is now the uncached remainder rather than the prompt size; the
whole prompt is the sum of the three. Nothing in spore reads it as a size --
context estimation works from text -- but the field's meaning changed and the
comment says so."
```

---

### Task 5: Price the three buckets

**Files:**
- Modify: `internal/provider/registry.go:9-14`
- Modify: `internal/config/config.go:117-126`
- Modify: `cmd/spore/wire.go:98`
- Test: `internal/provider/registry_test.go`

**Interfaces:**
- Consumes: `provider.Usage` cache counters (Task 4).
- Produces: `provider.ProviderPrice{In, Out, CacheWrite, CacheRead float64}`; `config.ProviderConfig.PriceCacheWrite`, `.PriceCacheRead`.

- [ ] **Step 1: Write the failing tests**

```go
// Sub-agent budgets are enforced on recorded cost, so pricing reads at the
// full input rate would cut trees off on spend that never happened.
func TestCostBillsEachBucketAtItsOwnRate(t *testing.T) {
	p := ProviderPrice{In: 5, Out: 25, CacheWrite: 6.25, CacheRead: 0.5}
	u := Usage{InputTokens: 1e6, OutputTokens: 1e6, CacheWriteTokens: 1e6, CacheReadTokens: 1e6}

	if got, want := p.Cost(u), 36.75; math.Abs(got-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v (5 + 25 + 6.25 + 0.5)", got, want)
	}
}

// An operator who configured prices before caching existed still gets correct
// costs, with no edit.
func TestCostDefaultsTheCacheRatesFromPriceIn(t *testing.T) {
	p := ProviderPrice{In: 10, Out: 50}
	u := Usage{CacheWriteTokens: 1e6, CacheReadTokens: 1e6}

	if got, want := p.Cost(u), 13.5; math.Abs(got-want) > 1e-9 {
		t.Errorf("Cost = %v, want %v (1.25x + 0.10x of price_in)", got, want)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 -run TestCost ./internal/provider/`
Expected: FAIL — `unknown field CacheWrite`.

- [ ] **Step 3: Implement**

`internal/provider/registry.go`:

```go
// ProviderPrice is USD per million tokens. CacheWrite and CacheRead are
// optional: unset, they default to the usual multipliers of In. They are
// settable because the multipliers are not universal -- Fable 5.1 reads at
// 0.025x -- and an operator should not wait for a spore release to price
// their model correctly.
type ProviderPrice struct{ In, Out, CacheWrite, CacheRead float64 }

const (
	defaultCacheWriteMultiplier = 1.25
	defaultCacheReadMultiplier  = 0.10
)

func (p ProviderPrice) Cost(u Usage) float64 {
	write, read := p.CacheWrite, p.CacheRead
	if write == 0 {
		write = p.In * defaultCacheWriteMultiplier
	}
	if read == 0 {
		read = p.In * defaultCacheReadMultiplier
	}
	return float64(u.InputTokens)/1e6*p.In +
		float64(u.OutputTokens)/1e6*p.Out +
		float64(u.CacheWriteTokens)/1e6*write +
		float64(u.CacheReadTokens)/1e6*read
}
```

`internal/config/config.go`, in `ProviderConfig`:

```go
	// PriceCacheWrite and PriceCacheRead are USD per million tokens for
	// cached input. Unset, they default to 1.25x and 0.10x of PriceIn.
	PriceCacheWrite float64 `toml:"price_cache_write"`
	PriceCacheRead  float64 `toml:"price_cache_read"`
```

`cmd/spore/wire.go:98`:

```go
		price := provider.ProviderPrice{
			In: pc.PriceIn, Out: pc.PriceOut,
			CacheWrite: pc.PriceCacheWrite, CacheRead: pc.PriceCacheRead,
		}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/provider/ ./internal/config/ ./cmd/...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
gofmt -l .
git add internal/provider internal/config cmd/spore
git commit -m "Price cached input at its own rates

Cost bills three input buckets rather than one. price_cache_write and
price_cache_read default to 1.25x and 0.10x of price_in, so configs written
before caching existed stay correct without an edit."
```

---

### Task 6: Persist the counters and show them

**Files:**
- Modify: `internal/store/schema.go:20-33`, `internal/store/store.go` (`Message`, `AppendMessage`, `Open`)
- Modify: `internal/store/usage.go:8-17`
- Modify: `internal/agent/agent.go:166-176`
- Modify: `internal/daemon/sessions.go:37,221`
- Modify: `cmd/spore/tui.go:798-836`
- Test: `internal/store/store_test.go`, `internal/store/usage_test.go`

**Interfaces:**
- Consumes: `provider.Usage` cache counters (Task 4).
- Produces: `store.Message.TokensCacheWrite`, `.TokensCacheRead`; `store.UsageRow.TokensCacheWrite`, `.TokensCacheRead`; JSON keys `tokens_cache_write`, `tokens_cache_read`.

- [ ] **Step 1: Write the failing tests**

```go
// A database written before caching existed must gain the columns on open.
// The migration is only real if Open calls it: a migration function that
// exists and is never called is the defect that shipped on the last change.
func TestMessagesGainCacheTokenColumns(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.AppendMessage(ctx, Message{
		SessionID: sid, Role: "assistant", BlocksJSON: []byte(`[]`),
		Model: "m", TokensIn: 3, TokensOut: 4,
		TokensCacheWrite: 100, TokensCacheRead: 900,
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.SessionUsage(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d usage rows, want 1", len(rows))
	}
	if rows[0].TokensCacheWrite != 100 || rows[0].TokensCacheRead != 900 {
		t.Fatalf("cache tokens = %d/%d, want 100/900",
			rows[0].TokensCacheWrite, rows[0].TokensCacheRead)
	}
}

func TestOpenAddsCacheColumnsToAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		session_id TEXT NOT NULL, seq INTEGER NOT NULL, role TEXT NOT NULL,
		blocks TEXT NOT NULL, model TEXT NOT NULL DEFAULT '',
		call_site TEXT NOT NULL DEFAULT '', tokens_in INTEGER NOT NULL DEFAULT 0,
		tokens_out INTEGER NOT NULL DEFAULT 0, cost_usd REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL, UNIQUE (session_id, seq))`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if _, err := st.TotalUsage(context.Background()); err != nil {
		t.Fatalf("TotalUsage on a migrated database: %v", err)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 -run 'TestMessagesGainCacheTokenColumns|TestOpenAddsCacheColumns' ./internal/store/`
Expected: FAIL — `unknown field TokensCacheWrite`.

- [ ] **Step 3: Migrate, store, and read back**

Add `migrateMessages` to `internal/store/store.go`, in the shape of `migrateRecallSync`:

```go
// migrateMessages adds the cache token columns when they are missing. They are
// not in schemaSQL: messages ships in databases written before caching
// existed, and CREATE TABLE IF NOT EXISTS leaves those alone. Keeping the
// columns in one place means a fresh database and an upgraded one cannot
// disagree about them.
func migrateMessages(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return fmt.Errorf("inspect messages table: %w", err)
	}
	defer func() { _ = rows.Close() }()
	have := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, col := range []string{"tokens_cache_write", "tokens_cache_read"} {
		if have[col] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE messages ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add messages.%s: %w", col, err)
		}
	}
	return nil
}
```

**Call it in `Open`, beside `migrateRecallSync`, after `db.Exec(schemaSQL)`:**

```go
	if err := migrateMessages(db); err != nil {
		_ = db.Close()
		return nil, err
	}
```

Add the fields to `Message` (`TokensCacheWrite`, `TokensCacheRead int`), add both columns to `AppendMessage`'s INSERT and its value list, and extend `usage.go`:

```go
type UsageRow struct {
	Model            string  `json:"model"`
	Turns            int     `json:"turns"`
	TokensIn         int     `json:"tokens_in"`
	TokensOut        int     `json:"tokens_out"`
	TokensCacheWrite int     `json:"tokens_cache_write"`
	TokensCacheRead  int     `json:"tokens_cache_read"`
	CostUSD          float64 `json:"cost_usd"`
}

const usageSelect = `SELECT model, count(*), sum(tokens_in), sum(tokens_out),
  sum(tokens_cache_write), sum(tokens_cache_read), sum(cost_usd)
  FROM messages WHERE tokens_in > 0 OR tokens_out > 0 OR cost_usd > 0`
```

and the matching `rows.Scan(&r.Model, &r.Turns, &r.TokensIn, &r.TokensOut, &r.TokensCacheWrite, &r.TokensCacheRead, &r.CostUSD)`.

- [ ] **Step 4: Carry them from the turn to the row**

`internal/agent/agent.go:171`:

```go
	_, err = a.Store.AppendMessage(ctx, store.Message{
		SessionID: sessionID, Role: string(role), BlocksJSON: raw,
		Model: model, CallSite: site, TokensIn: u.InputTokens, TokensOut: u.OutputTokens,
		TokensCacheWrite: u.CacheWriteTokens, TokensCacheRead: u.CacheReadTokens,
		CostUSD: cost,
	})
```

`internal/daemon/sessions.go:37` gains the two fields with `json:"tokens_cache_write,omitempty"` and `json:"tokens_cache_read,omitempty"`, and `:221` populates them from `m.TokensCacheWrite` / `m.TokensCacheRead`.

- [ ] **Step 5: Show the share in `/usage`**

In `renderUsage` (`cmd/spore/tui.go:798`), sum the two new keys alongside the others and add one line after `tokens out`:

```go
	if cacheRead+cacheWrite > 0 {
		total := tokensIn + cacheRead + cacheWrite
		share := 0
		if total > 0 {
			share = cacheRead * 100 / total
		}
		b.WriteString("    cache: " + strconv.Itoa(cacheRead) + " read, " +
			strconv.Itoa(cacheWrite) + " written (" + strconv.Itoa(share) + "% of input)\n")
	}
```

- [ ] **Step 6: Run everything**

Run: `go test -tags sqlite_fts5 ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
gofmt -l .
git add internal/store internal/agent internal/daemon cmd/spore
git commit -m "Record and report the cache counters

The counters are the only signal that caching works: when it breaks, requests
keep succeeding and the bill goes up quietly. The documented failure mode is
not a bad first implementation but a regression later, so the numbers are
persisted and /usage shows the read share.

migrateMessages is wired into Open at the call site."
```

---

### Task 7: The standing regression guard

The test that would have caught the failure this feature is most likely to suffer: someone adds a dynamic value to the system prompt, every request misses, and nothing says so.

**Files:**
- Test: `internal/agent/caching_test.go` (create)

**Interfaces:**
- Consumes: everything from Tasks 1-5.

- [ ] **Step 1: Write the test**

```go
package agent

// Two turns of the same session must send a byte-identical prefix, or every
// request after the first pays full price and nothing announces it. This is
// the guard against a later change -- a timestamp in the system prompt, a tool
// list that stopped being sorted -- quietly turning caching off.
func TestTwoTurnsShareTheSamePrefix(t *testing.T) {
	first := Assemble(Snapshot{
		System:      "SYSTEM",
		Environment: "files: one.go",
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "one"}}},
		},
	}, config.ContextConfig{})

	second := Assemble(Snapshot{
		System:      "SYSTEM",
		Environment: "files: one.go two.go", // the workspace changed
		Messages: []provider.Message{
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "one"}}},
			{Role: provider.RoleAssistant, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}},
			{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "two"}}},
		},
	}, config.ContextConfig{})

	want := renderPrefix(first)
	got := renderPrefix(second)
	if !strings.HasPrefix(got, strings.TrimSuffix(want, "|#|")) {
		t.Fatalf("turn 2 does not extend turn 1's prefix:\n turn 1: %q\n turn 2: %q", want, got)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test -tags sqlite_fts5 -run TestTwoTurnsShareTheSamePrefix ./internal/agent/`
Expected: PASS. It should pass on the first run — Tasks 1-2 built the property; this test pins it so a later change cannot quietly remove it.

- [ ] **Step 3: Run every gate**

```bash
make fmtcheck vet lint vulncheck tidycheck test
```
Expected: all pass.

- [ ] **Step 4: The manual live check**

The strongest assertion needs a paid API call, so it is a manual step rather than a test. Against a real key:

```bash
spore serve &
# send two turns in one session, then:
spore usage
```

Expected: the second turn reports a non-zero cache read. If `cache: 0 read` after two turns, caching is not working — check the prefix bytes with the test helper before assuming the API is at fault. Record the observed numbers in the PR description.

- [ ] **Step 5: Commit**

```bash
gofmt -l .
git add internal/agent
git commit -m "Pin the prefix-stability property with a standing test

The costliest caching failure is silent: requests keep succeeding and the bill
is just higher. This asserts that turn two extends turn one's prefix rather
than replacing it, so a later change that reintroduces a per-turn value into
the prefix fails the suite instead of going unnoticed for months."
```

---

## Self-Review

**Spec coverage.** Section 3 "One mechanism" → Task 1. "The prefix" → Task 2. "The wire" → Task 3. "Usage and cost" → Tasks 4 and 5. "Observability" → Task 6. "Config" → Tasks 3 and 5. Section 4's five test groups → Tasks 2, 3, 4, 5 and 7, with the live check as Task 7 step 4. Section 5's out-of-scope list is untouched by every task.

**Placeholders.** None: every code step carries the code, every run step carries the command and the expected result.

**Type consistency.** `Block.CacheBreak` (Task 1) is read in Tasks 2 and 3. `Request.System []Block` (Task 1) is produced in Task 2 and consumed in Task 3. `Usage.CacheWriteTokens` / `CacheReadTokens` (Task 4) are consumed in Tasks 5 and 6 under those exact names. `ProviderPrice.CacheWrite` / `CacheRead` (Task 5) match `config.PriceCacheWrite` / `PriceCacheRead` via `wire.go`. `store.Message.TokensCacheWrite` / `TokensCacheRead` and the `tokens_cache_write` / `tokens_cache_read` columns and JSON keys agree across Task 6. `renderPrefix` is defined once, in Task 2's test file, and reused by Task 7 in the same package.
