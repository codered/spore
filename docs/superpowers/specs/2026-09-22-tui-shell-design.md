# spore — the TUI shell

**Date:** 2026-09-22
**Status:** approved (brainstorming dialogue), pending written review
**Replaces:** the Bubble Tea interface in `cmd/spore/tui.go`
**First of three:** (1) this shell, (2) k9s resource views, (3) first-run experience

## 1. What this adds

`spore chat` today is an inline interface: finished lines are printed into the
terminal's scrollback with `tea.Println` and can never change afterwards. It
cannot re-wrap on resize, collapse a tool result, show more than one session,
or stop a turn.

This change replaces it with a full-screen interface modelled on two tools a
terminal developer already knows:

- **herdr** gives the layout: a sidebar of sessions grouped by workspace, each
  marked working, blocked or idle, and a status bar along the bottom.
- **k9s** gives the navigation: vim-style modes, `:` commands, `/` filter,
  `j/k`, `esc` back.

It also adds the two daemon capabilities the layout needs: one event feed for
every session, and stopping a turn.

The plain line-at-a-time loop (`chatPlain`) is unchanged and remains what runs
when either end of the CLI is not a terminal.

### Out of scope

- The resource views (`:skills`, `:mcp`, `:jobs`, `:policy`, `:recall`,
  `:agents` as tables). That is spec 2; this spec builds the command line they
  will hang off.
- Bare `spore` opening the TUI, provider setup, `spore doctor`. That is spec 3.
- A context-window denominator in the status bar. No model's window is recorded
  anywhere in spore today.
- Mouse support beyond wheel scrolling of the transcript.

## 2. Architecture

### 2.1 Package

The interface moves out of `package main` into **`internal/tui`**, which never
imports `cmd/spore`. It talks to the daemon through an interface:

```go
type Backend interface {
    Sessions(ctx context.Context) ([]Session, error)
    Transcript(ctx context.Context, id string) (Transcript, error)
    Events(ctx context.Context) (<-chan Event, error) // GET /api/events
    Send(ctx context.Context, id, text string) error
    Stop(ctx context.Context, id string) error
    Resolve(ctx context.Context, id string, pendingID int64, ans policy.Answer) error
    CancelAgent(ctx context.Context, parent, child string) error
    NewSession(ctx context.Context, workspace string) (string, error)
    Slash(ctx context.Context, id, cmd string) (SlashResult, error)
}
```

`cmd/spore` adapts its existing `client` to it; tests use a fake. `Events`
returns a channel that closes when the stream ends for any reason.

### 2.2 Units

| file | one job |
|---|---|
| `app.go` | root model: mode, focus, layout arithmetic, dispatching messages to units |
| `cache.go` | client-side state — every session, its blocks, its state. `Apply(Event)` is pure: no Bubble Tea types, no I/O |
| `block.go` | transcript blocks: user, assistant text, tool call (+ its result), approval record, notice, turn footer. `Render(width, expanded)` is memoised per width |
| `sidebar.go` | grouping, nesting, the default filter, `/` filtering, selection |
| `chat.go` | transcript viewport and input textarea |
| `statusbar.go` | the bottom bar |
| `cmdline.go` | `:` input, completion, dispatch |
| `approval.go` | the approval overlay |
| `keys.go` | the keymap and the `?` help overlay |

### 2.3 Data flow

One goroutine reads `Backend.Events` and posts `eventMsg{Session, WireEvent}`
into the program. `Update` hands it to `cache.Apply`; only blocks that changed
are re-rendered. The transcript viewport stays pinned to the bottom unless the
user has scrolled up, in which case the status bar shows `↓ new`.

When the event channel closes, the program posts `streamLostMsg`, shows
`reconnecting…`, and retries with backoff from 0.5s doubling to a 5s cap. On
success it re-fetches `Sessions` and the focused session's `Transcript`,
replaces those entries in the cache, and resumes consuming events. A daemon
restart and an overflow disconnect (§3.1) take the same path.

### 2.4 What happens to `cmd/spore`

- `chat.go`: `chatTUI` becomes an adapter plus `tui.Run(ctx, backend, sessionID)`.
- `tui.go` and `tui_test.go` are deleted.
- `tui_style.go` keeps only what `chatPlain` uses; the rest moves to
  `internal/tui` or is deleted.
