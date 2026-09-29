# spore — TUI: match the design canvas

**Date:** 2026-09-29
**Status:** approved scope (items 1–11 below); written review pending
**Source:** the "spore — TUI & Web UI" design canvas (private), boards
"TUI · chat (NORMAL, turn streaming)", "TUI · approval modal" and "Shared
language"
**Builds on:** `2026-09-22-tui-shell-design.md`, the TUI parity commit on
`tui-web-parity` (feafa42), which this partly supersedes (§4)

## 1. What this changes

The design canvas has a chat screen and an approval screen for the TUI. The
TUI has drifted from both. This spec brings the TUI in line with the two
boards, using only the data the TUI already receives. Each item below names
where it lives today and what changes.

The constraints are the same as for the shell: `internal/tui` only, no new
daemon routes and no wire changes. One piece of new wiring goes from
`cmd/spore` into `tui.Options` (§3.1).

### Out of scope

These things are on the boards but cannot be built from what the TUI receives
today. They wait for a later spec:

- live token and cache figures on the working line
  (`tokens 12.4k in / 1.1k out · 92% cache`). Tokens arrive only on
  `turn_done`;
- the rule's source location (`config.toml:31`);
- `inside go_run → spore.Shell` on the approval, and the `spore.*` call
  summary inside an expanded `go_run` row. Nested kernel calls are not on any
  event;
- the `mcp M` and `policy P` tabs, which wait for the 2a-2 endpoints;
- the refinements board (split table and detail pane), which is a separate
  change.

The boards also contain three details this spec does not copy, on purpose:

- **The input placeholder says `/ for commands`.** In the TUI `/` filters and
  `:` runs a command. The placeholder says `:`.
- **The approval footer says `also shown on web and Discord`.** That is true
  only for some sessions. The footer shows the countdown only.
- **The approval status bar says `other keys do nothing`.** That is false:
  `j`/`k`, `[`/`]` and the view keys keep working while an approval waits,
  and this spec does not change that.

## 2. Palette

The boards' "Shared language" board is the source for colour. It differs from
`style.go` in one colour, and it adds three fills the TUI does not have.
Each fill is an `AdaptiveColor`, so a light terminal gets a legible pair.

| token | dark (board) | light | use |
|---|---|---|---|
| `colMuted` | `#8A9792` (was `#8A8F98`) | `#6B7280` (unchanged) | meta, hints, idle |
| `colFillSel` | `#1D2A26` | `#DDEFE7` | selected sidebar row, lit tab |
| `colFillWarn` | `#2A2418` | `#FBEBD3` | selected row while it is blocked |
| `colFillCursor` | `#1A2320` | `#ECF1EF` | tool row under the `[ ]` cursor |
| `colVisor` | `#67E8F9` | `#0E7490` | the `›` marker on your messages |

The `›` user marker moves from accent to `colVisor`, as the board and the web
UI both have it.

## 3. The chat screen

### 3.1 Header (item 1)

- **The blocked count gets its glyph:** `◐ 2 blocked`, in warn.
- **The daemon address goes last on the right, in muted:** `daemon :7777`.
  The TUI does not know the address today, so `tui.Options` gains a
  `Daemon string` field. `cmd/spore/chat.go` sets it from `cfg.Daemon.Addr`.
  A loopback host is dropped, so `127.0.0.1:7777` shows as `:7777`. Any
  other host is shown in full. An empty `Daemon` shows nothing.
- **`reconnecting…` keeps its place,** between the two.
- **When the row is too narrow,** the existing rule applies: the tab bar
  loses its hotkeys first. After that, `daemon …` is the first thing on the
  right to go.

### 3.2 Sidebar (item 2)

- **The selected session row gets a filled background** instead of reverse
  video. The fill is `colFillSel`, or `colFillWarn` while that session is
  blocked (item 9). The text keeps its own colours: the glyph stays green,
  amber or muted, and the id and tag stay muted. The cursor on the jobs
  folder or a job label uses the same fill. Reverse video goes, because it
  hides the state glyph's colour.
