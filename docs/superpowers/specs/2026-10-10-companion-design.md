# spore — companion: initiative, habits and a self

**Date:** 2026-10-10
**Status:** approved (brainstorming dialogue); written spec awaiting review
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
| `first_seen` | TEXT | RFC 3339 |
| `last_seen` | TEXT | RFC 3339 |
| `days_seen` | INTEGER | distinct local days with at least one signal |
| `watch_job_id` | INTEGER NULL | the scheduler job, once a watch is approved |
| `cooldown_until` | TEXT | set by "Not now"; no proposal before this time |
| `declined_at` | TEXT | set by "Never" |

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
`sessions`, ON DELETE CASCADE), `seen_at`, `kind` (`asked`, `mentioned`,
`acted`). This is the evidence a proposal cites ("you asked about ZS on Oct 3,
6, 8 and 10"). `days_seen`, `first_seen` and `last_seen` are derived from
signals and recomputed after a session delete.

### 2.3 `outreach`

Every message spore starts on its own: `id`, `interest_id` (nullable),
`kind` (`proposal`, `alert`, `finding`, `followup`, `digest`), `channel`
(`discord`, `terminal`, `terminal-fallback`), `text`, `sent_at`, `outcome`
(`pending`, `replied`, `reacted`, `ignored`, `stop`), `resolved_at`. Only the
governor and the reflection input read it.

An outreach is `ignored` when 12 hours pass with no reply, reaction or button
press in the companion session; `replied` when the user sends a message there
first; `reacted` for a Discord reaction or a button press; `stop` when the
user's reply asks spore to stop or be quieter (decided by the companion turn
through the `companion_feedback` tool, §5.3).

### 2.4 `companion_state`

A key-value table (`key` TEXT PK, `value` TEXT, `updated_at`) for the
companion's singletons: `session_id` (the companion session, §5.1),
`daily_budget` (§4.6), `paused_until`, `last_reflection_at`,
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
quiet_hours = "22:00-08:00"   # local time, from location/timezone config
habit_days = 3
heartbeat = "30m"
self_max_bytes = 10240
start_budget = 4              # unprompted messages per day at the start
alert_budget = 3              # alerts per day, separate from start_budget
alerts_in_quiet_hours = false
```

`enabled` defaults to `false` so an upgrade does not start messaging anyone.
Validation: `channel` from the closed set; `quiet_hours` parses as `HH:MM-HH:MM`
(may wrap midnight); `habit_days` 1..30; `heartbeat` >= 5m;
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

`days_seen` counts distinct calendar days in the user's configured timezone.
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
2. **Resolve stale outreach:** mark `pending` rows older than 12 h `ignored`.
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
| `action` | one of `propose`, `alert`, `finding`, `followup`, `digest`, `research`, `none` |
| `interest` | required for `propose`, `alert`, `research`; must exist; `propose` requires state `candidate` |
| `message` | required unless `none`/`research`; 1..1200 characters |
| `watch` | required for `propose`; `cron` parses with the scheduler's parser and fires at most hourly; `goal` 1..500 chars |
| `research` | for `research`: `{"question": "..."}`, 1..300 chars; interest must be `active` |
| `self_md` | optional full replacement; must contain the four headings and fit `self_max_bytes` |

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

### 4.6 The governor (cadence 3 → 2)

State: `daily_budget` (float, persisted in a `companion_state` key-value
table), starting at `start_budget`. On each outreach resolution:

| outcome | effect |
|---|---|
| `replied` / `reacted` | `+0.25`, capped at `start_budget` |
| `ignored` | `-0.5` |
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
   `self.md` in the prompt with the ledger kind, `self_note`, the `companion`
   profile, `spore companion interests|status`. Spore starts learning and says
   nothing.
2. **Heartbeat + outreach.** Router site, reflection, governor, channels, the
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
