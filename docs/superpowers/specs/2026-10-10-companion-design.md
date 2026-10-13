# spore — companion: initiative, habits and a self

**Date:** 2026-10-10
**Status:** PR 1, 2a, 2b shipped; PR 3 design appended 2026-10-13, awaiting review
**Builds on:** `2026-09-22-soul-and-agent-files-design.md` (soul.md, the prompt
prefix), `2026-09-26-refinement-design.md` (the reviewer round and its ledger),
`2026-09-22-job-checkin-design.md` (jobs reporting into their origin chat)

## 1. What this adds

Spore already remembers things about the user (memory facts, written by the
refiner), has a voice (`soul.md`), and can run on a schedule (jobs). It still
only speaks when spoken to. A fact is a single statement, so nothing notices
that the user asked about ZS on five separate days, and nothing turns that into
"ZS dropped 6% after earnings -- want the breakdown?"

The companion makes spore start conversations. It:

1. **observes** recurring interests across sessions and counts the evidence;
2. **proposes** acting on a habit once it is established, and acts only after a
   yes;
3. **watches and researches** approved interests unattended, with read-only
   tools;
4. **reaches out** on a configurable channel, at a cadence that starts
   conversational and settles toward a digest plus alerts, steered by how the
   user engages;
5. keeps **its own self** in `self.md`: curiosities, open threads with the
   user, how the user likes to be talked to, opinions it has formed.

"Alive" here means memory, initiative and follow-through. It does not mean
simulated feelings.

### User decisions (brainstorming, 2026-10-10)

| decision | answer |
|---|---|
| Initiative | Propose first; once approved, act autonomously |
| Channel | Configurable; default Discord when configured, else the terminal |
| Cadence | Start conversational (option 3), settle toward digest + alerts (option 2) by engagement |
| Self | spore writes `self.md`; `soul.md` stays the user's and wins any conflict |
| Autonomy | Read-only tools plus spore's own notes; anything else becomes a proposal |
| Approach | A companion loop built on refinement, the scheduler and the bridge |
| Habit bar | Seen on 3 distinct local days |
| self.md cap | Configurable, default 10 KiB |

## 2. Data and files

### 2.1 `interests`

What spore believes the user cares about.

| column | type | meaning |
|---|---|---|
| `id` | INTEGER PK | |
| `key` | TEXT UNIQUE | normalized handle, e.g. `stock:zs`, `topic:daughter-reading` |
| `label` | TEXT | human text, e.g. "Zscaler (ZS) share price" |
| `state` | TEXT | `observing`, `candidate`, `proposed`, `active`, `declined`, `retired` |
| `watch_job_id` | INTEGER NULL | the scheduler job, once a watch is approved |
| `cooldown_until` | TEXT | set by "Not now"; no proposal before this time |
| `declined_at` | TEXT | set by "Never" |

`days_seen`, `first_seen` and `last_seen` are not columns. They are derived
from `interest_signals` on every read (`COUNT(DISTINCT day)`, `MIN`/`MAX` of
`seen_at`), so deleting a session corrects them with no recompute step.

State transitions:

```
observing --(days_seen >= habit_days)--> candidate
candidate --(heartbeat sends a proposal)--> proposed
proposed  --Yes--> active          (watch job created)
proposed  --Not now--> observing   (cooldown_until = now + 14d)
proposed  --Never--> declined
observing/candidate --(no signal for 30d)--> retired
active    --(watch deleted, or user retires it)--> retired
declined  --(spore companion interests undecline)--> observing
retired   --(new signal)--> observing
```

### 2.2 `interest_signals`

One row per sighting: `id`, `interest_id` (FK, cascade), `session_id` (FK to
`sessions`, ON DELETE CASCADE), `seen_at`, `day` (the local calendar date of
`seen_at` in `companion.timezone`, fixed when the row is written), `kind`
(`asked`, `mentioned`, `acted`). This is the evidence a proposal cites ("you asked about ZS on Oct 3,
6, 8 and 10"). `days_seen`, `first_seen` and `last_seen` are derived from
signals at read time (§2.1).

### 2.3 `outreach`

Every message spore starts on its own: `id`, `interest_id` (nullable),
`kind` (`proposal`, `alert`, `finding`, `followup`, `digest`, `nudge`),
`channel` (`discord`, `terminal`, `terminal-fallback`), `text`, `sent_at`,
`outcome` (`pending`, `unanswered`, `replied`, `reacted`, `ignored`, `stop`),
`nudge_of` (nullable: the outreach a `nudge` follows up), `drop_reason`,
`resolved_at`. Only the governor and the reflection input read it.

An unanswered message is never dropped silently. When 12 hours pass with no
reply, reaction or button press in the companion session, a `pending` outreach
becomes `unanswered` -- not `ignored` -- and the next reflection must decide
what to do with it (§4.6): follow it up with a `nudge`, or drop it with a
recorded reason. It becomes `ignored` only after that decision: when its nudge
has itself gone 24 hours unanswered, or when reflection drops it. It is
`replied` when the user sends a message there first; `reacted` for a Discord reaction or a button press; `stop` when the
user's reply asks spore to stop or be quieter (decided by the companion turn
through the `companion_feedback` tool, §5.3).

### 2.4 `companion_state`

A key-value table (`key` TEXT PK, `value` TEXT, `updated_at`) for the
companion's singletons: `session_id` (the companion session, §5.1),
`daily_budget` (§4.7), `paused_until`, `last_reflection_at`,
`reflection_failures`.

### 2.5 `self.md`