- `formatSkills` and `formatAgents` stay in `cmd/spore`: the plain loop uses
  them. The TUI's `Slash` result carries structured data and renders its own.

## 3. Daemon changes

### 3.1 The global feed: `GET /api/events`

- `Hub.SubscribeAll()` returns a channel of events from every session.
  `Publish` delivers to the session's own subscribers and to every global
  subscriber.
- `WireEvent` gains `Session string \`json:"session,omitempty"\``, set only on
  events sent over the global feed.
- On connect, before any live event, the handler sends every pending approval
  across all sessions — the global analogue of what `handleEvents` does per
  session today.
- **Overflow closes, it does not skip.** A per-session subscriber whose buffer
  is full misses events silently, which is acceptable for a transcript the
  client can re-fetch. A global subscriber that missed a `turn_done` would show
  a session as working forever with nothing to tell it otherwise. So a global
  subscriber's buffer is 1024, and when a send to it would block, the hub
  removes and closes it. The client's reconnect path (§2.3) resynchronises.
- Heartbeat as on the per-session stream.

### 3.2 New wire types

Appended to the constant block in `internal/daemon/event.go` (the list is
append-only):

| type | published when | carries |
|---|---|---|
| `turn_started` | `startTurn`, after the turn slot is claimed | — |
| `stopped` | a turn ended because it was stopped | — |
| `session` | a session is created | `Title`, `Workspace`, `Source`, `ParentID` |
| `agent_state` | a sub-agent run starts or settles (§3.5) | `State` (`running`, `done`, `failed`, `interrupted`) |

`WireEvent` gains the fields these carry: `State`, `Title`, `Workspace`,
`Source`, `ParentID`, all `omitempty`.

`turn_done` gains `TokensCacheRead` and `TokensCacheWrite`. Since prompt
caching shipped, `TokensIn` is only the uncached remainder, so the context
figure a client shows is the sum of all three.

### 3.3 Stopping a turn: `POST /api/sessions/{id}/stop`

- `startTurn` derives the turn's context with `context.WithCancelCause(s.base)`
  and stores the cancel function on the session's hub entry beside `running`.
  `Hub.End` clears it.
- `Hub.Stop(id)` calls it with the cause `agent.ErrStopped` and reports whether
  a turn was running. The handler returns 202 when it was, 409 when it was not,
  404 for an unknown session.
- **The cause is what makes it a stop.** Daemon shutdown also cancels every
  turn (through `s.base`), and that must stay an error, not read as the user
  stopping it. So the agent treats a turn as stopped only when
  `errors.Is(context.Cause(ctx), agent.ErrStopped)`. The cause propagates to
  derived contexts, so an `agent_run` child of a stopped turn is stopped too.