- **A legend goes on the last row of the pane:** `● working  ◐ blocked
  ○ idle`, in muted, with each glyph in its state colour. It is drawn only
  when the rows leave at least one blank row above it. Otherwise the rows
  win and the legend is hidden.
- **Two things on the board already exist and do not change:** the
  `+N more` row (`idleShownPerWorkspace`) and the unread badge on `▸ jobs`.

### 3.3 Session facts line (item 3)

The board ends the line with `· $0.12`. The TUI already does this when
`show_cost` is on (`sessionFacts`). The gate stays, because cost is `$0.00`
for the local models most sessions use. Nothing changes here.

### 3.4 Tool rows (item 4)

Today a row is `▸ bash {args}  ✓ 3 lines`, with the status right after the
args. The board lays it out as three columns: the tool name, the args filling
the row, and the status against the right edge.

- **Status against the right edge.** `drawTool` pads the args so the status
  ends at the right edge of the transcript.
- **Duration after the status,** in muted: `✓ 41 lines  1.3s`. `block`
  gains `startedAt` and `doneAt`. `cache.Apply` sets `startedAt` from
  `c.now()` on `tool_call` and `doneAt` on `tool_result`. A row rebuilt
  from a stored transcript has neither, so it shows no duration. Format:
  `0.4s` under 10 s, `12s` under a minute, `1m05s` above.
- **Cursor row.** The row under the `[ ]` cursor gets the `colFillCursor`
  background across the full width, and its `▶` marker. On the board that
  row is also the failed one, but the highlight means the cursor, not
  failure; failure stays `✗` in danger.
- **An expanded row** uses `▾` instead of `▸`. Its body (args, then result)
  is indented two columns under the name, with a muted `│` rule running down
  its left edge, instead of today's four-space indent.

### 3.5 Working line (item 5)

This replaces the pinned row from feafa42. The board puts it in the
transcript, as its last line, after one blank line:

```
⠹ thinking · 14s
⠹ writing · 14s
⠹ running bash · 14s
```

- **Spinner.** The `⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏` frames, in accent, with the rest in muted.
- **Label.** Worked out as feafa42 does it: `writing` while the last block
  is streaming text, `running <tool>` while the trailing run of tool rows
  has one with no result, and `thinking` otherwise.
- **Elapsed time.** From `started`, shown once `turn_started` has landed,
  in the format of §3.4. `submit` clears `started`, as it does now.
- **When it shows.** While the selected session is working, from the moment
  a message is sent. It hides while an approval waits, as it does now.
- **Where it lives.** It is part of the transcript string, so it scrolls with
  the chat. The line numbers `toolLines` records are unaffected, because it
  comes after every block.

**Tick.** The one-second tick from feafa42 becomes a frame tick. It runs
every 120 ms while the working line is on screen, so the spinner turns, and
every second while only an approval countdown needs it. `armSecond` keeps its
rule of at most one tick in flight, and picks the interval when it arms. The
frame index is `now / 120ms`, so frames do not depend on how many ticks
arrived.

**Cost.** Each frame changes the transcript string, which means a
`vp.SetContent`. Blocks cache their rendering, so a frame is a join and a
viewport reset, but a long transcript makes that join longer. The plan
includes a benchmark of `sync` on a 2,000-block transcript. If a frame costs
more than 2 ms there, the spinner drops to one frame per second, and nothing
else changes.

### 3.6 Input placeholder (item 6)

- **In NORMAL:** `Ask spore something…  (i to type · : for commands)`, with
  the brackets part in a dimmer muted.
- **In INSERT:** `Ask spore something…`, as now.

The placeholder is swapped when the mode changes.

### 3.7 Status bar (item 7)