`<DataDir>/self.md`, i.e. `~/.spore/self.md`. Written by spore only through the
heartbeat (§4) and the `self_note` tool (§5.4); the user may edit it by hand.
Fixed headings:

```
## What I'm curious about
## Threads with you
## How you like to be talked to
## Opinions I've formed
```

It is assembled into the stable prompt prefix directly below `soul.md` as
`## Your own notes`, prefixed by one line: "These are your own notes. Where
they conflict with 'Who you are', 'Who you are' wins." An absent file renders
no section.

Size cap: `self_max_bytes` (default 10240). A write that would exceed it is
refused; the reflection call must rewrite, not append. Every write is ledgered
in `refinements` under a new kind `self.update` with before/after content, so
the existing rollback works. Because it changes only on a heartbeat rewrite,
the prompt-cache prefix stays stable between rewrites.

### 2.6 Facts are unchanged

Interests are the working layer; facts are the settled layer. A durable habit
("checks ZS most mornings") still becomes an ordinary `user` fact through the
refiner, under its existing trust rules.

### 2.7 Config

```toml
[companion]
enabled = false               # off until the user turns it on
channel = "auto"              # auto | discord | terminal
quiet_hours = "22:00-08:00"   # local time in `timezone`
timezone = ""                 # IANA name, e.g. "America/Los_Angeles"; empty = the daemon host's zone
habit_days = 3
heartbeat = "30m"
self_max_bytes = 10240
start_budget = 4              # unprompted messages per day at the start
alert_budget = 3              # alerts per day, separate from start_budget
alerts_in_quiet_hours = false
```

`enabled` defaults to `false` so an upgrade does not start messaging anyone.
Validation: `channel` from the closed set; `quiet_hours` parses as `HH:MM-HH:MM`
(may wrap midnight, start != end); `timezone` loads with
`time.LoadLocation`; `habit_days` 1..30; `heartbeat` >= 5m;
`self_max_bytes` 1024..65536; budgets >= 1.

## 3. Observing

### 3.1 Signals from refinement

The refinement planner's JSON output gains an optional list:

```json
"signals": [{"key": "stock:zs", "label": "Zscaler (ZS) share price", "kind": "asked"}]
```

Validated in Go; anything invalid is dropped and logged, never fatal to the
round:

- `key` matches `^(stock|crypto|topic|person|place|project):[a-z0-9._-]{1,48}$`
  after lowercasing;
- `kind` is in `{asked, mentioned, acted}`;
- `label` is 1..80 characters;
- at most 10 signals per round; duplicate keys in one round are merged.

