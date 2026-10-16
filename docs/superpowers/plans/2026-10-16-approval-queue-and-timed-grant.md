# Approval Queue Fixes and Timed Grants Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expired approvals leave every queue quietly, TUI approval keys act on the first press, and a timed grant lets a session tree run every ask-rule call without prompting while deny rules still block (and say so).

**Architecture:** The broker publishes `resolved/expired` when it stops waiting. The guard holds in-memory grants keyed by root session and checks them at the top of its ask branch; it reports deny-rule refusals through an optional interface the broker implements. Two new /api routes start and end a grant; the TUI drives them from the approval card (`a`), `:grant`, and shows a header countdown.

**Tech Stack:** Go, bubbletea/lipgloss TUI, net/http daemon, SQLite store, vanilla JS web UI.

**Spec:** `docs/superpowers/specs/2026-10-16-approval-queue-and-timed-grant-design.md` — read it before each task.

## Global Constraints

- Config key: `policy.grant_durations`, TOML `grant_durations`, default `["5m", "15m", "30m", "1h"]`. Empty list = feature off.
- Wire event types (append-only strings in `internal/daemon/event.go`): `WireGrant = "grant"`, `WireBlocked = "blocked"`. Resolved decision for a dropped ask: `"expired"`.
- Audit reason for a call a grant allowed without asking: `"grant"`; trace rule `"timed grant"`.
- Deny is never overridden by a grant. Tests that assert deny behaviour build policy through `config.Load` (or append `config.BaselineDeny()`), never bare `config.Default()` — it lacks the baseline deny.
- The confirm modal stays for everything except approval answers.
- CI gate before every commit that ends a task: `go vet ./... && make fmtcheck && make lint && go test ./...`; before the PR also `make vulncheck` and `go mod tidy && git diff --exit-code go.mod go.sum`. Check the Makefile for the exact target names.
- Stage files by name; never `git add -A`.
- Comments and messages follow the repo's existing style (plain, explain why).

## Review Focus

1. A grant started while a sub-agent's session is selected must land on its root, never on the child — the API refuses a child id (Task 5, `TestGrantRejectsChild`) and the TUI sends the root (Task 6, `TestGrantCommand` with a child selected).
2. A grant that expires mid-turn: the next ask-rule call must prompt again, not run — test in Task 4 (`TestGrantExpiredAsksAgain`).
3. A config reload (`SetEngine`) that removes a duration must not end a running grant, but new grants must use the new list — test in Task 5 (`TestGrantDurationFromCurrentEngine`).
4. Answering a card whose answer fails with a network error must keep it answerable, but behind the live ones — test in Task 2 (`TestResolveFailureRestoresAtBack`).
5. `:grant` typed with a duration not in the list must not call the daemon — test in Task 6 (`TestGrantCommandRejectsUnknownDuration`).

---

### Task 1: Expired asks publish `resolved/expired`; dead answers are distinguishable

**Files:**
- Modify: `internal/daemon/approver.go` (`Broker.Ask` ctx.Done branch; `handleResolveApproval`)
- Modify: `internal/policy/guard.go` (`Guard.Resolve`, new sentinel)
- Test: `internal/daemon/approver_test.go` (or the existing broker test file — grep for `NewBrokerWithTTL` in tests), `internal/policy/guard_test.go`

**Interfaces:**
- Produces: `var ErrNotPending = errors.New(...)` in package policy; `Guard.Resolve` returns an error wrapping it (`fmt.Errorf("...: %w", ErrNotPending)`) when the claim is not won. `const DecisionExpired = "expired"` in package daemon (approver.go), used as `WireEvent.Decision`.

