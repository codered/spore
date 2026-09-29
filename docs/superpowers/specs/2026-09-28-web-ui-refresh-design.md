# spore — web UI refresh

**Date:** 2026-09-28
**Status:** from the implementation brief (design canvas, private); written review pending
**Builds on:** `2026-09-22-tui-shell-design.md` (§3.2 context tokens, §4.2 safety
rules), `2026-09-22-tui-views-design.md` (views 2a-1)

## 1. What this changes

`web/` (index.html, style.css, app.js) still looks like Plan 3: a flat session
list, a transcript that prints a tool call and its result as two unrelated
boxes, and approvals with no context. The TUI has since grown a clear
information layout. This change gives the browser the same one:

- the TUI's colours and state glyphs;
- sessions grouped by workspace, sub-agents under their parent, live state from
  the global feed, and a banner when anything waits on a human;
- a header with the facts the TUI header shows, and a Stop button;
- one row per tool call with its result merged in;
- an approval card with origin, rule, profile and a countdown;
- the 2a-1 views (skills, agents, jobs, usage) plus refinements, as tables;
- single-key shortcuts with a per-browser off switch.

The constraints stay: no framework, no build step, embedded through
`web/embed.go`, loopback only, nothing fetched from the network.

### Out of scope

Light theme; the Policy, MCP and Memory views (they need the 2a-2 endpoints,
which do not exist); webfonts; any TUI change; daemon auth or bind address.
Nested `spore.*` calls inside a `go_run` program are not on any event today
(the kernel serves them through the guard without publishing), so the go_run
row shows the program and its result only.

## 2. One daemon change: two optional fields on `approval`

The brief asks for the approval's profile and an auto-deny countdown. Neither
is on the wire, and the browser cannot read `config.toml`. Two append-only
fields fix that:

| field | json | set when |
|---|---|---|
| `Profile` | `profile` | always: the asking session's policy profile |
| `ExpiresAt` | `expires_at` | only while a live waiter holds the ask: RFC 3339 UTC time at which the guard auto-denies |

- `policy.Ask` gains `Profile string` and `Deadline time.Time`; the guard fills
  them from the session and from `askCtx.Deadline()`.
- The broker remembers each waiter's deadline, so the replay paths
  (`pendingApprovalEvents`, `allPendingApprovalEvents`) can set `expires_at`
  too. A suspension replayed after a restart has no waiter and so no timer;
  it gets no `expires_at` and the card says "waiting (no timeout)". Showing a
  countdown there would be false.
