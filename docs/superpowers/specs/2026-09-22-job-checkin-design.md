# Job check-in and notifications — design

**Status:** draft for review. Nothing here is built.
**Depends on:** branch `sessions-delete-jobs` (jobs folder, `job_output`, job runs tagged with their job).

## Goal

When a scheduled job runs successfully for the **first time**, spore tells the user in the chat that
created the job, and asks how they want to hear about it from now on:

> Job 1 ran fine — "My grandfather died leaving me his stress…". Want each run posted here, only
> failures, or nothing? It's always in the Jobs folder, and you can ask me for its last output any time.

The user's answer sets that job's notification mode, and every later run follows it. "Here" means the
surface the job was created from: the TUI chat, or the Discord thread or DM.

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
3. **Later runs:** `each` delivers the run's reply; `failures` delivers only when the outcome is not ok;
   `none` and `ask` deliver nothing.

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

## Later runs with `each` / `failures` — open question

A model turn per run (Approach A again) keeps the chat's model aware of every run, but costs one turn
per run. With `*/5 * * * *` that's 288 turns a day. A plain note (Approach B) costs nothing, but the
model won't know about the runs unless asked, and `job_output` covers that case.
**Recommendation:** notes for later runs. That needs a model-invisible `note` role (shown in
transcripts, skipped by `agent.Snapshot`) and a `Deliver` hook on the bridge for bound channels.

## Tests (to plan in detail after review)

- Store: migration of an older `jobs` table; origin cleared when its session is deleted.
- `schedule_create` records the root of a sub-agent's session as the origin.
- The first successful run starts exactly one check-in turn in the origin; a second success starts
  none; a failed first run starts none.
- A busy origin runs the check-in after its turn ends, never alongside it.
- `schedule_notify` sets the mode; `each` / `failures` / `none` deliver as specified, on the TUI feed
  and to a bound Discord channel (fake client).
- End to end: a real daemon with a scripted provider, a job created from a chat session, one tick,
  and the check-in turn in that session's transcript.

## Questions for review

1. Later runs under `each` / `failures`: a plain note (recommended) or a model turn each time?
2. A job created from the TUI has no Discord thread. If Discord is connected, should its
   notifications *also* go to your Discord DM, or strictly to where the job was created
   (recommended: strictly the origin)?