- [ ] **Step 1: Write failing tests**

  - `TestAskTimeoutPublishesExpired`: subscribe to a hub topic, call `Broker.Ask` with a ctx that times out (e.g. 20ms); assert the topic receives `WireEvent{Type: WireResolved, PendingID: <id>, Tool: <tool>, Decision: "expired"}` and Ask returns `context.DeadlineExceeded`.
  - `TestAskCancelPublishesExpired`: same with `cancel()`; Ask returns `context.Canceled`, event Decision `"expired"`.
  - `TestAnsweredAskPublishesOnce`: answer via `Broker.Answer`; assert exactly one resolved event, Decision `"allow"`, none `"expired"`.
  - `TestResolveNotPendingSentinel` (policy): resolve a pending call twice; second error satisfies `errors.Is(err, policy.ErrNotPending)`.
  - `TestResolveApprovalStatus` (daemon, httptest): already-resolved pending id → 409; a store failure (close the store before the call, or an injected failing guard if the tests already have one) → 500.

- [ ] **Step 2: Run them, expect FAIL**

  Run: `go test ./internal/daemon/ ./internal/policy/ -run 'Expired|PublishesOnce|NotPending|ResolveApprovalStatus' -v`

- [ ] **Step 3: Implement**

  In `Broker.Ask`, in the `stillWaiting` branch, publish `WireEvent{Type: WireResolved, PendingID: a.PendingID, Tool: a.Tool, Decision: DecisionExpired}` to `rootID` before returning `ctx.Err()`. In `Guard.Resolve`, wrap `ErrNotPending` in the `!won` error. In `handleResolveApproval`, `errors.Is(err, policy.ErrNotPending)` → 409, else 500.

- [ ] **Step 4: Run the tests and the package suites, expect PASS**

  Run: `go test ./internal/daemon/ ./internal/policy/`

- [ ] **Step 5: Commit** — `daemon: an ask that stops waiting tells clients it expired`

---

### Task 2: Clients drop expired cards quietly; TUI answers without a modal

**Files:**
- Modify: `internal/tui/cache.go` (`WireResolved` case, `restoreApproval`)
- Modify: `internal/tui/app.go` (`answer`, insert-mode alt branch ~line 553-571, `keyNormal` ~line 611, `approvalFailedMsg` handler ~line 459)
- Modify: `internal/tui/backend.go` (sentinel)
- Modify: `cmd/spore/tui_backend.go` (`Resolve` maps errors)
- Modify: `internal/tui/approval.go` (`draftLine` text: `answer with alt+y/n/s/p, or ctrl+o then y/n/s/p` stays valid; leave unless a key is added in Task 6)
- Modify: `web/app.js` (`case "resolved"`)
- Test: `internal/tui/approval_test.go`, `internal/tui/app_test.go`, `cmd/spore/tui_backend_test.go` (create if absent)

**Interfaces:**
- Consumes: `daemon.DecisionExpired` (Task 1).
- Produces: `var ErrApprovalGone = errors.New("that approval is no longer waiting")` in package tui. `tuiBackend.Resolve` returns it (wrapped is fine) when the client's `*httpError` has Status 404 or 409.

