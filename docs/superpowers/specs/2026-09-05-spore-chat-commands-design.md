# Chat Commands (Design)

## Goal

Four slash commands in the interactive chat client: `/clear`, `/compact`,
`/context`, `/usage`. Commands are user-facing shortcuts over existing daemon
state, not new subsystems. `/skills` (Claude-Code-style SKILL.md) is a
separate, larger subsystem deferred out of scope.

## Decisions

| Question | Answer |
|----------|--------|
| Where do commands live? | Client-side interception; `/compact` gets one new API endpoint |
| `/clear` semantics? | Moves the summary boundary to now; transcript never deleted |
| `/skills` meaning? | Full subsystem (SKILL.md folders), deferred |
| Hybrid approach? | Only `/compact` needs a new endpoint; others use existing ones |

## Architecture

Commands are intercepted client-side before they reach `c.send()`. The
Bubble Tea model (`chatUI`) sees the raw input, recognises a leading `/`,
dispatches locally, and never posts the message to the daemon. The one
exception is `/compact`, which triggers agent code on the server.

No subsystems are added. Commands are thin, testable shell behaviour over
existing endpoints.

## Commands

### `/clear`

Moves the summary boundary to "now": every message in this session is folded
into the summary, and Snapshot skips rows at or below the boundary.

Client-side: computes the latest `seq` for the session, then calls the
existing `PATCH /api/sessions/{id}` endpoint with a new `summary_through`
field. The transcript is untouched; only `summaries.through_seq` moves.

No bridge complication: the session stays the same, so `bridge_bindings`
is unaffected.

### `/compact`

Calls `MaybeCompact` on demand. The auto-threshold (0.75 of
"context.compact_at") remains, so `/compact` is a manual override, not a new
mechanism.

New API endpoint: `POST /api/sessions/{id}/compact` → calls
`Agent.MaybeCompact(ctx, sessionID)`. Returns a JSON body with the new
summary boundary and token estimate, mirroring what `Snapshot` would report.

### `/context`

Client-side: `GET /api/sessions/{id}` (already returns the transcript and
session record), then computes `SnapshotTokens` locally and renders the
breakdown:

- System prompt: `~N tokens`
- Environment section: `~N tokens`
- Facts section: `~N tokens`
- Summary: `~N tokens`
- Live messages: `~N tokens`
- Total: `~N / B tokens` (where B is context.max_tokens)

`SnapshotTokens` is already exported from `internal/agent` and takes a
`Snapshot` plus `ContextConfig`. The client assembles the same `Snapshot` from
the API response and runs the estimate.

### `/usage`

Client-side: `GET /api/sessions/{id}` (transcript includes tokens/cost per
message), aggregates:

- Session totals: tokens in, tokens out, cost
- Running totals: computed over all messages in the transcript

Renders as a table (session vs. total), plus the count of turns.

### `/skills`

Deferred. Claude-Code-style SKILL.md folders are a full subsystem: folder
scanning, skill loading with triggers, tool exposure changes, spec sections
for the format and lifecycle. Out of scope for this design. The backlog
entry stays open for the brainstorming session it deserves.

## Data Flow

```
User types "/clear"
  → chatUI.handleKey sees leading "/"
  → submits to /api/sessions/{id} (PATCH summary_through)
  → shows confirmation in transcript

User types "/compact"
  → chatUI.handleKey sees leading "/"
  → POST /api/sessions/{id}/compact
  → daemon calls MaybeCompact
  → returns boundary + token estimate
  → chatUI displays the result

User types "/context" or "/usage"
  → chatUI.handleKey sees leading "/"
  → GET /api/sessions/{id} (existing)
  → computes tokens/cost locally
  → renders in transcript
```

## API Changes

One new endpoint:

```
POST /api/sessions/{id}/compact
Response: { "through_seq": N, "tokens": N }
```

No new endpoint for `/clear` — it reuses `PATCH /api/sessions/{id}` (the
existing workspace-patching endpoint generalises to accept summary fields).

## Files Changed

| File | Change |
|------|--------|
| `cmd/spore/tui.go` | Add command dispatch in handleKey |
| `cmd/spore/tui_test.go` | Harness tests for each command |
| `internal/daemon/server.go` | Add POST /api/sessions/{id}/compact route |
| `internal/daemon/sessions.go` | Implement compact handler |
| `internal/daemon/api_test.go` | Tests for the new endpoint |
| `internal/store/store.go` | Add SetSummaryThrough (summary boundary update) |
| `internal/store/store_test.go` | Test for summary boundary move |
| `cmd/spore/chat.go` | No change — all dispatch is in the TUI |

No new subsystems, no spec changes, no new dependencies.
