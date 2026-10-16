# Approval queue fixes and timed grants

Date: 2026-10-16

## Why

The user stepped away from the TUI with approvals open. Each ask timed out
(policy.approval_timeout, default 5m) and the guard denied it, but the TUI
kept every card. On return they had to work through one confirm modal per
card, and the stale ones kept coming back.

Root cause, found in recon:

- `Broker.Ask` (internal/daemon/approver.go) publishes `WireResolved` only
  when an answer arrives. On `ctx.Done()` with the waiter still present —
  a timeout, or the turn stopped — it returns `ctx.Err()` and publishes
  nothing. Clients keep the card.
- Answering such a card fails (`Guard.Resolve` finds the row already
  resolved as "timeout"), and the TUI's `approvalFailedMsg` handler calls
  `cache.restoreApproval`, which puts the card back at the FRONT of the
  queue. The dead card blocks the live ones behind it.
- Every approval key opens a confirm modal (`Model.answer` in
  internal/tui/app.go), so each card costs two keystrokes and a read.

The user also wants to be able to walk away on purpose: a timed grant that
lets the model act without prompting for a while.

## Goals

1. Approvals that can no longer be answered leave every client's queue
   quietly — no notice, no modal.
2. Approval keys in the TUI (y/n/s/p) act on the first press, with no
   confirm modal.
3. A timed grant: for a chosen duration, every call in a session (and its
   sub-agents) that policy would ASK about runs without asking. Durations
   are configurable.
4. While a grant is active, a call a DENY rule matches is still refused,
   and the TUI shows a line saying it was blocked by the config.

## Non-goals

- Starting a grant from the web UI or Discord (they only receive the new
  events this round).
- Persisting grants across a daemon restart.
- Restricting shell commands to the workspace during a grant. The user
  decided shell runs too; only deny rules stop it.
- Changing the confirm modal for anything other than approval answers
  (session delete, sub-agent cancel and actions keep theirs).

## Anti-goals

- A grant must never override a deny rule: baseline, config or learned.
  Deny is evaluated before ask today (`Guard.Run`, `case DecisionDeny`);
  the grant lives in the ask branch only.
- A grant must never turn a policy `allow` into anything else, or change
  any rule on disk. It writes no learned rule.
- A sub-agent must not be able to start a grant for itself. Grants are
  started only through the human-facing API by a root session's client.
- A grant must not outlive its deadline by more than the time it takes one
  call to be evaluated.

## Design

### 1. Expired approvals leave the queue

Daemon (`Broker.Ask`): when it stops waiting with the waiter still present,
publish `WireResolved` to the root with `Decision: "expired"` before
returning `ctx.Err()`. "expired" covers both deadline and cancellation; the
guard still writes its own "timeout"/"error" resolution and audit row as
today.

TUI cache (`WireResolved` handler in internal/tui/cache.go): drop the card
as today, but add no "answered elsewhere" notice when the decision is
"expired".

TUI backstop: when a resolve fails, decide by the HTTP status. A 409
(already answered / already resolved) or 404 means the card is dead: drop
it silently, do not restore it. Any other failure (network, 5xx) keeps
today's behaviour: restore the card and flash the error. Restoring puts
the card at the BACK of the session's queue, not the front, so a card that
fails for another reason does not block live ones. The backend client
must surface the status for this; the builder chooses how (a typed error
is fine).

Web UI and Discord: they already drop/print on `WireResolved`. Discord
prints "→ expired"; that is acceptable. The web UI must not show the
"expired" event as a decision the user made; the builder checks
web/app.js and makes the smallest change that drops the card silently.

### 2. No confirm modal on approval keys

`Model.answer` calls `resolve` directly instead of opening a
`confirmState`. The flash line after answering (`answerText`) stays, so
the user still sees what was sent. The `p` key on an ask with no pattern
stays swallowed. Tests that walk through the modal are updated; the
memory note that alt+s/p confirm is superseded.

### 3. Timed grants