- [ ] **Step 1: Write failing tests**

  - `TestApprovalKeysActAtOnce`: for each of `y`, `n`, `s`, `p` (pattern set) in NORMAL, and `alt+` each in INSERT: one key press → `fakeBackend.resolved` has one entry with the right scope, `m.confirm == nil`, mode unchanged (INSERT stays INSERT).
  - `TestExpiredResolvedIsQuiet`: apply an approval then `WireResolved{Decision: "expired"}`; the card is gone and the session has no new notice block. A `Decision: "allow"` not answered locally still adds the "answered elsewhere" notice.
  - `TestResolveGoneDropsCard`: `fakeBackend.resolveErr = fmt.Errorf("x: %w", tui.ErrApprovalGone)`; press y; after the cmd's msg is processed the card is gone and no flash says "could not answer".
  - `TestResolveFailureRestoresAtBack`: two approvals A, B; `resolveErr = errors.New("connection refused")`; answer A; after the msg, approvals order is `[B, A]` and the flash names the error.
  - `TestTUIBackendResolveGone` (cmd/spore): httptest server returning 409 then 404 then 500; `errors.Is(err, tui.ErrApprovalGone)` true, true, false.
  - Update existing tests that walk through the approval confirm modal (grep `modeConfirm` / `confirm` in `internal/tui/*_test.go` and approval-related e2e tests) to the one-press behaviour.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/tui/ ./cmd/spore/ -run 'ActAtOnce|ExpiredResolved|ResolveGone|RestoresAtBack|TUIBackendResolveGone' -v`

- [ ] **Step 3: Implement**

  `Model.answer` calls `m.resolve(root, ev, ans)` and returns its cmd (change its signature to `answer(root string, ev daemon.WireEvent, k string) (tea.Cmd, bool)`; update both callers). Remove the INSERT branch's special case for ScopeOnce and the `confirm.back` patch, which no longer apply. Delete `answerQuestion` if it becomes unused (lint catches it). The cache's `WireResolved` case skips the notice when `ev.Decision == daemon.DecisionExpired`. The `approvalFailedMsg` handler: `errors.Is(msg.err, ErrApprovalGone)` → `delete(c.answered, id)` and drop silently; otherwise `restoreApproval`, which now APPENDS. In `web/app.js` `case "resolved"`, skip `addNote` when `ev.decision === "expired"`.

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/tui/ ./cmd/spore/`

- [ ] **Step 5: Commit** — `tui: approval keys answer at once; dead cards leave quietly`

---

### Task 3: `policy.grant_durations` config

**Files:**
- Modify: `internal/config/config.go` (`PolicyConfig`, defaults ~line 752, merge ~line 973, validate ~line 1142)
- Modify: `internal/policy/engine.go` (`Engine` field, `NewEngine`, accessor)
- Modify: the example/shipped config that documents `approval_timeout` (grep `approval_timeout` outside Go: README, `assets/`, docs) — add `grant_durations` beside it with a one-line comment.
- Test: `internal/config/config_test.go`, `internal/policy/engine_test.go`

**Interfaces:**
- Produces: `PolicyConfig.GrantDurations []string` (`toml:"grant_durations"`); `func (e *Engine) GrantDurations() []time.Duration` (in config order); `func (e *Engine) GrantDuration(s string) (time.Duration, bool)` — true only when `s` is exactly one of the configured strings.

- [ ] **Step 1: Write failing tests**

  - `TestDefaultGrantDurations`: `config.Default().Policy.GrantDurations` equals `[]string{"5m","15m","30m","1h"}`; a loaded file that omits the key gets the same.
  - `TestGrantDurationsExplicitEmpty`: a file with `grant_durations = []` loads with an empty list (not the default). If the merge code cannot tell absent from empty, use a `*[]string` or the toml metadata `IsDefined` the loader already uses — check how other list keys are merged and follow that.
  - `TestGrantDurationsInvalid`: `["5m","soon"]` → Load error mentioning `policy.grant_durations`; `["0s"]` and `["-5m"]` → error.
  - `TestEngineGrantDuration`: engine from `["5m","1h"]`: `GrantDuration("1h")` → `time.Hour, true`; `GrantDuration("60m")` → false; `GrantDuration("2h")` → false.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/config/ ./internal/policy/ -run 'GrantDuration' -v`
- [ ] **Step 3: Implement** the field, default, merge, validation (each entry parses and is > 0) and the engine accessors.
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/config/ ./internal/policy/`
- [ ] **Step 5: Commit** — `config: policy.grant_durations lists the timed-grant choices`

---

### Task 4: Guard grants and blocked reports

**Files:**
- Create: `internal/policy/grant.go` (grant state and methods)
- Modify: `internal/policy/guard.go` (`Guard` struct, `NewGuard`, `Run`)
- Modify: `internal/daemon/approver.go` (`Broker.Blocked`)
- Test: `internal/policy/grant_test.go`, `internal/daemon/approver_test.go`