- **Hints drop the angle brackets:** `i type`, not `<i> type`. The key stays
  in accent bold and the label in muted, and hints are separated by two
  spaces. This applies to every mode and to the confirm modal's answer line.
- **Chat-focused NORMAL shows, in this order:** `tab sessions` (only while
  the sidebar is on), `i type`, `j/k scroll`, `[ ] tools`, `o expand`,
  `n new`, `b next blocked` (only while something else is blocked), `: cmd`,
  `? help`.
- **Sidebar-focused NORMAL** keeps its current set, without brackets.
- **When the row is too narrow,** hints are dropped from the end of the list,
  except `? help`, which is always kept. This replaces today's `…`
  truncation of the left half.
- **The right side** keeps its signals. `esc stop` becomes warn bold (was
  accent key and muted label).

## 4. The approval screen

Today the approval is a warn-bordered box stacked above the input, and the
transcript shrinks to make room. The board draws it as a card centred in the
chat pane, over a dimmed transcript, and colours the pane, the sidebar row
and the status bar to say the session is waiting on you. The chat pane's
layout does not change while an approval waits. The input keeps its place and
the viewport keeps its height.

### 4.1 The card (items 8 and 11)

The card is drawn with `placeOver` onto the viewport's rows. Its frame is a
double border in warn, with one row and two columns of padding. Its width is
`min(76, viewport width − 4)` columns.

The card's rows, in order:

1. **Title,** in warn bold: `spore wants to run shell_exec`.
2. **Origin and profile,** in muted: `from sub-agent 7f3e · profile local`.
   Each part appears only when it is known. The row is left out when both
   are empty.
3. **Rule,** in muted: `matched ask  <rule>`, where `ask` is in warn. The
   event does not carry a decision, but an approval is always an ask. When
   the rule is `policy.default`, the row reads
   `no rule matched · default is ask`.
4. **A blank row.**
5. **What the call does,** in a box with a `colFillCursor` background:
   - **For `shell_exec`:** `$ <command>`, and then `timeout <n>s` in muted
     when `timeout_seconds` is set. There is no cwd row: the shell tool's
     arguments carry no directory. Its working directory is the session's
     workspace, which the session facts line already shows.
   - **For any other tool:** `prettyArgs`, as now.

   Either way, the box holds at most 8 lines, clipped with `… N more lines`.
6. **A blank row.**
7. **The keys:** `y allow once   n deny   s allow <tool> this session`. Then,
   on its own row when there is a pattern: `p always allow <pattern>`, with
   the pattern in accent.
8. **A blank row, then the deadline,** in muted: `auto-deny in 4:32`,
   `waiting (no timeout)` or `auto-denying…`, worked out as feafa42 does
   (with `auto-denies` shortened to `auto-deny`, as on the board).

**Rows under the card are faint.** The viewport's rows are drawn with faint
applied while the card is up, so the transcript reads as behind it. The
card's own rows are not faint.

**Placement.** The card is centred on the viewport. When the viewport is
shorter than the card, the args box gives up rows first, down to one. If it
still does not fit, the card is pinned to the top of the viewport, and the
bottom is clipped by the pane. The minimum terminal size (60×15) is not
changed.

**Answering.** Keys do not change. `y`/`n`/`s`/`p` in NORMAL open the
existing confirm modal (thick warn border, centred on the whole screen), and
`y` there answers. The confirm modal draws over the card, as it draws over
the body today.

### 4.2 Pane and sidebar while blocked (item 9)

These apply while the selected session is blocked, whether by its own
approval or by one of its sub-agents' approvals (`cache.approvalFor`):

- **The chat pane's border is warn** and heavy, whether or not the pane has
  focus. Blocked outranks focus, because the person has to act.
- **The pane title reads** `a1b2 fix flaky test · waiting on you`, in warn.
- **The selected sidebar row uses the `colFillWarn` fill** (§3.2).