**Config.** `policy.grant_durations`, a list of Go duration strings,
default `["5m", "15m", "30m", "1h"]`. Validated in config.Load like
`approval_timeout`: every entry parses, is > 0, and the list may be empty
(empty means the feature is off: no `a` key, `:grant` refuses). The
example config shipped with spore documents it next to approval_timeout.

**Daemon state.** A grant is `{root session id, expires at}`, held in
memory by the guard (or a small type the guard owns), keyed by root
session id. Expired grants are treated as absent and pruned lazily.
Restarting the daemon drops every grant.

**Evaluation.** In `Guard.Run`, in the ask branch, BEFORE
`rootScopedDecision`: resolve the call's root (`rootOf`, already used
below), and if that root has an unexpired grant, run the call. Record it:
`sporetrace.RecordPolicy(ctx, "allow", "timed grant")` and an audit row
via `store.RecordApproval(..., "allow", "grant")`. No pending row is
written and no approver is asked.

A grant must also apply to asks that are already waiting when it starts
(see API below), so the user's other cards clear at once.

**API.** Two endpoints on the existing authenticated /api, on the root
session:

- `POST /api/sessions/{id}/grant` with `{"duration": "15m"}`: the duration
  must be one of `policy.grant_durations` exactly (refuse others with
  400, so a client cannot ask for 24h). Refuse with 400 when {id} is a
  sub-agent (has ancestors). Starts or replaces the grant, then answers
  every waiting ask in that root's tree with allow/once through the
  normal `Broker.Answer` path (falling back to `Guard.Resolve` for
  suspensions with no live waiter, as `handleResolveApproval` does).
  Publishes a `grant` wire event to the root with the expiry.
- `DELETE /api/sessions/{id}/grant`: ends it; publishes a `grant` event
  with an empty expiry.

The wire event carries `ExpiresAt` (RFC3339, empty = no grant). Clients
attaching later need the current grant: include it in whatever the
session/attach replay already sends (the builder picks the spot, e.g.
alongside `pendingApprovalEvents`).

**TUI.**
- The approval card gets one more row: `a  allow all for…`. Pressing `a`
  opens a small picker listing the configured durations (picking IS the
  action; there is no second confirmation). Picking calls the grant
  endpoint; that both starts the grant and allows the card on screen
  (it is one of the waiting asks). Esc closes the picker without acting.
  In INSERT mode the key is `alt+a`, matching the other answer keys, and
  `draftLine` mentions it.
- `:grant <duration>` starts a grant on the selected session's root
  without a card; `:grant off` ends it. A duration not in the config is
  refused with a flash listing the allowed ones.
- The header shows the remaining time for the selected session's root
  grant (e.g. `grant 14:32`) and drops it at zero, driven by the existing
  tick.
- The TUI's backend interface gains the two calls; the fake backend used
  in TUI tests implements them.

### 4. Blocked calls are shown

