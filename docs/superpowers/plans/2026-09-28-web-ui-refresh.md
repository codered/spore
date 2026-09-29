# Web UI Refresh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bring `web/` to the TUI's visual language and layout, add the 2a-1 views plus refinements as tables, and add single-key shortcuts with a per-browser off switch.

**Architecture:** One additive daemon change (two optional fields on the `approval` wire event). Everything else is `web/index.html`, `web/style.css` and `web/app.js`, rewritten in place: still no framework, no build step, embedded by `web/embed.go`. The page drives all live state from the one global feed `GET /api/events`.

**Tech Stack:** Go (net/http ServeMux patterns), vanilla ES2020, CSS custom properties.

**Spec:** `docs/superpowers/specs/2026-09-28-web-ui-refresh-design.md` (read §2, §4.2 and §5 before starting any task).

## Global Constraints

- No external resource in any served file: no `http://`, `https://`, CDN or webfont (`TestUIReferencesNoExternalResources`).
- Every API call in `app.js` is written `api("METHOD", "/api/...{param}...", params, body)` with a **literal** path template; ids are substituted by `api` with `encodeURIComponent`. `EventSource` URLs are literals. This is what lets the route-coverage test read the JS.
- No route to a 2a-2 endpoint (`/api/policy`, `/api/mcp`, `/api/memory`).
- Approval keys (`y n s p`) never act while `input`, `textarea`, `select` or `[contenteditable]` has focus. Not configurable.
- `localStorage` access only through `loadKeys`/`saveKeys`, each in try/catch.
- `WireEvent` stays comparable: new fields are strings.
- Gates: `make vet`, `make fmtcheck`, `make lint`, `make test`.
- Stage files by name. Never `git add -A`.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp
  ```

## Review Focus

1. A `y` typed into the composer while an approval card is visible → a `y` in the composer, no POST to `/approvals/` (manual gate 1; handler step 3 in `onKey`).
2. A sub-agent asks → the **root** row shows `◐`, the child row does not (the feed's `session` is already the root; spec §4.2).
3. A daemon restart with an approval outstanding → card shows `waiting (no timeout)`, never a countdown (Task 1 `TestReplayWithoutWaiterHasNoExpiry`).
4. `app.js` gains a call to a route that does not exist → `TestAppJSRoutesAreRegistered` fails.

---

### Task 1: Approval event carries profile and deadline

**Files:**
- Modify: `internal/policy/guard.go` (`Ask` struct; `Run` fills `Profile`, `Deadline`)
- Modify: `internal/daemon/event.go` (`Profile`, `ExpiresAt` fields)
- Modify: `internal/daemon/approver.go` (waiter keeps deadline; `approvalEvent` and both replay paths set the fields; `Broker.deadline(id)`)
- Test: `internal/daemon/approver_test.go`

- [ ] **Step 1: Failing tests.** `TestApprovalEventCarriesProfileAndExpiry`: `Broker.Ask` with `Profile: "remote"`, `Deadline: now+5m` publishes an event with `Profile == "remote"` and `ExpiresAt` parsing to the deadline (RFC 3339). `TestReplayWithoutWaiterHasNoExpiry`: `approvalEvent`-style replay for a pending id the broker has no waiter for leaves `ExpiresAt` empty.
- [ ] **Step 2: Run** `go test -tags sqlite_fts5 ./internal/daemon/ -run 'Approval|Replay'` → FAIL (fields do not exist).
- [ ] **Step 3: Implement.** `policy.Ask{Profile string; Deadline time.Time}`; in `Guard.Run` set `Profile: string(sess.Profile)` and `Deadline` from `askCtx.Deadline()`. `WireEvent{Profile string "profile,omitempty"; ExpiresAt string "expires_at,omitempty"}`. `waiter.deadline time.Time`; `func (b *Broker) deadline(id int64) (time.Time, bool)`. Replay sets `Profile: p.Profile` and `ExpiresAt` from `s.broker.deadline(p.ID)`.
- [ ] **Step 4: Run** the daemon and policy packages → PASS.
- [ ] **Step 5: Commit** `approval events carry profile and auto-deny deadline`.

### Task 2: Route-coverage test for app.js

**Files:**
- Test: `internal/daemon/web_test.go`

- [ ] **Step 1: Write `TestAppJSRoutesAreRegistered`.** Read `web.FS` `app.js`. Regex `api\("([A-Z]+)",\s*"(/api/[^"]*)"` gives method + template; a second regex `"(/api/[^"]*)"` gives every literal. For each: drop `?query`, replace `{name}` with `1`, build a request, call `mux.Handler(req)` on `(&Server{...}).Handler().(*http.ServeMux)`. Pass when the returned pattern is non-empty and not `GET /`. Literals seen only outside `api(...)` (EventSource) must match with `GET`. Also assert at least 10 routes were found, so a regex that silently matches nothing cannot pass.
- [ ] **Step 2: Run** → FAIL against the current `app.js` (`"/api/sessions/"` concatenations resolve to the `GET /` catch-all). That red is the proof the test reads the file.
- [ ] **Step 3:** Update `TestIndexRenders`/`TestStaticAssetsAreEmbedded` needles to the new landmarks (`id="sidebar"`, `id="settings"`, `spore.keys`, `--selected`). Stays red until Tasks 3–4.
- [ ] **Step 4: Commit** with Task 4 (the test is red until app.js is rewritten; do not commit a red tree).

### Task 3: Tokens, layout skeleton

**Files:**
- Rewrite: `web/style.css`, `web/index.html`

- [ ] **Step 1:** `:root` tokens from spec §3, `--mono`/`--sans`, `color-scheme: dark`, explicit `body` background.
- [ ] **Step 2:** `index.html` landmarks: `#sidebar` (brand, `#new-session`, `#views` nav, `#filter`, `#banner`, `#sessions`, `#jobs`, footer `#addr` + `#open-settings`), `main` with `#chat` (`#header`, `#transcript`, `#approvals`, `#composer`, `#status`) and `#view` (table host), overlays `#settings` and `#help` as `<div role="dialog" hidden>`.
- [ ] **Step 3:** Styles: sidebar groups and indented children, glyph colours, banner, header, 780px transcript column, `.tool` rows (`--tool` name, ✓ accent, ✗ error), approval card, composer, tables, overlays, `kbd.hint`, `body.hints-off kbd.hint {display:none}`, phone width (sidebar stacks above main under 720px).

### Task 4: app.js rewrite

**Files:**
- Rewrite: `web/app.js`

- [ ] **Step 1: Core.** `api(method, tpl, params, body)` substituting `{name}`; state object `{sessions: Map, order, open, view, pending: Map<root, approvals[]>, working: Set, transcript}`.
- [ ] **Step 2: Feed.** One `EventSource("/api/events")`; `onopen` → `loadSessions()` + reload open transcript; `apply(ev)` per spec §4.2; events for the open session also update the transcript (text append, tool row create/merge, footer, notes, errors).
- [ ] **Step 3: Sidebar render.** Filter, group by workspace, children under parents, glyph, banner, jobs, footer address.
- [ ] **Step 4: Header, transcript, tool rows, go_run, footers.**
- [ ] **Step 5: Approval card** with countdown (one 1s interval while any card with `expires_at` is shown), buttons, note.
- [ ] **Step 6: Views** via one `renderTable({title, columns, rows, actions, detail})`; the five views per spec §6.
- [ ] **Step 7: Keys.** `loadKeys/saveKeys`, settings overlay switches, help overlay, `onKey` per spec §5.2.
- [ ] **Step 8: Run** `make test` → the web tests from Task 2 pass. `node --check web/app.js` if node is present.
- [ ] **Step 9: Commit** Tasks 2–4 together: `web: TUI layout, views and keyboard shortcuts`.

### Task 5: Gates and manual checklist

- [ ] `make vet && make fmtcheck && make lint && make test`.
- [ ] Headless smoke (Playwright if available) against a daemon on a scratch config: page loads without console errors, every view renders, settings toggles persist, `localStorage` throwing still loads defaults.
- [ ] Hand the manual gate (spec §7) to the human. Do not open the PR until they have run it.
