# Companion PR 3: watch alerts, research, findings, digest — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Watch runs decide their own alerts; reflection gains research, finding, follow-up and a daily digest; the two 2a leftovers are fixed.

**Architecture:** A new `companion_runs` table records every watch and research run. The daemon asks the heartbeat two things about each job: what prompt to run (`RunPrompt`, which appends the verdict suffix to watches and records a research run's session) and whether it owns the finished run (`RunFinished`, which records the row and delivers or holds an alert). Everything else is in the heartbeat tick: research jobs are created by `apply`, and reflection reads unreported runs, follow-up subjects and the digest slot from its `View`.

**Tech Stack:** Go, SQLite (modernc), existing `companion`, `store`, `daemon`, `cmd/spore` packages.

**Spec:** `docs/superpowers/specs/2026-10-10-companion-design.md`, sections "PR 3 decisions (2026-10-13)" and "Design — PR 3" (they win over earlier sections).

## Global Constraints

- Run text is never stored; replies are read with `schedule.LastReply` from the run's session. A session that no longer exists renders `(run deleted)`.
- Reflection clip sizes: watch reply 1024 runes, research reply 6144 runes; a clipped entry ends `… (clipped; full reply in job run <session>)`.
- Alert outreach text: the verdict message clipped to 1200 runes, then `\n\n· the rest is in the Jobs folder` when clipped.
- Research: question 1..300 characters; at most one `pending` row younger than 2 h; fewer than 6 started per local day; a `pending` row older than 2 h becomes `failed`; a late finish never overwrites it.
- Runs window shown to reflection: finished in the last 7 days, no `reported_outreach_id`. Rows older than 30 days (by `started_at`) are deleted by the heartbeat.
- Follow-up subjects: an active watch with ≥10 runs in 14 days and no `replied`/`reacted` outreach about its interest sent in 14 days; a watch whose last 3 runs are `failed`; in the digest slot, the threads line.
- Pending decision discarded after 12 h. A `replied` proposal whose interest is still `proposed` 24 h after `resolved_at` returns to `candidate` with `ReturnCooldown` (7 d).
- Budget: `research` and `digest` cost nothing; `finding` and `followup` count against the daily budget like proposals and nudges; alerts have `companion.alert_budget` (count of `alert` outreach since local midnight).
- New watches are created with `notify = none`. The companion branch ignores `notify`, so existing `each` watches need no migration.
- Comments, errors and commit messages follow the repo's plain-English style (codered:mission-grade is loaded for every review).
- Run the whole CI gate before claiming a task done: `go vet ./... && make fmtcheck && make lint && go test ./...` (vulncheck and tidy at the end).

## Review Focus

1. **A watch reply with the marker inside markdown** (`**ALERT:** …`, a fenced block, a leading heading) must still parse as that verdict — owned by Task 2.
2. **A watch run that finishes while the companion session has a turn running** must be held, then delivered by the next tick with no model call — Task 3 (held) and Task 5 (retry).
3. **A job that is not a companion watch or research** (a user's own cron job) must take exactly the old path: no suffix, ordinary note — Task 3.
4. **The digest slot on a day the budget is spent** must still send the digest — Task 5.
5. **A saved decision that has gone stale** (its unanswered row was answered meanwhile) must be discarded and the tick must reflect normally — Task 6.

---

### Task 1: `companion_runs` table and store methods

**Files:**
- Modify: `internal/store/schema.go` (append the table after `companion_state`)
- Create: `internal/store/companion_runs.go`
- Test: `internal/store/companion_runs_test.go`

**Interfaces:**
- Produces:
  ```go
  const (RunWatch = "watch"; RunResearch = "research")
  const (VerdictAlert = "alert"; VerdictNothing = "nothing"; VerdictFailed = "failed"; VerdictPending = "pending"; VerdictDone = "done")
  type CompanionRun struct {
      ID, InterestID, JobID int64
      Kind, SessionID, Question, Verdict string
      StartedAt, FinishedAt time.Time   // zero when unset
      ReportedOutreachID int64          // 0 when NULL
      SeenAt time.Time                  // zero when NULL/empty
  }
  func (s *Store) AddCompanionRun(ctx context.Context, r CompanionRun) (int64, error)
  func (s *Store) CompanionRunByJob(ctx context.Context, jobID int64, kind string) (CompanionRun, bool, error) // newest row for the job
  func (s *Store) SetCompanionRunSession(ctx context.Context, id int64, sessionID string) error
  func (s *Store) FinishCompanionRun(ctx context.Context, id int64, verdict string, at time.Time) (bool, error) // only from pending
  func (s *Store) CompanionRuns(ctx context.Context, since time.Time) ([]CompanionRun, error) // started_at >= since, newest first
  func (s *Store) SetCompanionRunsReported(ctx context.Context, ids []int64, outreachID int64) error // only rows still NULL
  func (s *Store) SetCompanionRunsSeen(ctx context.Context, ids []int64, at time.Time) error      // only rows still unseen
  func (s *Store) DeleteCompanionRunsBefore(ctx context.Context, before time.Time) (int64, error)
  ```

Schema (comment it like its neighbours):

```sql
CREATE TABLE IF NOT EXISTS companion_runs (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  interest_id          INTEGER NOT NULL REFERENCES interests(id) ON DELETE CASCADE,
  kind                 TEXT NOT NULL,
  job_id               INTEGER NOT NULL,
  session_id           TEXT NOT NULL DEFAULT '',
  question             TEXT NOT NULL DEFAULT '',
  verdict              TEXT NOT NULL,
  started_at           TEXT NOT NULL,
  finished_at          TEXT NOT NULL DEFAULT '',
  reported_outreach_id INTEGER REFERENCES outreach(id) ON DELETE SET NULL,
  seen_at              TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_companion_runs_started ON companion_runs(started_at);
CREATE INDEX IF NOT EXISTS idx_companion_runs_job ON companion_runs(job_id);
```

`session_id` has no foreign key on purpose: the row outlives a deleted run session.

- [ ] **Step 1: Write the failing tests** in `companion_runs_test.go` (open a store on `t.TempDir()` as `outreach_test.go` does; seed an interest with `TouchInterest`):
  - `TestCompanionRunRoundTrip`: add a finished watch row (verdict alert, both times set) and a pending research row (question "q"); `CompanionRuns(since)` returns both newest first with every field equal; `CompanionRunByJob(jobID, RunResearch)` finds the research row; an unknown job gives `false`.
  - `TestFinishCompanionRunOnlyFromPending`: finish pending → `done` returns true and sets `finished_at`; a second finish → `failed` returns false and the verdict stays `done`.
  - `TestReportedAndSeenOnlyOnce`: `SetCompanionRunsReported([a,b], 7)` then `([a], 9)` leaves a=7, b=7; `SetCompanionRunsSeen` twice keeps the first time. Empty `ids` is a no-op, not an error. (Create real outreach rows 7 and 9 first, for the FK.)
  - `TestCompanionRunsCascadeAndSweep`: deleting the interest row deletes its runs; `DeleteCompanionRunsBefore(t)` deletes only rows started before `t` and returns the count.
- [ ] **Step 2: Run** `go test ./internal/store -run 'CompanionRun' -v` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement** the schema block and `companion_runs.go`, following `outreach.go` (scan helper, `nullID`, `timeFormat`, wrapped errors). `ids` lists are expanded into `IN (?,?,…)` placeholders.
- [ ] **Step 4: Run** the same command — Expected: PASS. Then `go test ./internal/store` — PASS.
- [ ] **Step 5: Commit** `store: companion_runs records watch and research runs`.

---

### Task 2: Verdicts and run prompts

**Files:**
- Create: `internal/companion/runs.go` (pure functions in this task; Task 3 adds methods)
- Modify: `internal/companion/service.go` (`startWatch`)
- Test: `internal/companion/runs_test.go`, `internal/companion/service_test.go`

**Interfaces:**
- Produces:
  ```go
  type Verdict struct { Kind string /* store.VerdictAlert|VerdictNothing */; Message string }
  func ParseVerdict(reply string) Verdict
  const VerdictSuffix = "..."           // exact text below
  func ResearchPrompt(label, question string) string
  func AlertText(message string) string // 1200-rune clip + Jobs-folder footer
  ```

`ParseVerdict`: strip one surrounding ``` fence (with or without a language tag); take the first non-empty line; trim leading `*`, `#`, `_`, `>` and spaces, and trailing `*`/`_` around the marker; compare the prefix case-insensitively. `NOTHING:` → `{nothing, rest of that line}`. `ALERT:` → `{alert, rest of that line + the remaining lines, trimmed}`. Anything else, including empty → `{alert, whole reply trimmed}`.

Exact copy:

```go
const VerdictSuffix = "\n\nEnd your work with a verdict. Begin your reply with exactly one of these lines:\n" +
	"ALERT: <what to tell the user, in a few lines>\n" +
	"NOTHING: <one line on what you checked>\n" +
	"Use ALERT only when what you found crosses the goal's bar."
```

`ResearchPrompt(label, question)`:
`"You are running research for spore's companion, on %s. Question: %s\n\nYou run unattended, with read-only tools and your own notes; nobody will answer you. Answer the question in a few short paragraphs and list your sources (URLs, files or tool results). If you need a tool you do not have, say which and why."` (question trimmed).

`startWatch`: create the job with `Notify: store.NotifyNone`; the reply becomes `"Watching %s (job %d, cron %s UTC). I'll tell you here when a run finds something worth it."`.

- [ ] **Step 1: Write failing tests** `TestParseVerdict` (table): `"ALERT: ZS fell 6%\nmore"` → alert `"ZS fell 6%\nmore"`; `"nothing: flat day"` → nothing `"flat day"`; `"```\nNOTHING: x\n```"` → nothing `"x"`; `"**ALERT:** big move"` → alert `"big move"`; `"## Alert: y"` → alert `"y"`; `"\n\n  NOTHING: z"` → nothing `"z"`; `"no marker here"` → alert `"no marker here"`; `""` → alert `""`. `TestAlertTextClips`: 1300 `a`s → 1200 `a`s + `"\n\n· the rest is in the Jobs folder"`; a short text is unchanged. Update `TestAnswerProposalYesCreatesTheWatch` to expect `NotifyNone`.
- [ ] **Step 2: Run** `go test ./internal/companion -run 'Verdict|AlertText|AnswerProposalYes' -v` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same — PASS; `go test ./internal/companion` — PASS.
- [ ] **Step 5: Commit** `companion: watch verdicts, research prompt, new watches notify none`.

---

### Task 3: Run hooks — the daemon hands companion runs to the heartbeat

**Files:**
- Modify: `internal/companion/runs.go` (methods), `internal/companion/heartbeat.go` (`alertMu` field)
- Modify: `internal/daemon/jobs.go` (`StartJob`), `internal/daemon/checkin.go` (`afterJobRun`)
- Test: `internal/companion/runs_test.go`, `internal/daemon/checkin_test.go`

**Interfaces:**
- Consumes: Task 1 store API; Task 2 `ParseVerdict`, `VerdictSuffix`, `AlertText`; `schedule.LastReply` (`internal/tool/schedule`, no import cycle).
- Produces:
  ```go
  // RunPrompt is the prompt a job runs with. A companion watch gets
  // VerdictSuffix appended; a research job has its run session recorded.
  func (h *Heartbeat) RunPrompt(ctx context.Context, job store.Job, sessionID string) string
  // RunFinished records a companion run. handled is true when the job is
  // the companion's (the caller then skips its ordinary note path);
  // failNote is true only for a failed watch run.
  func (h *Heartbeat) RunFinished(ctx context.Context, job store.Job, sessionID string, ok bool) (handled, failNote bool)
  // deliverAlert sends one alert row now or leaves it held; it reports
  // whether it was sent. It takes h.alertMu.
  func (h *Heartbeat) deliverAlert(ctx context.Context, run store.CompanionRun, now time.Time) (bool, error)
  // RetryHeldAlerts tries every held alert (watch, verdict alert,
  // reported_outreach_id NULL, started in the last 30 days). Used by Task 5.
  func (h *Heartbeat) RetryHeldAlerts(ctx context.Context, now time.Time) error
  ```

Rules:
- Both hooks return the untouched prompt / `false` when `!h.Cfg.Companion.Enabled`.
- A job is a **watch** when an `active` interest has `WatchJobID == job.ID`; it is **research** when `CompanionRunByJob(job.ID, RunResearch)` finds a row. Neither → plain job.
- `RunFinished` for a watch: read the reply (`schedule.LastReply`) only when `ok`; verdict = `failed` when `!ok`, else `ParseVerdict(reply).Kind`; `AddCompanionRun` with both times `now`. If `alert`, call `deliverAlert`. Errors are logged (`slog.Warn`) and the method still reports `handled`.
- `RunFinished` for research: `FinishCompanionRun(row.ID, done|failed, now)`; a `false` (already failed by the stuck sweep) is logged at debug and ignored. Reports `handled`, never `failNote`.
- `deliverAlert` holds when: paused (`PausedUntil`), quiet hours unless `AlertsInQuietHours`, alerts sent today ≥ `AlertBudget`, or `Deliverer.Deliver` returns `ErrBusy`. On send: `Outreach{InterestID, Kind: OutreachAlert, Text: AlertText(verdict message), SentAt: now}`, then `SetCompanionRunsReported([run.ID], o.ID)`. The text is re-read from the run's session each time, so a retry needs no stored text. A session gone → no send, row stays held until the digest takes it.
- Daemon `StartJob`: `prompt := job.Prompt; if s.heartbeat != nil { prompt = s.heartbeat.RunPrompt(ctx, job, sessionID) }` and pass `prompt` to `startTurnThen`.
- Daemon `afterJobRun`: compute `ok` right after the job is read, then, **before** the `OriginSessionID == ""` return: if `s.heartbeat != nil`, call `RunFinished`; when `handled`, post `runNote(job, end, "")` to `job.OriginSessionID` if `failNote` and the origin is set, and return.

- [ ] **Step 1: Write failing companion tests** (reuse `newHeartbeat`; add helper `fx.activeWatch(t, key) (store.Interest, store.Job)` that creates a cron job and `ActivateInterest`; write the run's reply into a session with `AppendMessage` role assistant):
  - `TestRunPromptAddsTheSuffixOnlyToWatches`: watch job → prompt ends with `VerdictSuffix`; a plain job → unchanged; a research job (row pending) → unchanged prompt and the row's `session_id` is now set.
  - `TestWatchNothingPostsNothing`: reply `"NOTHING: flat"` → handled, one row verdict `nothing`, `fx.del.sent` empty.
  - `TestWatchAlertIsDelivered`: reply `"ALERT: ZS -6%"` → one `alert` outreach with text `"ZS -6%"`, row `reported_outreach_id` = its id.
  - `TestWatchAlertHeld` (subtests): over `AlertBudget` (seed 3 alert outreach today); quiet hours with flag off (`fx.now` at 23:00); paused; `fx.del.err = companion.ErrBusy` → nothing sent and row unreported. Subtest: quiet hours with `AlertsInQuietHours = true` → sent.
  - `TestRetryHeldAlertsSendsWithoutAModelCall`: hold on ErrBusy, clear the error, `RetryHeldAlerts` → sent, `len(fx.refl.inputs) == 0`.
  - `TestFailedWatchRunIsRecordedFailed`: `ok=false` → row `failed`, `failNote` true, nothing sent.
  - `TestDisabledCompanionLeavesJobsAlone`: `Enabled=false` → prompt unchanged, `handled=false`.
- [ ] **Step 2: Write the failing daemon test** in `checkin_test.go`: `TestCompanionWatchSkipsTheOrdinaryNote` — build the server with `checkInFixture` scripted reply `"NOTHING: quiet"`, attach a `companion.Heartbeat{Store, Cfg (companion enabled, timezone UTC), Deliverer: s.CompanionDeliverer(), Now}` with `s.AttachHeartbeat`, make the job a `notify=each`, `CheckedIn` watch of an active interest, `runJob` it → no note in the origin session and the `fakeNotifier` delivered nothing; then a plain `notify=each` job still posts its note (regression guard).
- [ ] **Step 3: Run** `go test ./internal/companion ./internal/daemon -run 'RunPrompt|Watch|RetryHeld|Disabled|CompanionWatch' -v` — FAIL.
- [ ] **Step 4: Implement** the methods, the `alertMu sync.Mutex` field on `Heartbeat`, and the two daemon call sites.
- [ ] **Step 5: Run** the same — PASS; `go test ./internal/companion ./internal/daemon` — PASS.
- [ ] **Step 6: Commit** `companion: watch runs decide their own alerts`.

---

### Task 4: Reflection decision, view, validation, input and prompt

**Files:**
- Modify: `internal/companion/reflect.go`
- Test: `internal/companion/reflect_test.go`

**Interfaces:**
- Produces:
  ```go
  const (ActionResearch = "research"; ActionFinding = "finding"; ActionFollowup = "followup"; ActionDigest = "digest")
  // Actions gains all four.
  type IDList []int64 // UnmarshalJSON accepts null, a number, or an array of numbers
  type ResearchAsk struct { Question string `json:"question"` }
  // Decision gains: Research *ResearchAsk `json:"research"`; Runs []int64 `json:"runs"`;
  // NudgeOf changes type to IDList (a nudge needs exactly one id).
  type RunView struct {
      Run   store.CompanionRun
      Key   string // interest key
      Reply string // full reply, or "(run deleted)"
      Held  bool   // watch alert with no reported_outreach_id
  }
  // View gains: Runs []RunView; Followups []string; DigestDue bool;
  // ResearchToday int; ResearchInFlight bool; ForceResearch string.
  func (v View) RunByID(id int64) (RunView, bool)
  const (WatchClip = 1024; ResearchClip = 6144; ResearchQuestionMax = 300; ResearchPerDay = 6)
  ```

Validation (add to `Validate`; existing rules unchanged except `nudge` reads `d.NudgeOf[0]` and needs `len == 1`):
- `v.DigestDue` and action ≠ `digest` → error `"the digest is due: the only valid action is digest"`; action `digest` and `!v.DigestDue` → error.
- `research`: interest shown and `active`; question 1..300 runes after trim; `!v.ResearchInFlight`; `v.ResearchToday < 6`; `message` must be empty.
- `finding`: `runs` non-empty, each `RunByID` found; `interest`, if set, shown.
- `followup`: `interest`, if set, shown.
- `digest`: each `runs` id found; each `nudge_of` id is unanswered, not already nudged, and not itself a `digest`.
- `nudge`: the target must not be a `digest` (a digest is never nudged).
- Message length 1..1200 for every action except `none` and `research`. Budget (`BudgetLeft >= 1`) for `propose`, `nudge`, `finding`, `followup` only.

`Eligible` adds: any `RunView` with zero `SeenAt`, any `Held`, `len(Followups) > 0`, `DigestDue`, `ForceResearch != ""`. `Fingerprint` adds lines `r <id> <held>` for each unseen run, `h <id>` for each held, `f <subject>` for each follow-up, `digest <bool>`.

`RenderInput` adds, after "Unanswered": 
- `## Runs since you last looked` — `(none)` or per run: `- run #<id> <kind> <key>: <verdict>, <N>h ago[ (held: not sent)][ (seen before)][, question: "<q>"]` then the reply on the next lines indented two spaces, clipped with `clip` at `WatchClip`/`ResearchClip` runes and, when clipped, suffixed `… (clipped; full reply in job run <session>)` (replace the `…` that `clip` adds).
- `## Follow-up subjects` — `(none)` or one `- ` line each.
- `Research today: <n> of 6[; one is running].` on the header line under the budget line.
- When `DigestDue`: `## The digest is due\nThis tick is the morning digest slot: answer with action "digest" and nothing else.`
- When `ForceResearch != ""`: `## Operator check\nThis is a verification run started by the user: answer with action "research" for interest <key>, with a question of your choosing.`
- Unanswered digests get the note ` (a digest: never nudge it)`.

`ReflectionPrompt` becomes (exact copy; keep propose/nudge/none bullets as they are and insert the new ones before "do nothing"):

```
- research an active interest: ask one question (1..300 characters) that a run of its own answers with read-only tools and sources. It sends the user nothing; you see its answer under "Runs since you last looked" on a later tick. Only one research run at a time, and at most 6 a day.
- report a finding: tell the user what one or more runs found that is worth their attention, and list the run ids in "runs".
- follow up: raise a follow-up subject you are shown, or a thread from your notes that has come due.
- write the digest: only when the input says the digest is due, and then it is the only action you may take. One short morning message: what the runs found since the last digest, anything unanswered folded in (list those ids in "nudge_of"), and what is coming. Send it even when it is short.
```

Add after the "Every unanswered message…" paragraph: `Watch runs send their own alerts; you see every run the user has not been told about. "Messages you may still start today" counts proposals, nudges, findings and follow-ups; research and the digest do not.` A digest is never nudged (append to the nudge bullet). Replace the JSON schema with:

```
{"action": "propose" | "nudge" | "research" | "finding" | "followup" | "digest" | "none",
 "interest": "<interest key: for propose and research; optional for finding and followup>",
 "message": "<what to send, 1..1200 characters; empty for research and none>",
 "watch": {"cron": "<five-field cron, UTC>", "goal": "<what to check, 1..500 characters>"} or null,
 "research": {"question": "<1..300 characters>"} or null,
 "runs": [<run ids, for finding and digest>],
 "nudge_of": <outreach id, for nudge> or [<outreach ids>, for digest],
 "drops": [{"outreach": <id>, "reason": "<1..200 characters>"}],
 "self_md": null or "<the full new self.md>"}
```

- [ ] **Step 1: Write failing tests** (pure, build `View` literals): `TestIDListAcceptsNumberOrArray` (`5`, `[5,6]`, `null`, absent); one table test `TestValidatePR3Actions` with a row per rule above (valid and invalid case each), including "digest due + propose → error", "digest outside slot → error", "research with message → error", "research 7th of the day → error", "research in flight → error", "nudge a digest → error", "finding with budget 0 → error", "digest with budget 0 → ok"; `TestRenderInputRuns` (watch reply of 2000 runes clipped to 1024 with the marker and session id, research reply of 7000 clipped at 6144, `(held: not sent)`, `(seen before)`, `(run deleted)`); `TestRenderInputDigestAndOperatorResearch`; `TestEligibleAndFingerprintSeeRuns` (unseen run makes eligible and changes the fingerprint; a seen, unheld run alone does not make eligible).
- [ ] **Step 2: Run** `go test ./internal/companion -run 'IDList|PR3|RenderInput|EligibleAndFingerprint' -v` — FAIL.
- [ ] **Step 3: Implement**; fix existing `d.NudgeOf` uses in `heartbeat.go` (`d.NudgeOf[0]`, summarize).
- [ ] **Step 4: Run** — PASS; `go test ./internal/companion` — PASS (existing reflect tests unchanged).
- [ ] **Step 5: Commit** `companion: reflection sees runs and can research, report, follow up and digest`.

---

### Task 5: Heartbeat tick — housekeeping, view, apply

**Files:**
- Modify: `internal/companion/heartbeat.go`, `internal/companion/state.go`, `internal/store/outreach.go` (one const)
- Test: `internal/companion/heartbeat_test.go`

**Interfaces:**
- Consumes: Tasks 1–4.
- Produces: `StateLastDigestDay = "last_digest_day"` (local `2006-01-02`); `store.OutreachChannelDigest = "digest"`; `const ResearchStuckAfter = 2*time.Hour; RunsShownFor = 7*24*time.Hour; RunsKeptFor = 30*24*time.Hour; FollowupWindow = 14*24*time.Hour; FollowupIdleRuns = 10; FollowupFailedRuns = 3`.

Tick order after `Recorder.Sweep`: (1) every `pending` research row started ≥ 2 h ago → `FinishCompanionRun(failed)`; (2) `DeleteCompanionRunsBefore(now-30d)`; (3) `RetryHeldAlerts`; then promote, pause/quiet, view, (Task 6 pending decision), gate, reflect.

`view` additions:
- `Runs`: from `CompanionRuns(now-30d)`, keep `verdict != pending`, `ReportedOutreachID == 0`, `FinishedAt >= now-7d`; read each reply with `schedule.LastReply` (a missing session via `Store.Session` → `(run deleted)`); `Held` = watch ∧ alert.
- `ResearchToday`: research rows started since `LocalDayStart`; `ResearchInFlight`: any research row still `pending`.
- `DigestDue`: `last_digest_day != today` (local), and false when `ForceResearch` or `Force` is set. (The tick has already returned on pause/quiet for non-manual ticks.)
- `Followups`: per the Global Constraints. Exact lines: `watch <key> has run <n> times in 14 days with no reply or reaction to anything about it: ask whether to keep, change or retire it`; `watch <key> failed its last 3 runs: tell the user, or suggest changing or retiring it`; in the slot `read "Threads with you" in your notes and raise any that has come due`.

Gate: `budget spent` only when `BudgetLeft < 1 && !v.DigestDue && v.ForceResearch == ""`.

`apply` additions (message first, as today):
- `research`: `CreateJob(Job{Kind:"once", Spec: now.UTC().Format(time.RFC3339), Prompt: ResearchPrompt(in.Label, q), Enabled: true, NextRun: now, Notify: NotifyNone, CheckedIn: true, Profile: config.CompanionProfile})`, then `AddCompanionRun{InterestID, Kind: research, JobID, Question: q, Verdict: pending, StartedAt: now}`.
- `finding` / `followup`: `Deliver` with that kind (InterestID from `interest` if set); `finding` then `SetCompanionRunsReported(d.Runs, id)`.
- `digest`: `Deliver` kind digest; `SetCompanionRunsReported(d.Runs ∪ every Held id in v, id)`; for each `nudge_of` id, `AddOutreach(Kind: nudge, NudgeOf: id, Text: "(folded into digest #<id>)", Channel: OutreachChannelDigest, SentAt: now)`; resolve every earlier digest still `pending`/`unanswered` with `Store.ResolveOutreach(..., OutcomeIgnored, "superseded by the next digest", now)` (no governor step); write `last_digest_day` last.
- After any successful apply: `SetCompanionRunsSeen(every run id in v.Runs, now)` before `succeed`.

`SpentToday` also skips kind `digest` and channel `digest`. `summarize` names the new actions: `research <key>`, `finding`, `followup`, `digest`.

- [ ] **Step 1: Write failing tests** (fixture helpers from Task 3; `fx.reply(t, sid, text)` writes an assistant message):
  - `TestTickResearchCreatesAJobAndRow` (job: once, profile companion, no origin, notify none, checked in; row pending with question).
  - `TestStuckResearchFailsAndLateFinishIsIgnored` (row 3 h old → failed after a tick; `RunFinished(ok=true)` leaves it failed).
  - `TestTickFindingMarksRunsReported` and spends budget (`SpentToday` 1).
  - `TestFollowupCountsAgainstTheBudget` (BudgetLeft 0 → validation error, nothing sent).
  - `TestDigest` subtests: slot due at 08:30 → only digest valid (a `none` reply is a failure and the slot stays due); a digest sent with budget spent; `last_digest_day` written only after delivery (deliverer error → unset); none while paused; previous digest resolved `ignored` with the reason and budget unchanged; `nudge_of [a,b]` creates two folded nudges and `Nudged` is true for both next tick; a held alert gets the digest's id and is not retried after.
  - `TestSeenAtOnlyAfterSuccess` (invalid reply → `seen_at` stays zero; valid → set; next input shows `(seen before)`).
  - `TestRunsOlderThan30DaysAreSwept`.
  - `TestFollowupSubjects` (10 nothing runs in 14 days → idle line; 3 failed in a row → failed line; a reacted outreach about the interest → no idle line).
- [ ] **Step 2: Run** `go test ./internal/companion -run 'Research|Finding|Followup|Digest|SeenAt|Swept|FollowupSubjects' -v` — FAIL.
- [ ] **Step 3: Implement.** Keep `TickWith` readable: move the housekeeping into `h.housekeep(ctx, now)` and the run part of the view into `h.viewRuns(ctx, &v, now, loc)`.
- [ ] **Step 4: Run** — PASS; `go test ./internal/companion` — PASS (update `TestTickBudgetSpent` only if it hits the digest slot: set `last_digest_day` to today in it).
- [ ] **Step 5: Commit** `companion: heartbeat runs research, findings, follow-ups and the morning digest`.

---

### Task 6: The 2a leftovers

**Files:**
- Modify: `internal/companion/heartbeat.go`, `internal/companion/state.go`
- Test: `internal/companion/heartbeat_test.go`

**Interfaces:**
- Produces: `StatePendingDecision = "pending_decision"`; `const PendingDecisionMaxAge = 12 * time.Hour; RepliedProposalReturn = 24 * time.Hour`.

- **Text-answered proposal:** at the end of `resolveStale`, for each `OutreachByOutcome(replied)` row of kind proposal with `ResolvedAt <= now-24h`, call `returnProposal` (it already moves only a `proposed` interest).
- **Busy session:** when `apply` returns `ErrBusy` from a reflection, store `{"decision": d, "saved_at": now}` as JSON under `StatePendingDecision` and return `"companion session busy (decision saved)"`. After the view is built and before the gate: if a saved decision exists — older than 12 h or failing `Validate(d, v)` → clear it and continue normally; valid → `apply`; `ErrBusy` again → keep it and return `"companion session busy"`; other error → clear and `fail`; success → clear, mark runs seen, `succeed(fp, summarize(d)+" (saved decision)")` and return. A saved decision also counts in `Eligible`'s place: the check runs before the gate, so nothing else is needed.

- [ ] **Step 1: Write failing tests:** `TestRepliedProposalReturnsToCandidateAfterADay` (23 h → still proposed; 25 h → candidate with cooldown now+7d); `TestBusyDecisionIsAppliedNextTickWithoutAModelCall` (ErrBusy on propose → saved; clear err, tick → proposal sent, `len(fx.refl.inputs)` unchanged, state key empty); `TestSavedDecisionDiscardedWhenStaleOrInvalid` (13 h old → discarded and the tick reflects; a saved nudge whose target was answered meanwhile → discarded and the tick reflects).
- [ ] **Step 2: Run** `go test ./internal/companion -run 'RepliedProposal|BusyDecision|SavedDecision' -v` — FAIL. Update `TestTickBusyIsNotAFailure` for the new result string.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS; `go test ./internal/companion` — PASS.
- [ ] **Step 5: Commit** `companion: text-answered proposals return, busy decisions are kept`.

---

### Task 7: `spore companion check --research <key>`

**Files:**
- Modify: `internal/companion/heartbeat.go` (`TickOptions.Research`), `internal/daemon/companion.go` (`handleCompanionCheck` body field `research`), `cmd/spore/companion.go` (`--research` flag, usage, doc comment)
- Test: `internal/companion/heartbeat_test.go`, `internal/daemon/companion_test.go`

Rules: `Research` with `Promote` or `Force` → `ErrBadCheck` ("--research cannot be combined with --promote"); interest missing or not `active` → `ErrBadCheck` naming its state; otherwise `v.ForceResearch = key`. CLI usage: `spore companion check [--promote <key> [--force]] [--research <key>]`.

- [ ] **Step 1: Write failing tests:** `TestCheckResearch` (non-active → ErrBadCheck; active → input contains the operator research line and a valid research reply creates the job); daemon `TestCompanionCheckResearchBadRequest` (POST `{"research":"nope"}` → 400).
- [ ] **Step 2: Run** `go test ./internal/companion ./internal/daemon -run 'CheckResearch' -v` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `companion: check --research starts a research run on demand`.

---

### Task 8: Status lines

**Files:**
- Modify: `internal/companion/service.go` (`Status`), `internal/companion/command.go` (`writeStatus`), `internal/daemon/companion.go` (`CompanionStatusJSON`)
- Test: `internal/companion/service_test.go`, `internal/companion/command_test.go`

`Status` gains `DigestSentAt time.Time` (today's digest outreach, zero if none), `DigestDue bool`, `ResearchToday, ResearchRunning, AlertsToday, AlertsHeld, AlertBudget int`. `writeStatus` prints, after `budget:`:

```
digest:    sent 08:02          (or: due, or: paused)
research:  2 of 6 today, 1 running
alerts:    1 of 3 today, 1 held
```

(`paused` wins over the others; `sent` uses local `15:04`; `, N running` / `, N held` only when N > 0.) JSON gains `digest_sent_at` (omitempty), `digest_due`, `research_today`, `research_running`, `alerts_today`, `alerts_held`.

- [ ] **Step 1: Write failing tests:** extend `TestStatus` for the new fields; `TestStatusPrintsDigestResearchAlerts` checks the three lines for (sent, 2 research with 1 pending, 1 alert sent + 1 held) and for (due), (paused).
- [ ] **Step 2: Run** `go test ./internal/companion -run 'Status' -v` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `companion: status shows the digest, research and alerts`.

---

### Task 9: Docs and the whole gate

**Files:**
- Modify: the spec (append `## As built — PR 3`), `docs/backlog.md` if a leftover is found, any user doc that says a watch posts every run (`grep -rn "Every run will land\|notify = each" docs README.md`).

- [ ] **Step 1:** Fix the doc hits; write "As built — PR 3" listing every deviation from the design taken during the build.
- [ ] **Step 2:** Run the whole CI gate: `go vet ./... && make fmtcheck && make lint && make vulncheck && go mod tidy -diff && go test ./...` — all green. Also run the companion tests with a symlinked TMPDIR (`mkdir -p $SCRATCH/real && ln -sfn $SCRATCH/real $SCRATCH/link && TMPDIR=$SCRATCH/link go test ./internal/companion ./internal/daemon`).
- [ ] **Step 3:** Commit `docs: companion PR 3 as built`.

The manual gate (isolated daemon on :7788 with the real model; never on the bot token) is run by the controller after Task 9, per the spec's Testing section.
