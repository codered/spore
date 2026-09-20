# spore — prompt caching

**Date:** 2026-09-20
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-08-29-spore-core.md` (the assembled request), `docs/technical-prd.md`
section 8 (Phase 3, caching)

## 1. What this adds

Every turn resends the whole prompt: the system prompt, the environment
section, the memory facts, the skills index, the compaction summary and the
entire live message tail. All of it is billed at full input price on every
request. The Anthropic Messages API will cache a prefix of that and charge a
fraction to read it back, and spore asks for none of it.

This change asks for it. Two `cache_control` breakpoints go on the request --
one at the end of the stable system blocks, one that moves forward with the
conversation -- and the prompt is reordered so those breakpoints have something
stable in front of them.

### The reordering is the change

Caching is a prefix match: one byte changes and everything after it is
invalidated. `agent.Assemble` (`internal/agent/context.go:170`) concatenates
into one system string in this order:

```text
system prompt  ->  Environment  ->  facts  ->  skills  ->  summary
```

`Environment` is, by its own comment, "rebuilt per turn rather than stored,
because it describes the machine as it is now, not as it was when the session
started". It sits at position two. Any turn where a file changed in the
workspace -- the normal case while coding -- invalidates the facts, the skills
index and the summary behind it.

So markers alone would buy close to nothing here, and would add the 1.25x write
premium on bytes that are never read back. The fix is structural: stable
content first, volatile content after the last breakpoint.

## 2. Decisions

| Question | Answer |
|---|---|
| Where does the environment go? | **After the cached prefix**, appended to the final user message at assembly time. This is what makes caching work at all. It changes what the model sees on every turn, which is accepted. |
| How are cached tokens priced? | **Explicit config fields**, `price_cache_write` and `price_cache_read`, defaulting to 1.25x and 0.10x of `price_in`. The multipliers are not universal -- Fable 5.1 reads at 0.025x -- and an operator should not wait for a spore release to price their model. |
| How many breakpoints? | **Two.** One on the last stable system block, giving the expensive shared part a guaranteed read point; one on the last block of the newest turn, moving forward so the prior conversation is read and only the new turn is written. Two of four slots. |
| How is it turned on? | **On by default** for a provider of type `anthropic`, with `cache = false` as the escape hatch. Cheaper and faster is the right default; the hatch exists for a proxy that rejects the field. |

## 3. Architecture

### One mechanism

`provider.Block` gains one field and `Request.System` stops being a string:

```go
// CacheBreak asks the provider to place a cache breakpoint after this block.
// It is never persisted: where the breakpoint goes is a fact about one
// request, not a property of a stored message.
CacheBreak bool `json:"-"`
```

```go
type Request struct {
	Model       string
	System      []Block // was: string
	Messages    []Message
	Tools       []ToolSpec
	MaxTokens   int
	Temperature float64
}
```

`json:"-"` is load-bearing. Messages persist as blocks JSON in
`messages.blocks`; a breakpoint baked into stored history would be wrong on
replay and would itself be a silent prefix change. The flag is computed by
`Assemble` on every request.

One field expresses both breakpoints. A second mechanism for the message tail
would be two things to keep consistent.

### The prefix

`Assemble` builds four system blocks in stability order and marks the last
non-empty one:

```text
[system prompt] [skills index] [facts] [summary]#
```

The environment is appended as a text block on the end of the final user
message, after that message's breakpoint:

```text
turn 3:  [system blocks]#   u1 a1 u2 a2 [u3]#[env]
```

**The environment is never stored.** It comes from `snap.Environment`, which is
rebuilt per turn, and `Assemble` injects it into the outgoing request only.
Turn 4 does not carry turn 3's environment, so the prefix turn 4 reads is
byte-identical to what turn 3 wrote. Appending it to the stored message instead
would drag a stale environment into the prefix and grow it every turn for
nothing.

The final message may be a tool-result message; a text block after the
`tool_result` blocks in the same user message is valid and is where the
environment goes in that case.

Facts stay inside the cached prefix even though `memory_write` changes them.
That is deliberate: a fact write is rare, and the turn that does it takes one
full-price re-read and re-warms the prefix. Moving facts after the breakpoint
to dodge that would cost more than it saves, because facts are re-sent every
turn either way.

### The wire

`toWire` gains one rule: a block with `CacheBreak` renders
`"cache_control": {"type": "ephemeral"}`. `system` becomes an array of text
blocks. Nothing else about the body changes.

The TTL is the 5-minute default and there is no knob. A cache read refreshes the
entry's timer for free, so turns starting less than five minutes apart keep it
warm indefinitely -- which is what an interactive loop looks like. The 1-hour
TTL doubles the write premium to 2x and only pays off in the 5-to-60-minute
gap; past an hour the first turn back is a cold miss either way.

`openaicompat` ignores `CacheBreak` and joins the system blocks into its single
system string, which is what `toWire(req.System, req.Messages)`
(`openaicompat.go:81`) already wants. OpenAI-compatible endpoints cache
automatically server-side with no client control, so there is nothing to
express there.

### Usage and cost

The SSE parser already reads usage from `message_start`
(`internal/provider/anthropic/anthropic.go:255`). Both cache counters arrive in
that same object.

```go
type Usage struct {
	InputTokens      int // the uncached remainder only
	OutputTokens     int
	CacheWriteTokens int
	CacheReadTokens  int
}
```

`InputTokens` is the remainder, not the prompt size: the total is the sum of
all three. Nothing in spore currently reads it as a prompt size --
`internal/agent/context.go` estimates from text -- but the field's meaning
changes and the comment says so.

Pricing is operator-set, per provider block:

```toml
price_in          = 5.0
price_out         = 25.0
price_cache_write = 6.25   # defaults to 1.25 x price_in when unset
price_cache_read  = 0.50   # defaults to 0.10  x price_in when unset
```

`ProviderPrice` carries the two extra rates and `Cost` bills each bucket at its
own rate. Existing configs keep working and get correct costs with no edit.

Sub-agent budgets are enforced on recorded cost, so getting this wrong in the
other direction matters too: counting cache reads as full-price input would cut
trees off early on spend that never happened.

### Observability

`usage` gains `tokens_cache_write` and `tokens_cache_read` beside
`tokens_in`, `tokens_out` and `cost_usd` (`internal/store/schema.go:28`),
through a migration in the shape of `migrateRecallSync` -- **wired into `Open`
at the call site**, which is the step that was missed on the delete path and
would have shipped a column nothing could read.

`/usage` gains one line showing the cache-read share.

This is deliberate scope. The usage counters are the only signal that caching
works: when it breaks, requests keep succeeding and the bill goes up quietly.
The documented failure mode is not a bad first implementation but a regression
later -- someone adds a dynamic field to the system prompt, and every request
misses from then on. Without the columns spore cannot notice.

### Config

`cache` on a `[[provider]]` block, defaulting to true for type `anthropic`.
When false, `Assemble` still orders the prompt the new way -- the ordering is
an improvement on its own -- but no breakpoints are emitted.

## 4. Testing

- **`Assemble`**: the environment lands at the tail of the final user message
  and nowhere else; exactly two breakpoints; the breakpoint sits before the
  environment block; stored history never contains an environment block; and
  **two consecutive assemblies with a changed environment produce
  byte-identical prefixes up to the last breakpoint**. The whole design rests
  on that last property, so it is asserted directly rather than inferred.
- **Wire**: golden JSON for a two-turn request -- `cache_control` on the last
  system block and on the last pre-environment message block, nowhere else. And
  with `cache = false`, nowhere at all.
- **Usage**: an SSE fixture carrying both cache counters parses into `Usage`;
  the existing fixtures still parse, with zeros.
- **Cost**: `Cost` against hand-computed values, including the defaulting rule
  when the two new config fields are unset.
- **The standing check**: a test against an `httptest` server that records
  request bodies and asserts the second request's prefix bytes match the
  first's up to the breakpoint. An assertion on `cache_read_input_tokens > 0`
  against the real API would be stronger, but it needs a paid call, so it is a
  manual verification step recorded in the plan rather than a test.

A green suite is not on its own evidence that caching works. The only ground
truth is the usage counters, which is why they are persisted and surfaced.

## 5. Out of scope

- **Sub-agent prefix sharing.** A child reusing the parent's exact `system`,
  `tools` and model would read the parent's cache. Children build their own
  prompt and tool set today; that is its own change.
- **The `role: "system"` operator channel.** It would suit the environment
  block and is the prompt-injection-safe way to inject operator context, but it
  returns 400 on models that do not support it, Sonnet 5 among them, and spore
  points at arbitrary providers.
- **A 1-hour TTL knob**, per the reasoning above.
- **Per-model minimum-prefix guards.** The minimum is 512 tokens on Opus 5,
  1024 on Sonnet 5, 4096 on Haiku 4.5, and under it the API silently declines
  to cache with no error. A per-model table would rot; spore reports what
  actually happened from the usage counters instead.
- **Caching for `openaicompat`**, which has no client-side control.
- **`compact.go`'s request**, which builds its own small prompt and would need
  the fork-reuse treatment to benefit.