- `ExpiresAt` is a string, not a `time.Time`, to keep `WireEvent` trivially
  comparable (event.go's stated invariant).

No route is added or changed.

## 3. Tokens and type

Dark only. `style.css` defines on `:root`:

| token | value | use |
|---|---|---|
| `--bg` | `#0E1312` | page |
| `--panel` | `#121917` | sidebar, cards, composer |
| `--line` | `#26332F` | borders |
| `--text` | `#DCE6E2` | body |
| `--dim` | `#8A9792` | meta, hints, idle |
| `--accent` | `#3DDC97` | working, ok, focus, keys |
| `--tool` | `#C4A2FF` | tool names |
| `--warn` | `#FFB454` | blocked, ask, proposed |
| `--error` | `#FF6B6B` | failed, deny |
| `--visor` | `#67E8F9` | the `›` user marker |
| `--selected` | `#1D2A26` | selected row |

`--mono` is the system monospace stack (ids, tool lines, numbers, tables);
`--sans` the system sans stack (prose, headings). No webfont.

## 4. Layout

```
┌ sidebar ──────────────┬ header: ● title   id · source · ws   model · ctx · $  [Stop] ┐
│ spore   [New session] ├──────────────────────────────────────────────────────────────┤
│ Chat Skills Agents …  │            transcript, centred, max 780px                    │
│ [/ filter]            │   › user message                                             │
│ ⚠ 2 sessions waiting  │   assistant prose                                            │
│ ~/dev/spore           │   ▸ read_file  path=main.go            ✓ 212 lines           │
│  ● fix flaky    chat  │   sonnet · ctx 38k · 91% cached · $0.0123                    │
│    ○ a1b2…      sub   ├──────────────────────────────────────────────────────────────┤
│  ◐ discord q  discord │ approval card (warn border)                                  │
│ scheduled             │ composer                                                     │
│ 127.0.0.1:7777 [⚙]    │ status                                                       │
└───────────────────────┴──────────────────────────────────────────────────────────────┘
```

### 4.1 Sidebar

- Brand and **New session**.
- **View nav**: Chat, Skills, Agents, Jobs, Usage, Refinements. Each shows its
  hotkey as a `<kbd>` hint.
- **Filter** input: case-insensitive substring over title, id, source and
  workspace. A matching child keeps its parent visible.
- **Blocked banner**: `N session(s) waiting on you` when any root session has
  `pending > 0`; click = select the next blocked session (same as `b`).
- **Sessions**, fetched with `GET /api/sessions?children=1`, grouped by
  workspace. Groups are ordered by their most recently updated session;
  sessions inside a group by `updated_at` descending. A session whose
  `parent_id` is present is rendered indented under its parent; a child whose
  parent is not in the list is shown at top level.
- **Session row**: glyph, title (id prefix when untitled), source label
  (`chat`, `discord`, `job`, `sub` for `subagent`). Glyphs: `●` working
  (accent), `◐` blocked (warn), `○` idle (dim).
- **Scheduled**: enabled jobs, `spec — prompt`.
- **Footer**: the daemon address (`location.host`) and a Settings button.

### 4.2 Live state

The page keeps one `EventSource` on `GET /api/events`, and a per-session one
for the open transcript is **not** used: the global feed tags every event with
`session`, so one stream drives both the sidebar and the open transcript. On
every (re)open the page refetches the session list and the open transcript,
because the hub keeps no backlog.

State per session, mirroring `internal/tui/cache.go`:

- `turn_started`, `text` → working.
- `turn_done`, `stopped`, `error` → not working.
- `agent_state` → working iff `state == "running"`.
- `approval` → added to that session's pending list (the event's `session` is
  already the **root**, so a child's approval blocks its root, not itself).
  `resolved` removes it. `stopped`/`error` drop approvals with no `origin`.
  A child settling (`agent_state` not running) drops approvals whose
  `origin` is that child.
- `session` → add/update the row; `session_deleted` → remove it.
- glyph: blocked if its pending list (or the listed `pending`) is non-empty,
  else working, else idle.

### 4.3 Header

Glyph, title, `id · source · workspace`, model and context tokens of the last
turn (`tokens_in + tokens_cache_read + tokens_cache_write`, TUI shell §3.2),
cost summed over the transcript when non-zero, and **Stop** while working.
Stop posts `/api/sessions/{id}/stop`; a 409 sets the status line to
`nothing running`.

### 4.4 Transcript

- Centred column, max width 780px.
- User message: `›` in `--visor`, then the text.
- Assistant prose, pre-wrapped.
- **Tool row**, one per `tool_use`, with the matching `tool_result` (by id)
  merged in. Collapsed:
  `▸ tool  one-line args  ✓ summary` or `✗ error`. The one-line args are the
  JSON input with the outer braces dropped, `key=value` pairs, cut to 80
  characters. The summary is the result's line count (`12 lines`) or its first
  line if it has one line, cut to 60; an error shows its first line. A call
  still waiting shows `…`. Expanded (`<details>`): full pretty-printed args and
  the full result, with `(truncated)` when the daemon truncated it.
- `go_run`: expanded view shows the `code` argument as a program block rather
  than JSON.
- **Turn footer**: `model · ctx 38k · 91% cached · $0.0123` (cost only when
  non-zero; cache % = cache read / whole input, `-` when no cache read).
- `job_note` rows render dim; `error` renders in `--error`.

### 4.5 Approval card

Pinned above the composer with a warn border, one card per pending approval of
the open session:

- `spore wants to run <tool>` (tool in `--tool`).
- `from sub-agent <origin>` when `origin_session` is set.
- `rule <rule> · profile <profile>` when present.
- Args, pretty-printed.
- `auto-denies in 4:32` counting down from `expires_at`; `waiting (no
  timeout)` without it.
- Buttons, each with its key hint: **Allow once** `y`, **Deny** `n`, **This
  session** `s`, **Always `<pattern>`** `p` (only when `pattern` is non-empty).
- Note: `Always allow writes the pattern to the learned block of your policy.`

Keys act on the first card. Answers post to
`/api/sessions/{root}/approvals/{pending_id}` with `{allow, scope}`.

### 4.6 Composer

Placeholder: `Message spore — queued while a turn runs or an approval is
pending (enter to send, shift+enter for a newline)`. Enter sends.

## 5. Keyboard

### 5.1 Setting

`localStorage["spore.keys"] = {"enabled": true, "hints": true}`. Missing,
unparseable or throwing storage (private windows) → defaults. Every read and
write is wrapped in try/catch; a failed write keeps the in-memory value.

The Settings overlay (sidebar button or `,`):

- **Keyboard shortcuts** — `<button role="switch" aria-checked>`. Off: the
  keydown handler returns at once; everything stays clickable.
- **Show key hints** — hides every `<kbd>` hint (a `hints-off` class on
  `<body>`, `kbd.hint { display: none }`). Disabled while shortcuts are off,
  and hints are hidden then too.
- A fixed note: approval keys never fire while a text field has focus.

### 5.2 Handler

One `keydown` listener on `document`:

1. `enabled` false → return.
2. ctrl, meta or alt held → return (browser shortcuts keep working).
3. Target is `input`, `textarea`, `select` or `[contenteditable]` → handle only
   `Escape` (blur) and return.
4. Otherwise:

| key | action |
|---|---|
| `C S A J U R` | open Chat, Skills, Agents, Jobs, Usage, Refinements |
| `,` | settings |
| `?` | shortcut overlay |
| `j` / `k` | next / previous session (sidebar order, visible rows) |
| `b` | next blocked session |
| `n` | new session |
| `/` | focus the filter (the filter of the open view, or the session filter) |
| `i` | focus composer (switches to Chat) |
| `o` / `O` | toggle the tool row under the cursor / all tool rows |
| `Escape` | innermost first: close overlay → close view detail → clear filter → stop the open session's turn |
| `y n s p` | resolve the first visible approval card, only when one is shown |

`n` is "new session" only when no approval card is shown; with a card, `n` is
deny. This is the same key doing the safer thing in the context where both are
possible.

**Cursor**: the tool row last clicked or hovered, else the last tool row in
the transcript.

**Safety rule** (not configurable, TUI shell §4.2): approval keys never fire
while a text field has focus, so a `y` typed mid-sentence cannot approve.
Leaving the composer costs one `Escape`, so stopping a turn from the composer
takes two.

## 6. Views

One table component: title with row count, a `/` filter, click-to-sort
headers (▲/▼), row actions as inline buttons. Destructive actions ask with the
TUI's confirmation text through `confirm()`. Clicking a row opens a detail
pane below the table; `Escape` closes it.

| view | source | columns | actions |
|---|---|---|---|
| Skills | `GET /api/sessions/{id}/skills` | NAME, LOADED, TOKENS, DESCRIPTION; each load error as a `!` row | — |
| Agents | `GET /api/sessions/{id}/agents` | ID, STATE, AGE, COST, PROMPT | Stop (running only; "stop sub-agent <id>?") → `DELETE …/agents/{child}` |
| Jobs | `GET /api/jobs` | ID, KIND, SCHEDULE, NEXT, LAST, PROMPT | Cancel (enabled only; "cancel job <id>?") → `DELETE /api/jobs/{id}`; detail lists runs from `GET /api/jobs/{id}/runs` |
| Usage | `GET /api/usage?session={id}` | session block (per model), then DAY, MODEL, TURNS, IN, OUT, CACHE %, COST | — |
| Refinements | `GET /api/refinements` | ID, STATUS, KIND, TARGET, SESSION, RATIONALE; proposed first | Accept, Reject ("reject refinement <id>?"), Roll back round ("roll back every edit in round <round>?") → `POST /api/sessions/{sid}/refine/rollback`; detail shows before/after |

Session-scoped views use the open session and say so when there is none.

## 7. Tests and gates

- `web_test.go`:
  - index still renders and names the new landmarks (`id="transcript"`,
    `id="sidebar"`, `id="settings"`);
  - static assets still embedded (app.js contains `EventSource` and
    `spore.keys`, style.css contains `--selected`);
  - **route coverage**: every `/api/...` literal in `app.js` must match a
    route registered on the mux. The test extracts string literals starting
    with `/api/`, replaces each concatenated id with a placeholder, and asks
    the mux (`http.ServeMux.Handler`) whether a non-catch-all pattern matches,
    trying every method the app uses. A 2a-2 path such as `/api/policy` fails
    it.
- `approver_test.go`: a live ask carries `profile` and an `expires_at` near
  now + timeout; the replay of a live ask carries the same `expires_at`.
- `make test`, `make vet`, `make fmtcheck`, `make lint` pass.

### Manual gate (before the PR)

1. An approval arrives while typing in the composer; typing `y` puts a `y` in
   the composer and does not approve.
2. `Escape` twice from the composer, with a turn running, stops the turn.
3. Shortcuts off → no key does anything outside fields.
4. Hints off → no `<kbd>` hint visible.
5. A blocked Discord session shows `◐` and the banner.
6. A private window loads with defaults (shortcuts and hints on).

## 8. Addendum after the first manual pass (2026-09-28)

Three findings from the human's first test:

1. **No sign a message went through.** The transcript now ends in a
   `spore is thinking…` row (`spore is writing…` while text streams,
   `spore is running <tool>…` while a call waits) whenever the open session is
   working and not blocked. It appears the moment Send is pressed, not when
   `turn_started` arrives, and it hides while an approval card waits.
2. **Clicking a session while a view was open did nothing visible.** A click
   on a session row now opens Chat on that session. `j`/`k` still keep the
   current view, so a session-scoped view follows the selection.
3. **Every session was called "chat".** The daemon names a session after its
   first turn ends:
   - Only top-level `chat`, `discord` and `unknown` sessions whose title is a
     placeholder (`""`, `chat`, `web`, `new chat`) are named. Jobs keep their
     prompt; sub-agents keep their task.
   - `internal/title` makes one call on the existing `title` router site
     (route it to a cheap model with a `[[router]]` rule if wanted), from
     the session's first user message, capped at 1000 characters. The reply
     is cleaned: `<think>` blocks, a `Title:` label, markdown, quotes and
     closing punctuation are dropped, and the result is bounded to 60 characters.
   - Naming runs after the turn ends, not beside it: on a local model the two
     calls would share one GPU and delay the reply.
   - If the call fails or returns nothing, the first line of the message is
     used (50 characters).
   - `store.RenameSessionFrom` only replaces the placeholder it read, so a
     rename made meanwhile wins. It does not touch `updated_at`.
   - The name reaches clients as a `session` event carrying every field.
   - The title call's tokens are traced but not written to the transcript,
     so `/usage` does not count them.
