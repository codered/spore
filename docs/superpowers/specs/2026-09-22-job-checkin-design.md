# Job check-in and notifications — design

**Status:** built 2026-09-22 on `sessions-delete-jobs`; see "As built" at the end for where the build
departs from this text.
**Depends on:** branch `sessions-delete-jobs` (jobs folder, `job_output`, job runs tagged with their job).

## Goal

When a scheduled job runs successfully for the **first time**, spore tells the user in the chat that
created the job, and asks how they want to hear about it from now on:

> Job 1 ran fine — "My grandfather died leaving me his stress…". Want each run posted here, only
> failures, or nothing? It's always in the Jobs folder, and you can ask me for its last output any time.

The user's answer sets that job's notification mode, and every later run follows it. "Here" means the
surface the job was created from: the TUI chat, or the Discord thread or DM. It is always and only that
chat. A job created in the TUI never notifies Discord, even when Discord is connected.

## Non-goals

- No check-in for jobs created before this ships: they have no recorded origin.
- No new surfaces: no email, no push. The Jobs folder stays the complete record.
- No check-in for any run after the first. A run that fails before any success never triggers one.

## Data

`jobs` gains three columns, added by a migration in the same style as `migrateSessions`:

| column | type | meaning |
|---|---|---|
| `origin_session_id` | TEXT, default `''` | the session `schedule_create` ran in, or its root if that was a sub-agent. `''` for jobs created over HTTP without `session`, and for all older jobs |
| `notify` | TEXT, default `'ask'` | `ask` (not decided yet), `each`, `failures`, `none` |
| `checked_in` | INTEGER, default `0` | 1 once the first-run check-in has been delivered |

Setting `origin_session_id`: `schedule_create` reads the session from `policy.SessionFrom(ctx)` and
stores its root (via `SessionAncestors`). `POST /api/jobs` accepts an optional `session`.

Deleting the origin session sets `origin_session_id = ''` on its jobs (in `DeleteSessions`'
transaction). The job keeps running, into the Jobs folder only.

## When a run ends

`StartJob` already knows the run's session. Its turn's end (turn_done, error or stopped, taken from
the hub) calls one function, `afterJobRun(job, runSession, outcome)`:

1. **No origin, or origin deleted:** nothing to do.
2. **First success (`!checked_in`, outcome ok):** deliver the check-in (below), then set `checked_in = 1`.
3. **Later runs:** `each` sends a notification that the job ran (see below, no reply text); `failures`
   sends one only when the outcome is not ok; `none` and `ask` send nothing.

## Delivering into the origin chat — the one real decision

The check-in must sit in the origin session's **model context**. Otherwise, when the user answers
"only failures", the model has no idea what they are answering.

**Approach A — a daemon-started turn (recommended).** The daemon appends an *event* message to the
origin session as a user-role message, clearly marked:
`[spore scheduler] Job 1 ("Send me a joke…") ran for the first time at 02:25 UTC and succeeded. Its
reply was: "…". Tell the user it ran, quote the reply briefly, and ask how they want to be notified:
each run, only failures, or not at all (it is always in the Jobs folder; job_output shows the last run).`
It then starts a normal turn. The model writes the message in its own voice. User and assistant
messages keep alternating, so every provider accepts the history. When the user answers, the model
calls a new tool, `schedule_notify {id, mode}` (allow-listed: it only narrows what spore sends).
- *TUI:* works as it is: the turn arrives on the global feed like any other.
- *Discord:* today the bridge renders only turns it started. It needs one addition: turns the daemon
  starts on a session bound to a Discord channel are rendered into that channel, through the same
  renderer.
- *Origin busy:* the event waits until the origin's current turn ends. The hub's End hook starts it,
  so it never interleaves with a running turn.
- *Cost:* one model turn per check-in. That's a single turn per job, since the check-in happens only
  once.

**Approach B — a plain note, no model turn.** Store a non-model "note" and show it in the TUI and on
Discord. It's cheap, but the user's reply lands in a model context that never saw the question.
Rejected for the check-in.

**Approach C — append an assistant message directly.** No model call, but it leaves two assistant
messages in a row, which some providers reject. Rejected.

## Later runs with `each` / `failures` — a plain notification

Decided: after the check-in, a run under `each` (or a failed run under `failures`) produces a short
**notification that the job ran**. It has no reply text and no model turn:

> ⏰ job 1 ran at 02:30 UTC — ok · it's in the Jobs folder
> ⏰ job 1 failed at 02:30 UTC: provider timeout · it's in the Jobs folder

It is delivered in three ways at once:

- **Persisted** in the origin session as a message with a new role, `note`. Transcripts show it, so a
  chat opened later still has it. `agent.Snapshot` skips it, so the model never sees it and the
  user/assistant alternation is untouched. Asked "what did job 1 say?", the model uses `job_output`.
- **Live** through a new wire event, `job_note` (appended to the wire constants), which the TUI renders
  as a notice block in the origin session.
- **On Discord**, when the origin session is bound to a Discord channel: the bridge gains
  `Deliver(sessionID, text)` and posts the note there. It never posts to any other channel.

A note is written even while the origin session is mid-turn: it is not a model message, so it cannot
interleave with the turn's history.

## Tests (to plan in detail after review)

- Store: migration of an older `jobs` table; origin cleared when its session is deleted.
- `schedule_create` records the root of a sub-agent's session as the origin.
- The first successful run starts exactly one check-in turn in the origin; a second success starts
  none; a failed first run starts none.
- A busy origin runs the check-in after its turn ends, never alongside it.
- `schedule_notify` sets the mode; `each` / `failures` / `none` deliver as specified, on the TUI feed
  and to a bound Discord channel (fake client). A TUI-created job's note never reaches Discord.
- `agent.Snapshot` leaves `note` messages out of the model's history; the TUI transcript shows them.
- End to end: a real daemon with a scripted provider, a job created from a chat session, one tick,
  and the check-in turn in that session's transcript.

## Decisions

- **Check-in:** once, after the first successful run, as a model turn in the origin chat (Approach A).
- **Later runs:** a plain notification that the job ran, with no reply text and no model turn.
- **Where:** only the chat that created the job.

## As built

- **Run outcome.** The turn's end is taken inside the daemon's own pump (`startTurnThen`), not from a
  hub subscription. A subscriber can drop events when its buffer is full, and the pump cannot.
  `afterJobRun` runs after the run's slot is released.
- **Check-in claim.** `checked_in` is set when the check-in is *claimed*, before its turn runs. Two
  runs that end together therefore cannot both check in. The cost: a daemon that stops while a
  check-in waits on a busy origin loses that check-in, and the job stays in `ask`.
- **Busy origin.** `Hub.Enqueue` queues the check-in, and `Hub.End` hands the slot straight to it. A
  client post cannot slip in between.
- **Profile.** The check-in turn runs as `local` only when the origin's source is `chat` or `job`.
  Any other origin, Discord included, runs as `remote`.
- **Compaction.** `Compact` pairs stored rows with snapshot messages by position, so it skips `note`
  rows as `Snapshot` does.
- **Clients.** The TUI shows `job_note` events and `note` rows as notices. It shows the scheduler's
  check-in message (prefixed `[spore scheduler]`) as a short notice, never as something the user
  typed. The web UI shows a `note` row under its role label.
- **Policy.** `schedule_notify` is in the default `allow` list. A config with its own `allow` list
  must add it, or the tool asks for approval.