When `Guard.Run` returns a deny from a DENY rule, the daemon publishes a
wire event to the root (new type, e.g. `blocked`) carrying the tool and
the rule. This happens for every deny-rule refusal, grant or not (the
assistant's call; the user asked for it during grants). The TUI adds a
transcript notice: `blocked by config: <tool> (rule <rule>)`. The tool
result the model sees is unchanged.

The guard has no hub; the builder chooses how the guard reports a block
(a small callback/interface set at wiring time, mirroring how the
approver is injected). Deny decisions for "no session"/"no workspace" are
not deny rules and publish nothing.

## Decided by the user

- Fix the pile-up; stale approvals leave the queue quietly.
- No confirm modal on approving actions.
- Timed grant with configurable durations (5m, 15m, 30m, 1h…).
- During a grant shell commands run without asking.
- Deny rules still block during a grant, and the TUI says it was blocked
  by the configuration instead of showing an approval.
- Started from the approval card with `a` + a duration; covers the session
  and its sub-agents; header countdown; can be ended early.

## Assistant's calls (user approved the playback)

- "expired" covers timeout and stopped turns; Discord prints it as is.
- Starting a grant also allows the other cards already waiting in that
  session tree.
- Blocked notice shows for every deny-rule refusal, not only during
  grants.
- `:grant <d>` / `:grant off` as the way to start without a card and to end
  early.
- Grant-allowed calls are audited with reason "grant".
- Grants are in memory only; a restart ends them.
- TUI only for starting grants this round.
- Durations must match the config list exactly; an empty list disables
  the feature.

## Settled after the builder check (assistant's calls)

1. **Telling a dead card from a failed send.** Today every
   `Guard.Resolve` error becomes 409, store failures included. Add a
   sentinel `policy.ErrNotPending` (returned when the claim is not won)
   and make `handleResolveApproval` return 409 only for that and for
   `AlreadyAnswered`; any other Resolve error becomes 500. The TUI drops
   the card silently on 409 or 404 and restores it (at the back) on
   anything else.
2. **Sweeping waiting asks.** `Broker.Answer` checks the waiter's
   `rootID`, which is the root for a child's ask, so the sweep answers
   every ask with the ROOT id. Enumerate with `Guard.PendingTree(root)`
   (exists: `store.PendingCallsTree`); for each, try `Broker.Answer`,
   else `Guard.Resolve`, ignoring `ErrNotPending`. Swept asks are
   answered allow/once and audited by the normal path (reason "once"),
   not "grant"; only calls that never asked are audited as "grant".
3. **Race with an ask that is just arriving.** Set the grant first, then
   sweep. The guard checks the grant once more immediately before calling
   the approver. A call that slips between those still shows a card the
   user can answer; this window is accepted.
4. **Grant vs. an earlier "declined this session".** The grant is checked
   before `rootScopedDecision`, so it overrides a remembered session
   decline. "Allow all" is the newer, broader instruction. A
   grant-allowed call skips `PatternFor` and proposals entirely.
5. **Lifetime.** Only the deadline, `DELETE`, or a daemon restart end a
   grant. Stopping a turn (esc) does not. New turns in the same root are
   covered.
6. **Blocked event.** Published for any `DecisionDeny` from
   `Engine.Evaluate`, path-containment denies included, to the root found
   by `rootOf` (one store read per deny is acceptable). Live only: not
   stored, not replayed. When `res.Rule` is empty the notice omits the
   rule part. When `res.Detail` is set, the notice may append it.
7. **Other clients.** The builder checks web/app.js and the Discord
   renderer and makes both ignore the `grant` and `blocked` event types
   without error. Discord already prints `→ <decision>` for resolved
   events (render.go), so it shows `→ expired`; that is accepted.
8. **Picker.** The `a` row and `alt+a` only exist when
   `grant_durations` is non-empty. The picker takes number keys 1-9 and
   j/k+enter; esc cancels. Cards stay on screen until the daemon's
   resolved events remove them; a failed grant call flashes the error.
   `:grant` with no argument opens the same picker. `:grant` on a
   sub-agent's session acts on its root.

## Left to the builder

- Names of the new wire event types and the backend error type.
- Where the grant state lives (inside Guard, or a type Guard holds).
- Picker rendering, as long as it fits the existing modal styling.
- Where the current grant is replayed to a newly attached client.

## Testing expectations

- Daemon: an ask that times out publishes `resolved/expired`; one stopped
  by cancellation does too; an answered ask still publishes allow/deny
  exactly once.
- Guard: with a grant, an ask-rule call runs and is audited as "grant";
  a deny-rule call is still refused (test with config.Load-built policy so
  the baseline deny is present — config.Default() lacks it); an expired
  grant asks again; a sub-agent's call is covered by its root's grant.
- API: duration outside the list → 400; sub-agent id → 400; grant answers
  waiting asks; DELETE ends it.
- TUI: y/n/s/p send on first press; an "expired" resolved drops the card
  without a notice; a 409 resolve drops the card; a network failure
  restores it at the back; `a` → picker → grant call; `:grant off`;
  header countdown.
- Config: invalid duration rejected; default list present.
- Full CI gate: vet, fmtcheck, lint, vulncheck, tidy, test.