### 4.3 Status bar while blocked (item 10)

This applies in NORMAL, with no view open, while the card is showing. The
mode badge reads `APPROVAL`, on a warn background instead of accent. The mode
stays `modeNormal` in code, so only the badge text and colour change. The
hints become:

- `y once`, `n deny`, `s session`, and `p pattern` (only when there is a
  pattern);
- `b next blocked (c9d0)`, naming the next blocked session's short id, and
  shown only when there is one.

`esc stop` stays on the right, since stopping the turn is still how to
abandon it.

### 4.4 A draft while an approval waits (item 11)

Today, in INSERT, the approval box swaps its key row for `esc, then y/n/s/p`.
The card no longer does that: it always shows the keys. Instead, while an
approval waits for the selected session and the mode is INSERT, one line in
warn goes under the input:

- `draft kept · approval keys work in NORMAL — esc, then y/n/s/p`, when the
  input is not empty;
- `approval keys work in NORMAL — esc, then y/n/s/p`, when it is empty.

That line takes one row from the viewport.

## 5. Code layout

Nothing in the model's state or message flow changes, apart from the tick
interval. The changes are to rendering:

- **`style.go`:** the palette in §2, `styApprovalCard` (double border), and
  the badge variants for `APPROVAL`.
- **`header.go`:** `globalFacts` (§3.1); `hint` without brackets; `keyHints`
  with the drop-from-the-end fitting and the blocked set (§3.7, §4.3).
- **`sidebar.go`:** the fill in `sessionRow` and `highlight`, and the legend
  (§3.2).
- **`block.go`:** the tool row's columns, duration, cursor fill and expanded
  rule (§3.4). The `›` colour.
- **`cache.go`:** tool `startedAt`/`doneAt`.
- **`view.go`:**
  - `transcript` appends the working line (§3.5);
  - `overlayView` becomes `approvalCard`, drawn over the viewport (§4.1);
  - `paneBox` takes the blocked state (§4.2);
  - the INSERT draft line (§4.4);
  - `thinkingView` and `thinkingHeight` go.
- **`app.go`:**
  - the tick interval (§3.5);
  - the placeholder swap (§3.6);
  - `Options.Daemon`.
- **`cmd/spore/chat.go`:** passes `cfg.Daemon.Addr`.

The card goes in a new `approval.go` rather than growing `view.go` further.

## 6. Testing

- **Golden screens** (`view_test.go`): every existing golden is regenerated,
  and each diff is read before it is accepted. Three are added:
  `working-100` (a turn mid-stream, with a tool row showing a duration),
  `approval-card-60` (the card squeezed at the minimum size), and
  `approval-subagent-100` (origin, profile and a `p` pattern).
- **Unit tests:**
  - the header address rule (loopback dropped, other hosts kept, empty
    hidden);
  - hint fitting keeps `? help` at every width from 60 to 200;
  - duration formats;
  - `startedAt`/`doneAt` from events, and none from a stored transcript;
  - card rows for `policy.default`, for a sub-agent origin, and for an empty
    profile;
  - `shell_exec` args rendering, with and without `timeout_seconds`;
  - the card shrinks its args box before pinning to the top;
  - the tick interval is 120 ms while working and 1 s while only an
    approval shows.
- **The feafa42 tests** in `parity_test.go` are rewritten for the new label
  format and position. What they check stays: labels, hidden while blocked,
  no stale time, and the countdown.
- **Benchmark:** `sync` on a 2,000-block transcript (§3.5).
- **CI gate:** the full set, with `-tags sqlite_fts5` on vet and test.
- **Manual check:** `spore chat` in a dark terminal and a light one:
  1. send a message and watch the working line;
  2. trigger a `shell_exec` approval and check the card, the amber pane, the
     row and the badge;
  3. type a draft in INSERT and check the line under the input;
  4. answer, and check everything returns to normal.

  Compare the screens with the two boards side by side.