**Interfaces:**
- Consumes: `Engine` from Task 3 (not needed for evaluation itself).
- Produces, package policy:
  - `func (g *Guard) StartGrant(root string, until time.Time)` — starts or replaces.
  - `func (g *Guard) EndGrant(root string)`
  - `func (g *Guard) GrantUntil(root string) (time.Time, bool)` — false when absent or expired (prunes it).
  - `type BlockReporter interface { Blocked(rootID, sessionID, tool, rule, detail string) }` — the guard type-asserts its approver to this; no constructor change.
  - Guard gains `now func() time.Time` (set to `time.Now` in `NewGuard`; tests in package policy assign it).
- Produces, package daemon: `func (b *Broker) Blocked(rootID, sessionID, tool, rule, detail string)` publishing `WireEvent{Type: WireBlocked, Tool: tool, Rule: rule, Content: detail}` to `rootID`, with `Origin: sessionID` when it differs from rootID. Add `WireBlocked = "blocked"` and `WireGrant = "grant"` to event.go now.

- [ ] **Step 1: Write failing tests** (policy tests use a config.Load-built engine with an ask rule for `shell_exec` and a deny rule, plus the baseline deny; a recording approver that fails the test if Ask is called)

  - `TestGrantRunsAskCall`: `StartGrant(root, now+5m)`; Run an ask-rule call → inner runner called, approver not asked, no pending row, an approval audit row with decision `"allow"` and reason/scope `"grant"`.
  - `TestGrantKeepsDeny`: same grant; a config-deny call and a baseline-deny call are refused with the existing "denied by policy rule" text; inner not called.
  - `TestGrantCoversChild`: child session whose ancestor is root; grant on root; child's ask call runs.
  - `TestGrantOverridesSessionDecline`: record a session-scope deny for the tool on root; with a grant the call runs.
  - `TestGrantExpiredAsksAgain`: grant until now+1m; advance `g.now` past it; the call reaches the approver.
  - `TestEndGrant`: `EndGrant(root)` → next call asks.
  - `TestBlockedReported`: approver also implements BlockReporter; a deny-rule call from a child reports `(root, child, tool, rule, "")`; a path-containment deny reports a non-empty detail; a no-workspace refusal reports nothing.
  - `TestBrokerBlockedPublishes` (daemon): event shape as in Interfaces.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/policy/ ./internal/daemon/ -run 'Grant|Blocked' -v`

- [ ] **Step 3: Implement**

  `grant.go`: a mutex-guarded `map[string]time.Time` on the Guard (embed a small `grants` struct in Guard). In `Run`: on `DecisionDeny` (after `Evaluate`, before returning), resolve root with `rootOf` (fall back to `sess.ID`) and call `Blocked` if the approver implements `BlockReporter`. At the top of the ask branch, before `rootScopedDecision`: resolve root; if `GrantUntil(root)` is live, record trace `"allow"`/`"timed grant"`, write `store.RecordApproval(ctx, sess.ID, call.Name, call.Input, "allow", "grant")`, run inner. Reuse the resolved root later in the function instead of calling `rootOf` twice. Immediately before `g.approver.Ask`, check `GrantUntil(rootID)` again; if live, resolve the just-written pending row as `"allow"` via `ResolvePendingCall`, audit `"grant"`, and run inner (spec, settled item 3).

- [ ] **Step 4: Run, expect PASS** — `go test ./internal/policy/ ./internal/daemon/`
- [ ] **Step 5: Commit** — `policy: timed grants allow ask calls; deny refusals are reported`

---

### Task 5: Grant API, grant event, replay

**Files:**
- Modify: `internal/daemon/server.go` (routes)
- Create: `internal/daemon/grant.go` (handlers, sweep, event helper)
- Modify: `internal/daemon/stream.go` (replay current grant after pending approvals, both per-session ~line 37 and global ~line 89)
- Test: `internal/daemon/grant_test.go`

**Interfaces:**
- Consumes: `Guard.StartGrant/EndGrant/GrantUntil` (Task 4), `Engine.GrantDuration` (Task 3), `ErrNotPending`, `Broker.Answer`, `Guard.Resolve`, `Guard.PendingTree`, `store.SessionAncestors`.
- Produces:
  - `POST /api/sessions/{id}/grant` body `{"duration":"15m"}` → 200 `{"expires_at":"<RFC3339 UTC>"}`.
  - `DELETE /api/sessions/{id}/grant` → 200 `{"status":"ended"}`.
  - Wire event `WireEvent{Type: WireGrant, ExpiresAt: <RFC3339 or "">}` published to the root; on the global feed it carries `Session: root` like other replayed events.
  - `{id}` that is a sub-agent (has ancestors): 400. Clients send the root; the TUI resolves it (Task 6).

- [ ] **Step 1: Write failing tests** (httptest against `buildMux` with a real guard as other daemon tests do)

  - `TestGrantStart`: POST `15m` → 200, `expires_at` ≈ now+15m; `GrantUntil(root)` live; a `grant` event published to root.
  - `TestGrantRejectsDuration`: `"2h"` and `"60m"` with default config → 400 naming the allowed list; no grant.
  - `TestGrantDisabled`: engine with empty list → 400.
  - `TestGrantSweepsWaiting`: two asks waiting (one root, one child) with live waiters → after POST both return allow to their callers; a suspension with no live waiter (insert a pending row directly) is resolved allow through `Guard.Resolve`.
  - `TestGrantRejectsChild`: POST to a child's id → 400; no grant on child or root.
  - `TestGrantEnd`: DELETE → `GrantUntil` false; event with empty `ExpiresAt`.
  - `TestGrantReplayedOnAttach`: start a grant, open the session event stream → a `grant` event with the expiry arrives; same on `/api/events` with `Session` set. No grant → no grant event.
  - `TestGrantDurationFromCurrentEngine`: start a 1h grant, `SetEngine` with a list lacking `1h` → grant still live; POST `1h` now → 400.
  - Unknown session id → 404 (use `findSession`).

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/daemon/ -run Grant -v`
- [ ] **Step 3: Implement** handlers in `grant.go`; register both routes with `api(...)`. Sweep: for each `PendingTree(root)` entry, `Broker.Answer(root, id, Answer{Allow: true, Scope: ScopeOnce})`, else `Guard.Resolve(ctx, root, id, ...)` ignoring `ErrNotPending`, then publish `WireResolved` with Decision `"allow"` for the Resolve path (as `handleResolveApproval` does). Set the grant before sweeping.
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/daemon/`
- [ ] **Step 5: Commit** — `daemon: start and end timed grants over the API`

---

### Task 6: TUI grant controls, countdown and blocked notice

**Files:**
- Modify: `internal/tui/backend.go` (two methods), `cmd/spore/tui_backend.go`, `cmd/spore/client.go` (two client calls)
- Modify: `internal/tui/cache.go` (`WireGrant`, `WireBlocked` cases; per-session `grantUntil time.Time`)
- Modify: `internal/tui/approval.go` (card row, `draftLine`)
- Create: `internal/tui/grant.go` (picker state, rendering, keys, `:grant`)
- Modify: `internal/tui/app.go` (key routing for `a`/`alt+a`, picker mode, `:grant` in the command switch ~line 931)
- Modify: `internal/tui/header.go` (`sessionFacts`)
- Modify: the TUI's knowledge of `grant_durations`: the TUI needs the list. Add it to an existing daemon response the TUI already fetches at start (e.g. `GET /api/policy` via `handlePolicy` — check what it returns) as `grant_durations []string`, and fetch it on connect; refetch on reconnect.
- Test: `internal/tui/grant_test.go`, `cmd/spore/tui_backend_test.go`

**Interfaces:**
- Consumes: Task 5 routes and event; `daemon.WireBlocked` (Task 4).
- Produces: `Backend.StartGrant(ctx context.Context, id, duration string) (time.Time, error)`, `Backend.EndGrant(ctx context.Context, id string) error`, `Backend.GrantDurations(ctx context.Context) ([]string, error)`; fakeBackend implements all three and records calls.

- [ ] **Step 1: Write failing tests**

  - `TestCardShowsGrantRow`: with durations, the card contains `a` and `allow all for…`; with an empty list it does not, and `a` does nothing.
  - `TestGrantPickerByNumber`: approval on screen, press `a` → picker lists `5m 15m 30m 1h`; press `2` → `StartGrant(root, "15m")` called once; no confirm modal opened; picker closed.
  - `TestGrantPickerJKEnter`: `a`, `j`, `j`, `enter` → `"30m"`; `esc` instead → no call, card still shown.
  - `TestGrantAltAInInsert`: INSERT with a draft, `alt+a` opens the picker; after picking, mode is INSERT and the draft is intact.
  - `TestGrantCommand`: `:grant 1h` → `StartGrant(root, "1h")`, and with a sub-agent selected the id passed is its root (walk `info.ParentID`); `:grant off` → `EndGrant(root)`; `:grant` alone opens the picker.
  - `TestGrantCommandRejectsUnknownDuration`: `:grant 2h` → no backend call, flash lists `5m, 15m, 30m, 1h`.
  - `TestGrantCountdown`: apply `WireEvent{Type: WireGrant, ExpiresAt: now+14m32s}` to root; selected child or root → `sessionFacts` contains `grant 14:32`; at `opts.Now` past expiry it is gone; event with empty ExpiresAt clears it.
  - `TestBlockedNotice`: apply `WireBlocked{Tool:"shell_exec", Rule:"shell_exec(rm *)"}` → notice `blocked by config: shell_exec (rule shell_exec(rm *))`; with empty Rule → `blocked by config: shell_exec`; with Content set, the detail follows after ` · `.
  - `TestStartGrantFailureFlashes`: `StartGrant` returns an error → flash shows it, card stays.

- [ ] **Step 2: Run, expect FAIL** — `go test ./internal/tui/ ./cmd/spore/ -run 'Grant|Blocked' -v`
- [ ] **Step 3: Implement.** The picker is a small state on Model (`grantPick *grantPicker` with the root, the list and a cursor) drawn with the existing modal style; it is not a `confirmState`. `draftLine` becomes `answer with alt+y/n/s/p/a, or ctrl+o then y/n/s/p/a` when durations exist. Countdown format matches `deadline` (`%d:%02d`), with hours as `h:mm:ss` when ≥ 1h. Grant state is stored on the ROOT's session view; `sessionFacts` looks up the selected session's root through `info.ParentID`.
- [ ] **Step 4: Run, expect PASS** — `go test ./internal/tui/ ./cmd/spore/`
- [ ] **Step 5: Full CI gate** (Global Constraints), then commit — `tui: timed grants from the approval card, :grant and a header countdown`

---

### Task 7: Manual check

- [ ] Build a branch binary and run it isolated (HOME in the scratchpad, port 7788, own `policy.workspace`, no discord — see the `spore once` memory). Use `policy.approval_timeout = "20s"`.
- [ ] Make the model ask twice, wait 25s: both cards vanish without notices.
- [ ] Ask again, press `y`: no modal, call runs.
- [ ] Press `a`, `1` (5m): card clears, header shows `grant 4:5x`; ask for a shell command → runs without a card; ask for something a deny rule matches → transcript shows `blocked by config: …`.
- [ ] `:grant off` → next call shows a card.
- [ ] Record what was seen in the PR description.