The planner prompt gains a short section telling it what a signal is (a
recurring interest of the user's, not a one-off fact) and to reuse keys it is
shown: the round's input lists existing interest keys and labels so `stock:zs`
does not become `stock:zscaler` next week.

### 3.2 Trust

Signals only raise a count; every action needs the user's yes. So signals are
recorded from `chat` and Discord sessions, unlike fact edits, which Discord
sessions may only propose. Signals from job, companion and sub-agent sessions
are refused. Without this, spore's own ZS watch would count as the user checking
ZS and the interest would feed itself.

### 3.3 Counting days

`days_seen` counts distinct calendar days in `companion.timezone`.
Several sightings on one day count once. When `days_seen` reaches
`habit_days` and the state is `observing` (and `cooldown_until` has passed),
the state becomes `candidate`. Nothing is sent at this point.

### 3.4 Fading

An `observing` or `candidate` interest with no signal for 30 days becomes
`retired`. `active` interests never fade on their own; an active watch with
no engagement for 14 days is a `followup` subject (§4.5).

## 4. The heartbeat

### 4.1 The tick

A daemon goroutine ticks every `heartbeat`. It starts after the scheduler and
the notifier are wired (the ordering `serve.go` already enforces for jobs).
Each tick:

1. **Gate (no LLM):** skip when companion is disabled or paused, when it is
   quiet hours, when the daily budget is spent, or when nothing changed since
   the last reflection. Changed means any of: a new `candidate`; a watch or
   research run finished; a `self.md` thread past its follow-up time; an
   outreach resolved; the first tick of the local day (the digest slot).
2. **Resolve stale outreach:** mark `pending` rows older than 12 h
   `unanswered`, and `pending` nudges older than 24 h `ignored` together with
   the outreach they follow up. A newly `unanswered` row counts as a change
   for the gate; during quiet hours it simply waits for the first tick after
   them.
3. **Reflect:** one LLM call on a new router site `companion` (added to
   `router.Sites` and `GlobalSites`, so `/model` can route it).
4. **Validate and apply** (§4.3).

The heartbeat reads current state, never a backlog of missed ticks: a daemon
restart or a night of quiet hours never produces a burst of messages.

### 4.2 Reflection input

`soul.md`; `self.md`; candidate, proposed and active interests with their
signal dates; the last 7 days of `outreach` rows with outcomes; results of
watch and research runs since the last reflection (their final assistant text,
capped at 2 KiB each); the local time; the remaining budget and alert
allowance.

### 4.3 Reflection output

```json
{"action": "propose",
 "interest": "stock:zs",
 "message": "You've checked ZS four mornings this week. Want me to keep an eye on it and ping you on moves over 5%?",
 "watch": {"cron": "0 14 * * 1-5", "goal": "Check ZS. Alert on a daily move over 5% or material news."},
 "research": null,
 "self_md": null}
```

| field | rule |
|---|---|
| `action` | one of `propose`, `alert`, `finding`, `followup`, `digest`, `research`, `nudge`, `none` |
| `interest` | required for `propose`, `alert`, `research`; must exist; `propose` requires state `candidate` |
| `message` | required unless `none`/`research`; 1..1200 characters |
| `watch` | required for `propose`; `cron` parses with the scheduler's parser and fires at most hourly; `goal` 1..500 chars |
| `research` | for `research`: `{"question": "..."}`, 1..300 chars; interest must be `active` |
| `self_md` | optional full replacement; must contain the four headings and fit `self_max_bytes` |
| `nudge_of` | required for `nudge`: an outreach id in state `unanswered` that has no nudge yet |
| `drops` | optional list of `{"outreach": id, "reason": "..."}` for `unanswered` rows reflection chooses not to follow up; reason 1..200 chars |

Every `unanswered` row in the input must be either nudged in this output,
listed in `drops`, or carried over: a row left unmentioned stays `unanswered`
and is shown again next tick. A `digest` may fold several unanswered items into
one message by listing them in `nudge_of` as an array.

At most one outreach per tick. `none` is valid and expected to be the common
answer. An `alert` counts against `alert_budget`; every other outreach counts
against the daily budget. Invalid output skips the tick: nothing is sent and
nothing is written.

### 4.4 Research runs

`research` starts one turn in a fresh session with source `companion`, under
the `companion` policy profile (§5.4), prompted with the interest, its label,
and the question. Its result is fed to the next reflection, which may write it
into `self.md` threads and may or may not tell the user (a `finding`). Spore
can learn without reporting every lesson.

At most one research run at a time, and at most 6 per local day.

### 4.5 Follow-ups and the digest

- **Follow-up:** reflection may raise a `self.md` thread ("he was deciding
  whether to sell ZS before earnings -- earnings were yesterday") or a stale
  watch ("your ZS watch has run 10 times with no reply -- keep it?").
- **Digest:** on the first tick of the local day outside quiet hours, the
  reflection is told this is the digest slot and may batch everything worth
  saying into one `digest` message.

### 4.6 Following up unanswered messages

The user not answering is information, not a reason to forget. Reflection sees
each `unanswered` outreach with its age and kind, and chooses:

- **nudge:** a short follow-up that refers back to the original ("Did you see
  the ZS note from this morning? It's down another 2% since."). A nudge
  should add something -- new information, a simpler question, or an easy way
  to say no -- rather than repeat the message.
- **drop:** with a reason, recorded in `drop_reason`, e.g. an alert about a
  move that has since reversed. Dropping is a decision the model writes down,
  never a timeout.

At most one nudge per outreach, so spore does not nag. A nudge counts against
the daily budget. For a `proposal`, a dropped or nudge-ignored proposal does not
decline the interest: it goes back to `candidate` with `cooldown_until = now +
7d`, so it can be raised again later.

### 4.7 The governor (cadence 3 → 2)

State: `daily_budget` (float, persisted in a `companion_state` key-value
table), starting at `start_budget`. On each outreach resolution:

| outcome | effect |
|---|---|
| `replied` / `reacted` | `+0.25`, capped at `start_budget` |
| `ignored` (after its nudge, or dropped) | `-0.5` |
| `unanswered` | no change yet |
| `stop` | halve |

Floor: 1. At the floor spore sends at most the digest each day plus alerts from
approved watches, which is option 2. Alerts have their own `alert_budget` and
are the only outreach that may be sent in quiet hours, and only when
`alerts_in_quiet_hours = true`. The integer part of `daily_budget` is the
count for the day.

## 5. Reaching out and acting

### 5.1 The companion session

One long-lived session with source `companion`, created on first outreach and
recorded in `companion_state`. Every outreach is appended to it as an
assistant message before it is delivered, so the model has the context when
the user answers. If the user deletes it, the next outreach creates a new one;
approved watches keep running and their notifications go to the new session.

### 5.2 Channels

```go
type Channel interface {
    Name() string
    Send(ctx context.Context, sessionID string, o Outreach) error
}
```

- `channel = "auto"` resolves to `discord` when `[bridge.discord]` is
  configured and connected, else `terminal`.
- **discord:** a DM to the first entry in `bridge.discord.user_ids`, bound to
  the companion session through `bridge_bindings`, so a reply in the DM
  continues that session through the existing bridge path.
- **terminal:** the companion session appears pinned at the top of the TUI
  sidebar as **spore**, with an unread badge using the `seen_seq` mechanism the
  jobs folder uses, and in the web UI session list.
- If the Discord send fails, the outreach is delivered to the terminal and
  recorded with channel `terminal-fallback`. It is never dropped.

### 5.3 Proposals: buttons, not interpretation

A `propose` outreach carries three actions:

| action | Discord button | TUI key | effect |
|---|---|---|---|
| Yes, watch it | ✅ | `alt+y` | create the watch job; interest → `active` |
| Not now | ⏸ | `alt+n` | interest → `observing`, `cooldown_until = now + 14d` |
| Never | ✖ | `alt+x` | interest → `declined` |

A pending tool approval in the companion session takes precedence for
`alt+y`/`alt+n`; the proposal keys apply only when no approval is pending, and
the status line says which one the keys will answer.

These are handled deterministically by the daemon (`POST
/api/companion/proposals/{id}`), the way approval answers are. The watch job
is created with the proposal's cron and goal, `origin_session_id` = the
companion session, `notify = each`, `checked_in = 1` (no first-run check-in;
the user already chose), and the `companion` profile.

A free-text reply goes to an ordinary model turn in the companion session,
which has two companion tools:

- `interest_update(key, cron?, goal?, state?)`: adjust an **active** watch or
  retire an interest. It can never move an interest to `active`; only the Yes
  button creates a watch. This holds on every channel.
- `companion_feedback(kind)`: `kind` in `{more, less, stop}`, applied to the
  governor; `stop` pauses outreach other than alerts for 7 days.

On Discord the companion session runs under the `remote` profile, which must
allow these two tools; under the terminal it runs `local`.

### 5.4 The `companion` policy profile

Used by watch jobs and research runs.

- allow: `web_*`, `recall_search`, `fs_read`, `fs_list`, `fs_glob`,
  `fs_grep`, `self_note`
- deny: everything else, including `shell_exec`, `fs_write`, `fs_edit`,
  `memory`, `mcp__*`, `schedule_*`, `agent_*`, `skill_install`

`config.Load` applies the baseline deny to it as to every profile. A run that
wants a denied tool says so in its reply; the next reflection may turn that
into a proposal. `self_note(heading, text)` appends one bullet under a
heading of `self.md`, subject to the size cap, ledgered as `self.update`.

`self_note` is registered only when `companion.enabled` is true. In the
default policy it is allowed for the `local` profile and denied for `remote`
(the same reasoning as `memory`: a Discord injection must not plant text that
rides in every later prompt), and it is non-learnable.

### 5.5 Managing it

- CLI: `spore companion status | interests | outreach | pause [duration] |
  resume`, and `spore companion interests undecline <key>`.
- Chat: `/companion` with the same subcommands, in the TUI, web and Discord.
- `status` shows: enabled/paused, channel resolved, today's budget and spend,
  the last reflection time and result, and a warning after three failed
  reflections in a row.

## 6. Failure handling

| failure | behaviour |
|---|---|
| Discord unreachable | deliver to terminal, record `terminal-fallback` |
| Reflection call errors or returns invalid JSON | skip tick, log, trace; nothing sent or written; 3 in a row → warning in status and TUI header |
| Watch job fails | existing job failure path; 3 consecutive failures make it a `followup` subject |
| Daemon down / restart | no catch-up; state is read fresh on the next tick |
| Session deleted | its signals cascade away; counts recomputed; the companion session is recreated on next outreach |
| `self.md` unreadable | prompt renders no section; reflection told the file is unavailable and may not rewrite it this tick |

## 7. Testing

- Heartbeat driven by a fake clock, fake provider and fake channel: gate skip,
  quiet hours (including a wrap past midnight), budget spent, `none`,
  `propose`, `alert`, `research`, `digest`.
- **Trust, built with `config.Load` (never `config.Default()`):** signals from
  job, companion and sub-agent sessions refused; the `companion` profile
  denies `shell_exec`, `fs_write`, `memory`, `schedule_create`;
  `interest_update` cannot make an interest `active`.
- Validation: every malformed reflection field; oversized `self.md`; missing
  headings; bad cron; cron firing more than hourly; bad key; unknown interest.
- Unanswered follow-up: a pending outreach turns `unanswered` at 12 h, never
  `ignored`; reflection output that neither nudges nor drops it leaves it
  `unanswered`; a second nudge for the same outreach is rejected; a nudge
  unanswered for 24 h marks both rows `ignored`; a dropped proposal returns its
  interest to `candidate` with a 7-day cooldown.
- Governor table test: sequences of replied/ignored/stop → expected budget,
  never below the floor nor above `start_budget`.
- Day counting across a local midnight; tests run with a symlinked TMPDIR.
- Session delete recomputes `days_seen`.
- Manual check before each PR merges, against a real Discord DM: plant three
  days of ZS signals, see the proposal with buttons, press Yes, see the watch
  job in the Jobs folder and its first run land in the DM.

## 8. Build order

One spec, three PRs, each usable on its own:

1. **Observe + self.** Tables, `[companion]` config, signals from refinement,
   `self.md` in the prompt with the ledger kind, `self_note`,
   `spore companion interests|status`. Spore starts learning and says
   nothing. With `enabled = false` nothing changes: no signals, no
   `self_note`, no `self.md` section.
2. **Heartbeat + outreach.** The `companion` policy profile (it has no user
   until watch jobs and research runs exist), router site, reflection, governor, channels, the
   companion session, proposals with buttons, watch creation,
   `interest_update`, `companion_feedback`, pause/resume, `/companion`.
3. **Research + digest.** Research runs, findings, the digest slot,
   follow-ups for stale watches and `self.md` threads.

## 9. Out of scope

Recorded in `docs/backlog.md` as enhancements:

- learning from sources outside spore's own chats (browser history, email,
  calendar);
- voice;
- mood or emotion simulation.

## As built — PR 1

- Day counts are derived from `interest_signals.day` on every read; there is
  no recompute step after a session delete.
- `Sweep` runs on every refinement round that asked for signals, even when none were valid; fading and demotion happen then. PR 2's heartbeat will sweep on its own schedule.
- `DaysSeen` counts every sighting ever recorded, with no window, so a retired interest revived by one new sighting can become a candidate at once. PR 2 decides whether proposals need recent evidence.
- `## ` headings inside `self.md` are demoted to `### ` when rendered under `## Your own notes`, so the user's file cannot open a new top-level prompt section.
- `/context` counts `self.md` in its `System` figure; it does not count `soul.md` or `agent.md` (a pre-existing gap, left alone).

## PR 2 decisions (2026-10-10)

Settled with the user before planning PR 2. Where these differ from the
sections above, these win.

- **PR 2 is split.** 2a: the `companion` profile, the router site, the
  heartbeat (reflection with `propose`, `nudge` and `none`, plus `drops` and
  `self_md`), the governor, unanswered follow-up, the companion session,
  proposals with Discord buttons, watch creation, `interest_update`,
  `companion_feedback`, and the CLI (`status`, `outreach`, `pause`, `resume`,
  `answer`, `interests undecline`). 2b: the pinned TUI row with
  `alt+y`/`alt+n`/`alt+x`, the web list, and `/companion` on every chat
  surface. `alert`, `finding`, `followup`, `digest` and `research` stay in
  PR 3.
- **Discord surface (replaces §5.2's DM rule).** A new `[companion.discord]`
  block:

  ```toml
  [companion.discord]
  mode       = "thread"   # thread (default) | dm
  channel_id = ""         # the channel the companion thread opens in; required for thread mode
  user_ids   = []         # who may talk in that thread and press its buttons; empty = bridge.discord.user_ids
  ```

  Thread mode posts one anchor message in `channel_id`, opens a thread named
  `spore` on it, and binds the thread to the companion session; a reply in
  the thread continues that session through the existing bridge path. The
  channel need not be in `bridge.discord.channel_ids`: a thread under it is
  admitted for the users in `companion.discord.user_ids` and nobody else,
  and a top-level message in it is not admitted at all. Every companion user
  must also be in `bridge.discord.user_ids`. DM mode binds the DM channel of
  the first companion user, which needs `bridge.discord.allow_dms` and takes
  over that user's rolling DM session. `channel = "discord"` with an
  incomplete block is a load error; `channel = "auto"` falls back to the
  terminal instead.
- **Watch output stays as specced:** `notify = each`, so every run's reply is
  a note in the companion session and its Discord thread.
- **Recency.** Only signals from the last 30 days (`FadeAfter`) count toward
  `habit_days`, for promotion to `candidate` and for demotion back to
  `observing`. The all-time count is still what `spore companion interests`
  shows.

## As built — PR 2a

- Each outreach is written into the companion session as a
  scheduler-tagged user cue followed by the message as an assistant reply.
  A session may never open on an assistant message (Qwen rejects it), and
  the cue tells the model which outreach the user is answering.
- Delivery holds the companion session's turn slot while it appends. While
  the user is mid-turn there, the tick reports "companion session busy" and
  tries again next tick. That does not count as a failed reflection.
- Reflection is skipped unless something is eligible (a candidate out of
  cooldown, or an unanswered message) and the state's fingerprint changed
  since the last successful reflection: local day, candidate evidence and
  cooldown, unanswered rows and nudges, and the week's outcomes.
- A nudge left pending for 24 h marks itself and its original `ignored`
  and steps the governor down once, not twice.
- `companion_feedback`: `more` +1, `less` −1, `stop` halves and pauses
  outreach for 7 days, all clamped to 1..`start_budget`. It does not set any
  outreach's outcome to `stop`.
- The first heartbeat waits one minute after start, so the Discord bridge
  has connected.
- An active interest whose watch job is cancelled or deleted retires on
  the next sweep.
- Known gap: a proposal the user answers in text but never presses stays
  `proposed`. PR 3's follow-ups are the place to raise it again.

## PR 2b decisions (2026-10-12)

Settled with the user before planning PR 2b. Where these differ from the
sections above, these win.

- **Scope.** The pinned companion row, the proposal bar on the TUI and the
  web, `/companion` on every chat surface, and one 2a leftover: companion
  sends to Discord set empty `allowed_mentions`. The other two 2a leftovers
  (a text-answered proposal stuck in `proposed`, ErrBusy wasting a
  reflection) stay out.
- **Proposals are answered from a bar**, in the slot the approval card uses,
  not from buttons inline in the transcript.
- **`/companion` runs every subcommand except `check`** -- `status`,
  `interests`, `interests undecline <key>`, `outreach`, `pause [dur]`,
  `resume`, `answer <id> yes|not-now|never` -- on every surface, for anyone
  already admitted there. A model cannot run it: slash commands are taken
  from the user's text before a turn starts. `check` stays CLI-only.
- **One formatter.** The subcommands' text is produced by one function in
  `internal/companion` that both the CLI and the daemon call; clients print
  the text they get back.

## Design — PR 2b

### Daemon API

- **Open proposal** means: an outreach of kind `proposal`, outcome `pending`
  or `unanswered`, whose interest is still `proposed` -- the test
  `Service.AnswerProposal` applies, so a listed proposal is always
  answerable. `store.OpenProposals` returns them newest first.
- `GET /api/companion/proposals` → `[{id, text, sent_at, interest_key}]`,
  newest first; 503 when the companion is off.
- `POST /api/companion/command {"args": [...]}` → `{"text": "..."}`. Empty
  args mean `status`. `answer` goes through `Server.AnswerProposal`, so its
  note still lands in the companion session and its Discord thread; the
  rest go through `companion.Run(ctx, w, svc, cfg, args)`, which moves out of
  `cmd/spore/companion.go` with its formatters unchanged. `check` or an
  unknown subcommand is 400 with a usage line; companion off is 503.
- The CLI keeps reading the store directly, so `spore companion status`
  works with the daemon down, and calls the same `Run`.
- Clients learn of changes from what already exists: delivering an outreach
  and answering a proposal both publish an event on the companion session's
  stream as a `job_note`. A client refetches `/proposals` when the
  companion session is opened and on every `job_note` for it -- not on
  every event, which would refetch per streamed token -- so a press on
  Discord clears the bar elsewhere. A `job_note` in the companion session
  also marks it unread unless it is the session on screen.
- `SessionJSON` already carries `source` and `unread`, and
  `POST /api/sessions/{id}/seen` accepts any session: no daemon change for
  the pin or the badge.
- Companion Discord sends -- the thread anchor, outreach, notes in the
  companion thread, and `/companion` replies -- set
  `allowed_mentions: {parse: []}`. The rest of the bridge sets none either;
  that wider gap goes to the backlog, not this PR.

### TUI

- **Pinned row.** The companion session is the first sidebar row, above the
  jobs folder and outside the workspace groups, drawn as `◆ spore`, with an
  unread badge from `Unread`. Opening it marks it seen through the path job
  runs use (`markSeen`), widened to `companion`.
- **Keys.** `alt+y` / `alt+n` / `alt+x` answer the newest open proposal --
  Yes / Not now / Never -- at once, in INSERT and in NORMAL, while the
  companion session is selected and a proposal is open. A pending tool
  approval in that session wins: `alt+y`/`alt+n` answer it as today and
  `alt+x` does nothing. In every other case alt+key keeps today's
  fall-through to NORMAL (where `n` is new session and `x` cancel agent).
  No confirm step: each answer settles one item and each is reversible
  (`interests undecline`, deleting the watch).
- **Proposal bar.** Docked between the transcript and the input, only
  when no approval is pending. It is not overlaid the way the approval
  card is: a proposal can stay open for days, and an overlay would hide
  the conversation it belongs to.

  ```
  ╭ spore proposes ──────────────────────────────────╮
  │ Watch ZS: check the daily close at 9:00 weekdays │
  │ alt+y yes   alt+n not now   alt+x never   (+1)   │
  ╰──────────────────────────────────────────────────╯
  ```

  The text is the outreach text through `visible()` (it is model output),
  clipped to two lines; `(+N)` counts the other open proposals. The header
  and status line show `alt+y/n/x proposal`, or "approval first" while an
  approval is pending. After an answer the posted note appears in the
  transcript and the refetch clears the bar; an error goes to the status
  line.
- `/companion …` and `:companion …` call a new `Backend.Companion(ctx,
  args)` and show the text as `/usage` output is shown. The `?` help gains
  one line.

### Web

- **Pin.** The companion session renders first in the session list, in its
  own `spore` group above the workspaces. `unread` counts `companion` as
  well as `job`; opening the session posts `/seen`.
- **Proposal bar.** In the `#approvals` slot above the composer, while the
  companion session is open, a proposal is open and no approval is pending:
  the text as a text node, Yes / Not now / Never buttons, and "+N more".
  Styles go through classes or `el.style` (the CSP forbids style
  attributes). A click posts `/api/companion/proposals/{id}`; a failure is a
  `msg error` line.
- **`/companion …`** is a prefix match in `slashCommands` (today they match
  whole strings); the returned text renders as a preformatted note. The help
  table gains a row.

### Discord

- In `handle`, beside `/usage` and `/model`: `/companion` or a
  `/companion ` prefix calls `CompanionCommand(ctx, args)` on the bridge's
  daemon interface and replies in the same channel or thread, with empty
  `allowed_mentions`, then settles. Admission has already run, so who may
  use it follows from who may talk there, the companion thread included.

### Plain chat

- `runPlainSlash` gains `/companion …` through the same client call. Plain
  chat has no bar; `answer` is how a proposal is answered there.

### Testing

- store / companion: `OpenProposals` (pending and unanswered listed;
  answered, non-proposal and interest-no-longer-proposed excluded); `Run`
  output per subcommand equals the old CLI's; the CLI tests move onto `Run`.
- daemon: `GET /proposals`; `POST /command` (`answer` posts the note,
  `check` and unknown are 400, companion off is 503); `/seen` on a
  companion session clears `unread`.
- TUI: pinned row order; unread badge and clearing; goldens for the bar
  alone and with "approval first"; `alt+y` answers the proposal without an
  approval and the approval with one; `alt+x` with no open proposal keeps
  today's fall-through.
- Discord (fake client): `/companion status` replies; that reply, outreach
  and the anchor carry empty `allowed_mentions`.
- Web has no JS test suite; the bar and pin are checked in the manual gate
  with Playwright.
- Manual gate: an isolated daemon on :7788 with the real model;
  `spore companion check --promote <key> --force` to get a proposal; answer
  one with `alt+y` in the TUI and one from the web bar; `/companion status`
  on both; a press on one surface clears the other's bar. Discord is checked
  live by the user only -- never a second daemon on the bot token.

## As built — PR 2b

- `Bridge.Deliver` sends every note quiet, not only the companion's: it
  cannot tell which session is the companion's, and a note never needs to
  ping. Button answers (`Respond`) are quiet too.
- The TUI bar is docked above the input and shrinks the transcript by its
  height; the approval card is still drawn over the transcript.
- Clients refetch proposals on open and on `job_note` in the companion
  session; a `job_note` there also marks it unread unless it is on screen.
- The TUI no longer shows a just-opened session as unread when its
  transcript arrives after the mark-seen call.

## PR 3 decisions (2026-10-13)

Settled with the user before planning PR 3. Where these differ from the
sections above, these win.

- **A watch run decides its own alerts.** The run ends its work with a
  verdict, `ALERT:` or `NOTHING:`. A `NOTHING` run posts nothing; an `ALERT`
  run is delivered at once as an `alert` outreach. Reflection does not gate
  watch output, and watch runs no longer post every reply (`notify = each`
  is retired for companion watches).
- **The digest goes out every morning**, even when it is short, in the first
  tick of the local day outside quiet hours. It is outside the daily budget.
- **Research is started by reflection only** (the `research` action, as in
  §4.4): no fixed schedule per interest.
- **Both 2a leftovers are fixed:** a proposal answered in text no longer
  stays `proposed` for ever, and a reflection that meets a busy companion
  session is kept and applied on the next tick instead of being thrown away.
- **Run text is not stored.** Run replies are read from their sessions and
  clipped only when rendered into the reflection input: 1 KiB per watch run,
  6 KiB per research run.

## Design — PR 3

### Watch alerts

- **The verdict instruction is added at run time.** A watch job stores its
  rendered prompt, not its goal, so changing `WatchPrompt` alone would never
  reach the watches that already exist. `StartJob` appends `VerdictSuffix`
  to the prompt of any job that is the `watch_job_id` of an `active`
  interest: begin the reply with `ALERT: <what to tell the user>` or
  `NOTHING: <one line>`. `WatchPrompt` keeps its current text.
- **`afterJobRun` takes a companion branch first.** When the finished job is
  an active interest's watch, the run is handled by the companion -- before
  the no-origin early return and whatever the job's `notify` mode -- and the
  ordinary note path is skipped. New watches are created with
  `notify = none`; existing ones need no migration, because the branch
  ignores `notify`.
- **Parsing.** `companion.ParseVerdict(reply)` reads the first non-empty
  line, after removing a surrounding code fence and leading `*`/`#`/space,
  case-insensitively. `NOTHING:` gives `nothing`; `ALERT:` gives `alert`
  with the rest of the reply as the message. Anything else -- no marker, an
  empty reply -- is `alert` with the whole reply (fail-open: a missed alert
  is worse than a noisy one, and the alert budget caps the noise). A failed
  or stopped run is `failed`.
- **Delivery.** Every finished watch run gets a `companion_runs` row. An
  `alert` row is then delivered through `Deliverer.Deliver` as an `alert`
  outreach (text clipped to 1200 runes, with "· the rest is in the Jobs
  folder"), so it lands in the companion session and its Discord thread and
  can go `unanswered` and be nudged like any outreach. Before sending:
  - **Paused:** held.
  - **Quiet hours:** held unless `alerts_in_quiet_hours`.
  - **Alert budget:** alerts already sent today (local day) are counted;
    at `alert_budget` the row is held.
  - **`ErrBusy`:** held.
  A held alert keeps `verdict = alert` with no `reported_outreach_id`. The
  heartbeat retries held alerts before reflecting, under the same checks
  and with no model call. An alert held when the next digest is written is
  offered to that digest instead, and is never retried after it.
- **A failed run** posts the existing "job N failed" note into the
  companion session (as today). Three failed rows in a row for one watch
  make it a follow-up subject.

### `companion_runs`

| column | meaning |
|---|---|
| `id` | INTEGER PK |
| `interest_id` | FK to `interests`, ON DELETE CASCADE |
| `kind` | `watch` or `research` |
| `job_id` | the job that ran |
| `session_id` | the run's session, or empty while a research job has not started |
| `question` | research only |
| `verdict` | watch: `alert`, `nothing`, `failed`; research: `pending`, `done`, `failed` |
| `started_at`, `finished_at` | |
| `reported_outreach_id` | the outreach that told the user about it, nullable |
| `seen_at` | when a reflection first saw it, nullable |

No reply text is stored. It is read from `session_id` with
`schedule.LastReply` when needed; a deleted session reads as `(run deleted)`.
Rows older than 30 days are swept by the heartbeat.

### Research runs

- **Action.** `{"action":"research","interest":"stock:zs","research":{"question":"…"}}`.
  Valid when the interest is `active`, the question is 1..300 characters,
  no research row is `pending` and younger than 2 hours, and fewer than 6
  research rows started this local day. It sends nothing, so it does not
  count against the daily budget and `message` must be empty. It is chosen
  only on ticks that reflect at all, so pause and quiet hours stop it as
  they stop everything else.
- **Running it.** The heartbeat creates a `once` job due now with
  `Prompt = ResearchPrompt(label, question)`, `Profile = companion`, no
  origin, `notify = none`, `checked_in = 1`, and a `pending` research row
  pointing at it. The scheduler starts it on its next tick; `StartJob`
  records the run session on the row. `ResearchPrompt` says the run is
  unattended and read-only, asks for an answer to the question with its
  sources, and asks it to name any tool it lacked.
- **Finishing.** `afterJobRun` finds the row by `job_id` and sets
  `verdict` to `done` or `failed`, and `finished_at`. A research run never
  alerts; what it found reaches the user only through a `finding` or the
  digest.
- **Stuck runs.** A research row still `pending` after 2 hours is set to
  `failed` by the heartbeat; a late finish does not overwrite it. There is
  no turn timeout in the daemon, so this is what stops one stuck run from
  blocking research.

### What reflection sees

- A new input section, **"Runs since you last looked"**: every finished row
  with no `reported_outreach_id` that finished in the last 7 days, newest
  first, each with its id, kind, interest key, verdict, age and reply --
  watch replies clipped to 1 KiB, research replies to 6 KiB, a clipped
  entry ending `… (clipped; full reply in job run <session>)`. Rows already
  seen are marked `(seen before)`. Held alerts appear here marked
  `(held: not sent)`.
- **Follow-up subjects** are listed in their own section: an active watch
  with 10 or more runs and no reply or reaction to any of its outreach in
  14 days; a watch whose last 3 runs failed; and, in the digest slot,
  "read your Threads with you and raise any that has come due".
- After a successful reflection, every row shown is given `seen_at`.

### New actions

| action | fields | budget | notes |
|---|---|---|---|
| `research` | `interest`, `research.question` | none | see above |
| `finding` | `message`, optional `interest`, `runs` (1+ ids shown in the runs section) | daily | sets `reported_outreach_id` on the cited runs |
| `followup` | `message`, optional `interest` | daily | for a follow-up subject or a `self.md` thread |
| `digest` | `message`, optional `runs`, optional `nudge_of` as a list | none | only in the digest slot, and required there |

`alert` is not a reflection action: alerts come only from watch runs.

### The digest slot

- The slot is due on a tick when the companion is not paused, it is not
  quiet hours, and `companion_state.last_digest_day` is not today's local
  date. The first such tick of the day is the slot, whatever the time, so a
  daemon down all morning still gets one.
- In the slot the input says so, and the only valid action is `digest`.
  The digest may cite `runs` (marking them reported), fold unanswered
  messages through a `nudge_of` list (each listed row counts as nudged), and
  carry `drops` and `self_md` as any reflection may.
- `last_digest_day` is written only after the digest is delivered. A
  failed or invalid reflection, or a busy session, leaves the slot due for
  the next tick.
- When a digest is delivered, the previous digest, if still `pending` or
  `unanswered`, is resolved `ignored` with the reason "superseded by the
  next digest", with no governor step. A digest is never nudged. A reply or
  reaction to a digest still steps the governor up.

### The gate

- `Eligible` is true when any of these holds: a candidate out of cooldown,
  an unanswered outreach, the digest slot is due, a finished run not yet
  seen, a held alert, a follow-up subject, or a saved pending decision.
- `Fingerprint` adds the unseen run ids, the held alert ids, the follow-up
  subjects and the digest-slot flag. An unchanged fingerprint still means no
  model call.

### The 2a leftovers

- **Text-answered proposal.** In `resolveStale`: a `proposal` outreach that
  is `replied`, whose interest is still `proposed` 24 hours after it was
  resolved, sends the interest back to `candidate` with
  `cooldown_until = now + 7d` (the `ReturnCooldown` path).
- **Busy session.** When `apply` meets `ErrBusy`, the validated decision is
  saved as JSON in `companion_state.pending_decision`. On the next tick,
  before any reflection, it is validated again against a fresh view: still
  valid, it is applied with no model call (and the fingerprint recorded as
  usual); invalid, it is discarded and the tick reflects as normal. A
  pending decision older than 12 hours is discarded.

### `spore companion check --research <key>`

The operator check gains `--research <key>`, the research counterpart of
`--promote … --force`: the reflection input gains an "Operator check" line
telling it to answer `research` for that interest, with a question of its
choosing. It fails with `ErrBadCheck` unless the interest is `active`. The
research validation rules (one in flight, 6 a day) still apply. It exists
so the manual gate does not wait for reflection to choose research.

### Status

`spore companion status` (and `/companion status`, through the shared
formatter) gains:

```
digest:    sent 08:02          (or: due, or: paused)
research:  2 of 6 today, 1 running
alerts:    1 of 3 today, 1 held
```

No other surface changes: alerts, findings, follow-ups and digests reach the
companion thread, the pinned TUI row and the web list as any outreach does.

### Failures

| failure | behaviour |
|---|---|
| A watch reply has no marker | delivered as an alert (fail-open) |
| A research run fails | row `failed`; reflection sees it and may ask again within the daily cap |
| A research run is still pending after 2 h | row `failed` by the heartbeat; a late finish is ignored |
| Reflection invalid in the digest slot | tick skipped; the slot stays due; three in a row warn in status as today |
| A run's session is deleted | the row stays; its reply renders `(run deleted)` |
| Discord down during an alert | the existing `terminal-fallback` path |

### Testing

Built with `config.Load`, a fake clock, and a symlinked TMPDIR.

- `ParseVerdict`: `ALERT:`/`NOTHING:` in any case, inside a code fence,
  after markdown emphasis, missing, empty.
- Run-time verdict suffix: added for an active interest's watch, not for an
  ordinary job, not for a research job.
- Alert path: delivered within `alert_budget`; held over it, in quiet hours
  with the flag off, while paused, and on `ErrBusy`; sent in quiet hours
  with the flag on; a held alert retried on the next tick with no model
  call; a held alert offered to the digest and not retried after it; an
  existing `notify = each` watch takes the companion branch and posts no
  ordinary note.
- `check --research`: rejected for a non-active interest; the input
  carries the operator line.
- Research: each validation rule; the created job's profile, origin,
  notify and check-in; the row filled by `afterJobRun`; a 2-hour stuck run
  failed and its late finish ignored.
- Digest: required in the slot and rejected outside it; exempt from the
  budget; none while paused; `last_digest_day` written only after delivery;
  the previous digest superseded with no governor step; a `nudge_of` list.
- Reflection input: watch and research clip sizes and the clip marker;
  `(seen before)` and `(held: not sent)`; unseen runs change the
  fingerprint; `seen_at` set after a successful reflection only.
- `finding` marks its runs reported; `followup` counts against the budget.
- Leftovers: a `replied` proposal returns to `candidate` after 24 h, not
  before; a pending decision applied without a model call, and discarded
  when no longer valid or older than 12 h.
- Manual gate: an isolated daemon on :7788 with the real model. A forced
  watch on a short cron: one `NOTHING` run stays in the Jobs folder, one
  `ALERT` run reaches the companion session. A research run via
  `spore companion check --research <key>` on an active interest. The digest slot
  reached by clearing `last_digest_day`. Discord is checked live by the user
  only -- never a second daemon on the bot token.