- The agent loop distinguishes a stop from failure. When the turn is stopped:
  - if the provider stream was interrupted with text already received, the
    assistant message is persisted with that text followed by
    `\n\n[stopped by you]`, through `persistCtx` as every other write is;
  - it emits `EvStopped` (carrying `Err: agent.ErrStopped`, so the
    supervisor's `drain` still sees a turn that did not finish), which
    `FromAgent` maps to `stopped`, instead of `EvError`;
  - the loop also checks for a stop at the top of each iteration, so a stop
    during tool execution ends the turn after the results are persisted
    instead of making one more provider call.
- Tools already running see a cancelled context. A pending approval returns
  when its context is cancelled — the approver already selects on `ctx.Done()`
  (`approver.go:88`); a test pins it.
- A child run by `agent_run` shares the tool's context and stops with its
  parent. A child launched by `agent_spawn` does not: delegated background work
  is not discarded by stopping the conversation that launched it. It is stopped
  on its own with `x` in the sidebar, through the existing
  `DELETE /api/sessions/{id}/agents/{child}`.
- A stopped turn must leave a transcript the next turn can send. `Snapshot`
  already trims a trailing assistant message whose tool calls never got
  results; a test asserts a stop during tool execution followed by a new
  message succeeds against the fake provider.

### 3.4 Session source and state

- `sessions` gains `source TEXT NOT NULL DEFAULT ''`. The migration adds the
  column and backfills: rows with a `parent_id` become `subagent`, the rest
  `unknown`. It runs on every open, as the existing migrations do, and is
  idempotent.
- `store.CreateSessionFrom(ctx, title, workspace, source)` is added.
  `store.CreateSession` keeps its signature (about a hundred test call sites
  use it) and delegates with `unknown`.
- `Server.CreateSession` gains a `source` parameter and calls
  `CreateSessionFrom`. The four call sites pass: `handleCreateSession` →
  `chat`, the jobs runner → `job`, the Discord bridge (both sites) → `discord`.
  Sub-agents are created by `store.CreateChildSession`, which writes
  `subagent` itself and takes no new argument.
- `GET /api/sessions?children=1` includes sub-agent sessions; without it the
  listing is unchanged.
- `SessionJSON` gains `source`, `parent_id`, `state` (`idle`, `working`,
  `blocked`) and `pending` (count of unresolved approvals). `state` is
  `blocked` when `pending > 0`, else `working` when the hub has a turn
  running — or, for a sub-agent, when its `subagent_runs` row is `running` —
  else `idle`.

### 3.5 Sub-agent visibility

A sub-agent's turns do not go through `startTurn` or the hub: the supervisor
calls `RunSite` and drains the channel itself. Without a change, the global
feed would carry nothing from a child and the sidebar could never show one
working.

- `subagent.Supervisor` gains `SetObserver(Observer)`:

  ```go
  type Observer interface {
      ChildStarted(parentID, childID, prompt string)
      ChildEvent(childID string, ev agent.Event)
      ChildSettled(parentID, childID, state string)
  }
  ```

  `ChildStarted` is called after `StartSubagentRun`; `drain` calls
  `ChildEvent` for every event; `ChildSettled` is called whenever
  `FinishSubagentRun` actually moved the row (so a cancel and the goroutine's
  later no-op write do not both report).
- The daemon implements it: `ChildStarted` publishes `session` and
  `turn_started` under the child's id; `ChildEvent` publishes
  `FromAgent(ev)` under the child's id; `ChildSettled` publishes
  `agent_state` under the child's id. The global feed tags each with the
  child's id, so the TUI streams a child exactly as it streams any session.
- A child's approvals are unchanged: published to the root session with
  `Origin` set to the child, and answered through the root.

## 4. Interaction

### 4.1 Modes

| mode | keys |
|---|---|
| **NORMAL** | `j/k` move the sidebar selection; the chat pane shows the selected session. `i`, `a`, `enter` enter INSERT on it. `ctrl+d/u` half page, `g/G` top/bottom. `[`/`]` move a cursor between tool blocks, `o` toggles the one under it, `O` toggles all. `/` filters the sidebar. `n` new session in the selected session's workspace. `b` selects the next blocked session. `x` on a sub-agent row asks `stop sub-agent 7f3e? y/n` in the command line. `esc` stops the selected session's running turn. `:` enters COMMAND. `?` help. `q` quits. `ctrl+b` toggles the sidebar |
| **INSERT** | as today: `enter` sends, `ctrl+j`/`alt+enter` newline, `↑↓` history at the first/last line, messages sent while a turn runs are queued. Slash commands still work. `esc` returns to NORMAL |
| **COMMAND** | `:new [dir]`, `:sessions` (default filter), `:sessions all`, `:clear`, `:compact`, `:context`, `:usage`, `:skills`, `:agents`, `:q`. Tab completes. `esc` cancels. Spec 2 registers the table views here |
| **APPROVAL** | the overlay on the selected session, entered only from NORMAL: `y` allow once, `n` deny, `s` allow the tool this session, `p` always allow the pattern (offered only when there is one). Any other key does nothing |

`ctrl+c` from any mode clears a non-empty input; otherwise it quits.

### 4.2 Two safety rules

1. **Approval keys are live only in NORMAL.** If an approval arrives while the
   user is in INSERT, focus stays on the input and the overlay reads
   `esc, then y/n/s/p`. Today an arriving approval captures every key, so a `y`
   typed mid-sentence can approve a tool call; that cannot happen here.
2. **Stopping takes `esc` in NORMAL.** Leaving INSERT consumes one `esc`, so an
   accidental stop needs a deliberate double press. The status bar shows
   `esc stop` whenever the selected session is working.

### 4.3 Screen

```
┌ spore ──────────────────────────────────────── :sessions ── ? help ┐
│ ~/dev/spore             │ › fix the flaky tui test                 │
│ ● a1b2 fix flaky  chat  │ Looking at tui_test.go…                  │
│   └ ● 7f3e sub-agent    │ ▸ read cmd/spore/tui_test.go  ✓ 484 ln   │
│ ◐ c9d0 lint audit disc  │ ▸ bash go test ./cmd/spore    ✗ exit 1   │
│ ~/dev/web               │ ──────────────────────────────────────── │
│ ○ e5f6 notes      chat  │ ╭ Ask spore something… ───────────────╮  │
│                         │ ╰─────────────────────────────────────╯  │
├─────────────────────────┴──────────────────────────────────────────┤
│ NORMAL │ a1b2 · chat · ~/dev/spore │ sonnet-5 · ctx 38k · $0.12 · 1 blocked │
└────────────────────────────────────────────────────────────────────┘
```

- **Sidebar**: 30 columns; hidden automatically below 90 columns, `ctrl+b`
  overrides. Grouped by workspace, most recently updated first. `●` working,
  `◐` blocked, `○` idle. Sub-agents indent under their parent.
- **Default filter**: chat sessions, their sub-agents, and every blocked
  session regardless of source. Idle sessions from other sources appear under
  `:sessions all`. Idle sessions beyond ten per workspace collapse to `+N more`.
- **Chat pane**: tool calls render collapsed to one line; `o` expands the full
  arguments and result. Assistant text renders as markdown as it streams:
  completed paragraphs are rendered with glamour, the trailing partial one as
  wrapped plain text, so a half-written code fence never reflows the screen.
- **Status bar**: mode, selected session (short id, source, workspace), then
  model, context tokens, session cost (when `show_cost`), blocked count,
  `↓ new`, `esc stop`, `reconnecting…` as they apply.
- **Too small**: below 60×15 the screen shows only `terminal too small`.
- **Quit**: leaves the alternate screen, then prints the selected session's last
  exchange and `resume: spore chat <id>` to ordinary scrollback.

## 5. Errors

| failure | behaviour |
|---|---|
| event stream ends | `reconnecting…`, backoff 0.5s→5s, resync (§2.3) |
| send returns 409 (a turn started elsewhere) | the message is queued locally and sent when the session is next idle |
| stop returns 409 | notice `nothing running` |
| resolve fails | notice with the error; the overlay stays so the user can retry |
| glamour errors on a block | the block renders as wrapped plain text |
| daemon not running at start | `ensureDaemon`, as today |

A notice is a transcript block in the selected session, never a modal.

## 6. Testing

**`internal/tui`**
- `cache.Apply` table tests over event sequences: a full turn; `stopped`
  mid-text; `resolved` for an approval shown and for one queued; events for an
  unselected session; a resync replacing a transcript that had live text.
- `app.Update` with a fake `Backend`, driving key sequences. Required cases:
  - `y` typed in INSERT while an approval is pending never calls `Resolve`;
  - `esc` in INSERT never calls `Stop`; `esc` in NORMAL on a working session
    calls it exactly once;
  - a queued message is sent after `turn_done` and after `stopped`;
  - `x` on a sub-agent calls `CancelAgent` only after `y`.
- Golden renders at 60, 100 and 160 columns with ANSI stripped: sidebar default
  and `all`, collapsed and expanded tools, the approval overlay in both modes,
  `terminal too small`.

**`internal/daemon` and `internal/store`**
- A global subscriber that falls 1024 events behind is closed, and a per-session
  subscriber in the same situation is not.
- Stop cancels the turn, persists partial text with the marker, and publishes
  `stopped`; a new message afterwards gets a normal turn.
- Stop during tool execution, then a new message, succeeds (§3.3).
- Cancelling the server's base context (shutdown) produces `error`, not
  `stopped`, and writes no `[stopped by you]` marker.
- A sub-agent run through the supervisor with an observer attached reports
  started, its events, and settled exactly once — including when it is
  cancelled.
- A pending approval returns when the turn is stopped.
- Each creation site sets `source`: the test calls the site and reads the row,
  so a site that forgets fails — asserting the call site, not the definition.
- A database created with the previous schema, opened by the new code, has the
  `source` column and backfilled values. This proves the migration is invoked,
  not merely defined.

**End to end**
- The TUI model with the real client against an in-process daemon and the fake
  provider: send, stream, stop, send again, quit.

**Manual gate (in the plan, before the PR)**
- In a real terminal: resize across the 90- and 60-column thresholds, wheel
  scrolling, an approval arriving from Discord while typing, stopping a turn,
  quitting and reading the printed resume line.
