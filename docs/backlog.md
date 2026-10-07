# Backlog

Wanted, not yet scheduled, and known gaps in what has shipped. Each entry
records what was asked for -- or what was knowingly left undone -- and the open
questions that must be answered before it can be specified, so the next
brainstorm starts where this one stopped rather than from the request.

Nothing here is a commitment to an order. Stages 5a, 5b, 5c, 6 and 7 have all
shipped; the staged plan in section 11 of the design spec is complete, and
everything below is what has been asked for since.

## Chat commands and skills: shipped, with two things left

Closed. All five asked-for commands shipped: `/clear` and `/compact` in plan 7,
and `/skills` with this change. The commands are client-side intercepts in the
Bubble Tea model, with one daemon endpoint each where new server behaviour was
needed (`POST /compact`, `GET /skills`); the open question about a combined
`POST /api/sessions/{id}/commands` dispatch table was answered by the cheaper
path -- plan 7 wired the intercepts, and the web UI never saw them.

The `/skills` open question was answered by building the reading that was
asked for: `SKILL.md` folders under the data directory, loaded on demand
through `skill_load` and the ask-gated `skill_install`. The command lists each
skill with its estimated body size, marks the ones already loaded by scanning
the transcript for `skill_load` calls, and reports the per-file errors `Load`
returned.

Scoping the command found a wiring bug that shipped with the skills
subsystem: `Snapshot.Skills` was never populated, so the "Skills you can
load" index in the system prompt was empty in every production turn. The fix
is in this change -- the agent holds the same `*skill.Caches` the tools were
built with, and `Snapshot` fills the index on every turn.

## Sub-agents: shipped

Closed. `agent_run` waits for a sub-agent's answer, `agent_spawn` starts one
in the background and `agent_result` collects it. Two tools were chosen over
one tool with a flag. `/agents` lists what a session launched. Section 1 of
the design spec no longer lists sub-agents as a non-goal.

The four open questions were answered as follows:

1. Approvals: a child gets the trust profile of the agent that launched it,
   so a Discord-launched child stays on `remote`. Its asks go to the human at
   the root of the chain through an ancestor walk that only goes up, so a
   child can answer neither its own approval nor a sibling's. Remembered
   "this session" answers are looked up at the root, so one answer covers the
   whole tree.
2. A sub-agent is a session with a `parent_id`. `ListSessions` hides children
   unless asked, and `spore session list --all` shows them.
3. Budget: `[subagents]` sets the maximum depth, the maximum cost of the
   whole tree and the maximum number of children running at once under one
   root.
4. Seeing them: `/agents` and `GET /api/sessions/{id}/agents` poll rather
   than stream, so a child's output stays off the parent's stream.

## Deleting a fact leaves its vector behind

Closed. The mirror now carries deletions as well as insertions. A removal from
`recall_fts` writes a row to `recall_tombstones` in the same transaction, and
the mirror drains that feed under a second cursor, `recall_sync.del_cursor`,
after each insert pass. `recall.Recall` gained `Delete`, so every backend
answers for deletion rather than one of them silently lacking it. Design:
`docs/superpowers/specs/2026-09-19-spore-recall-delete-path-design.md`.

The three open questions are answered:

1. **A tombstone, not a diff.** The tombstone is exact and costs one table plus
   a seven-day sweep; the diff needed no schema but cost a full scan of both
   sides and left a stale window measured in minutes. The feed is general over
   `(kind, ref_id)`: fact deletion is its only producer today, but the
   `messages` and `summaries` delete triggers write tombstones too, so message
   and summary deletion work on the day something deletes one.
2. **Deletion may fail, and is retried rather than lost.** A failed delete
   stops the pass with the cursor where it was, so the next tick retries the
   same tombstone; the ones behind it wait. The error is returned rather than
   swallowed, so `Run` reports the mirror as behind instead of logging that it
   caught up. This is the head-of-line blocking that insert batches already
   have.
3. **`recall reindex` stays the escape hatch**, and now also clears the
   tombstone table and zeroes `del_cursor` -- deletes queued against a dropped
   collection mean nothing. It is not worth running on a schedule: the sweep
   discards a tombstone only after seven days, which is far longer than a
   sidecar is plausibly down.

The one case worth remembering: a fact deleted and written again under the same
name occupies the same object id, because `objectID` hashes kind and ref id
alone. Before applying a tombstone the mirror asks whether `recall_fts` holds a
row for that key, and skips it if so. Without that guard the delete path
destroys live vectors, which is worse than the bug it fixes.

Proved end to end against a real Weaviate by `TestDeletedFactLeavesNoVector`
in the `weaviate`-tagged suite: index a fact, find it by semantic search,
delete it, run one mirror pass, and it is gone.

## The container tests: both suites have now run

Closed, and kept here because the next person to ask "has this ever actually
run?" deserves the answer rather than the question. Plan 5b added
`internal/recall/weaviate/integration_test.go` and the `make test-weaviate`
target; plan 5c added `internal/trace/phoenix/integration_test.go` and
`make test-phoenix`. Both have now been run against real servers on the
development machine, which does have Docker.

`make test-phoenix` passed against a real Phoenix collector: the exporter spore
builds is accepted on the default endpoint, and the collector logged the trace
export it received. `make test-weaviate` passed against a real Weaviate and its
model2vec sidecar, including `TestLiveRoundTrip` -- the compose file starting,
readiness detection, the collection created as mapped, and a full index and
search round trip through a real embedding container. Neither run left a
container or a volume behind.

The default suite is unaffected: both suites are guarded by a build tag
(`-tags phoenix` or `-tags weaviate`) and a `docker` lookup, so they skip
rather than fail where the containers are absent, and `make test` compiles
neither.

**Open questions**

1. Should either suite run in CI, and if so, is it a gate or advisory? Pinned
   images and a network make them the first tests here that can fail for
   reasons unrelated to the change. A local run proves the path works; it does
   not stop it rotting.
2. Nothing else. The honesty question these entries used to raise -- whether
   `spore recall setup` and `spore trace setup` were shippable while untested
   -- is answered: both provisioning paths have been exercised end to end.

## MCP path arguments are not checked against the session workspace

Closed. The design spec (section 6) promised that MCP tool path arguments are
evaluated against the calling session's workspace; `baselineDeny` bounded
`fs_*` only, and `mcp__*` appeared solely in the editable default `ask` list.
An MCP call naming a path outside the session's workspace resolved to `ask`,
and a human who approved it ran it with nothing in the policy engine bounding
it. `baselineDeny` now carries `mcp__*(any path outside workspace)`, which no
approval, learned rule or profile can override. Design:
`docs/superpowers/specs/2026-09-15-spore-mcp-path-containment-design.md`.

The three open questions are answered:

1. **Baseline deny, not an editable default.** Section 6 already promised a
   hard bound, and an editable rule protects only operators who never edit it.
2. **Paths are found by name and by shape.** Values under path-like key names
   at any depth, plus any string at any depth that looks like an absolute
   path, a `~` path or a `file://` URI. Name-only detection misses servers
   with unusual argument names; schema-based detection would trust a schema
   written by the server being restricted, the same reason `readOnlyHint` is
   ignored. Relative paths are judged where the server opens them, which is
   the ceiling, not the session root.
3. **Yes, an opt-out, per server.** `local_paths = false` on `[[mcp.server]]`,
   set by the operator who declared the server, for one whose paths are remote
   — repository paths, object keys. Not per tool: tool names change between
   server versions and a per-tool list would silently stop matching.

## The skill cache evicts idle directories

Closed. `skill.Caches` swept nothing, so under `skills.scope = "workspace"` it
held one entry per distinct session root the daemon had ever served. It now
sweeps entries idle beyond an hour on every miss, the same rule
`internal/workspace/describers.go` uses, which bounds the map by live use
instead of by session history.

The open question about the right idle window is answered by use rather than by
argument: a skills directory is read once per turn, so an entry untouched for an
hour belongs to a session that is over. The other question — whether anything
else is keyed by session root and unbounded — was checked at the same time, and
these two were the only ones.

## Refinement's two weak tests: fixed

Closed. Both tests from #48 now fail when the behaviour they name is removed;
each was checked by mutating the code and watching it go red.

1. The job case in `TestIdleSessionsEligibility` no longer records an attempt
   or a watermark, so it is shaped exactly like the fresh chat session and
   only the `SourceJob` exclusion keeps it out of `IdleSessions`.
2. `Refiner` reaches the recall index through an unexported `factIndexer`,
   which `New` sets to the store. `TestIndexErrorAfterFactWriteStillApplies`
   swaps in one that always fails and runs a `fact.create` and a
   `fact.delete` round: both rows stay `applied`, both files change, and each
   index method is called once.

## TUI: catch up with the web UI refresh

Built on `tui-web-parity`. The TUI now does the three things #49 gave the web
UI, with no daemon or wire changes:

1. **Thinking row.** While the selected session's turn runs, a line pinned
   above the input reads `● spore is thinking…` (`writing…` while text
   streams, `running <tool>…` while a call has no result), with the elapsed
   time once `turn_started` lands. It hides while an approval waits. It is
   pinned rather than in the transcript so it stays visible when scrolled up.
2. **Approval profile and deadline.** The prompt shows `profile <p>` beside
   the rule and `auto-denies in m:ss`, or `waiting (no timeout)` when
   `expires_at` is empty. The confirm modal does not repeat the countdown:
   `resolve` already reports an approval that timed out while it was open.
3. **Rename keeps its place.** A `session` event bumps `UpdatedAt` only for a
   session the cache did not know (or knew without a timestamp), and an empty
   field in the event no longer clears a known one.

A one-second tick drives items 1 and 2. It runs only while the selected
session is working or blocked, with at most one tick in flight. Key parity
needed nothing: `o`/`O` and `b` already exist in the TUI.

Followed by the canvas match on the same branch
(`docs/superpowers/specs/2026-09-29-tui-canvas-match-design.md`). It moves
the working line into the transcript with a spinner, draws the approval as
a centred card over a dimmed transcript, and brings the header, sidebar,
tool rows, placeholder and status bar to the canvas's chat and approval
boards. Still missing, because they need daemon data: live token and cache
figures on the working line, the rule's config location, and nested
`spore.*` calls inside `go_run`.

## Transcript control characters: shipped

Closed. #51 (04c07e7) runs every transcript field through `visible`, the
pass the approval card got in #50, so a carriage return or an escape
sequence shows as a symbol instead of repainting rows it does not own. The
gap was wider than tool rows: user, notice, error and footer rows, and
assistant prose, which can echo a tool's output, printed daemon text raw
too. `visibleLines` does the same for multi-line text and keeps newlines.

## Spinner frame cost: shipped

Closed. #51 (04c07e7) cut a spinner frame on 2,000 blocks from 2.8 ms and
1 MB to 61 us and 6 KB (`BenchmarkSyncLongTranscript`). #50 had put the
cost down to rejoining the transcript; a profile put 63% in
`viewport.SetContent` measuring every line and 17% in the join. The
viewport now ends in a blank row that `mainView` draws the working line
over, so its content changes only when the transcript does, and the joined
transcript is kept between frames. Anything that changes every frame
belongs over a row like that, not in the viewport's content.

The manual check -- watching a long session's spinner turn and stay on its
row while scrolling -- was not confirmed before merge.

## Web UI refresh: shipped

Closed. #49 (d15a2e1) brought `web/` to the TUI's colours and layout, and
added the skills, agents, jobs, usage and refinements views, plus
single-key shortcuts with a per-browser off switch. It also named sessions
from their first message on the `title` router site, and fixed
`SessionUsage`, which had reported the global total for every session. Design:
`docs/superpowers/specs/2026-09-28-web-ui-refresh-design.md`.

The manual gate in that spec (§7) was covered by a headless browser run
against a scripted daemon, not by hand. Policy, MCP and memory views are
still absent from the web UI; their endpoints shipped with TUI 2a-2 (`/api/policy`, `/api/mcp`, `/api/memory`), so they are a web-only follow-up.

## TUI views 2a-2: shipped

MCP (`C`), policy (`P`) and memory (`M`) views, with reconnect, revoke and
delete-fact. Design: `docs/superpowers/specs/2026-09-29-tui-views-2a2-design.md`.

It also fixed a bug that predates it: a rule learned with `p` was written to
`config.toml` but did not apply until the daemon restarted. The guard now
holds its engine behind an atomic pointer, and a `policy.Reloader` rewrites
the managed block, re-reads only that block and swaps in a rebuilt engine.
Hand edits elsewhere in `config.toml` still need a restart, on purpose.

Operator actions are audited as `operator action` log lines with
`actor=<X-Spore-Client>`, not approval rows: `approvals.session_id` must name
a session, and a view has none.

## A pattern answer cannot outrank a hand-written ask: fixed

Closed. Implemented in the policy-precedence-proposals spec
(`docs/superpowers/specs/2026-10-06-policy-precedence-proposals-design.md`).

The open questions are answered as follows:

1. A learned allow with a path condition decides calls inside that path, even
   when a bare tool-name ask covers the same tool. Precedence is tiered: tier 1,
   a rule with an argument condition; tier 2, a bare exact tool name; tier 3, a
   bare tool glob. Within a tier, ask wins a tie. Learned and hand-written rules
   are not distinguished by source.
2. `p` is offered only when the proposed rule would decide the call (never on the
   remote profile). When the call's deciding rule is a hand-written ask that
   would outrank a learned allow in a broader tier, the pattern is not offered.
   `p` now proposes a refinement for review in the Refinements view rather than
   writing the config directly.

## A learned pattern keeps the path form the model used: fixed

Closed. `PatternFor` now takes the session's workspace and resolves the
path with `Resolve` -- `~` expanded, `..` cleaned, symlinks followed, the
same resolution `path matches` applies to arguments -- so every new learned
rule is an absolute glob. It matches the file however the next call spells
it, and only in the workspace it was learned in: a relative rule had applied
to the same relative path in every workspace. The pattern is computed once,
when the call is suspended, and stored on the `pending_calls` row (new
`pattern` column): a reconnecting client is shown that string and an answer
through `Resolve` learns it, so the rule written is the rule shown even if a
symlink appeared or the session was re-rooted in between. Rows from before
the column have no pattern, and "always" is not offered on them.

The review of this change found a bug old enough to predate it: path globs
were quoted a byte at a time through `string(byte)`, which re-encodes every
non-ASCII byte, so any `path matches` rule naming a non-ASCII directory --
learned or hand-written, allow or deny -- never matched. Fixed with it.

Two narrowings came with it. A file directly in the workspace root gets no
pattern whichever way it is spelled; before, only the relative spelling was
refused and the absolute one learned the whole workspace, which for the
local profile defaults to the home directory. And a directory whose name
holds `*`, `?`, `,`, a quote, a backslash or a newline gets no pattern,
because the rule syntax cannot say it literally.

Rules already learned in relative form are left as they are: they still
match only relative arguments, in any workspace. Revoke them in the `P`
view.

## Operator routes share the daemon's unauthenticated API: fixed

Closed. Implemented in the policy-precedence-proposals spec
(`docs/superpowers/specs/2026-10-06-policy-precedence-proposals-design.md`).

The open questions are answered as follows:

1. The baseline deny includes `**/daemon.token` on the `fs_*` and `shell_exec`
   rules, and a new `shell_exec(matches spore web)` baseline deny, to keep the
   model from reading the token or opening a signed-in browser. This does not
   block reaching the daemon's address, which an interpreter run through an
   approved `shell_exec` could bypass via `python -c`.
2. Every `/api` route requires the token in `~/.spore/daemon.token` (bearer
   header or the `spore web` cookie). A Host check refuses foreign hostnames,
   and an Origin check refuses cross-origin requests, so only holders of the
   daemon token can call operator routes.

Remaining gap: an interpreter run through an approved `shell_exec` can still
read the token, because spore runs as the operator. Closing that needs a
separate OS user and is out of scope.

## LICENSE is a stub: fixed

Closed. #63 (723434c) replaced the 128-byte stub -- the MPL-2.0 name, a wrong
date and a link -- with the verbatim MPL-2.0 text. The release job copies
`LICENSE` into every tarball, so the next tag ships the full text with no
workflow change; `v0.1.0-alpha.1` shipped the stub.

## `/usage` in the TUI, the web UI and Discord: shipped

Closed. #65 (2b794b1). The premise was half right: the TUI already had a
`/usage`, summed from the transcript; the plain chat loop, the web UI and
Discord had none, and sent `/usage` to the model. Every surface now prints
the same report from `GET /api/usage`, formatted by `internal/usage` (Go)
and `usageReport` in `web/app.js`. The 30-day window lives in
`usage.Window`, so the daemon's query and the report's label agree.

The three open questions are answered:

1. **This session plus the 30-day total.** Turns, tokens, cache share and
   cost for the session, then one line with every session's totals.
2. **Inline in the web UI.** `send()` answers `/usage` as a notice row; it is
   not posted and starts no turn. Any other `/text` still goes to the model.
3. **A text intercept in Discord**, beside `/new`. A DM or thread bound to a
   session reports that session and the total; a plain channel has no
   session, so it reports the total alone and opens no thread. Cost follows
   `show_cost`.

The web change was checked in a headless browser against an isolated
daemon. `/usage` in a real Discord server was not tried by hand.

## Skills were not loading: fixed

Closed. #64 (26da049). The skills were installed; they were not loading.
Run against a real `~/.spore/skills`, `skill.Load` rejected 12 of 16. Eleven
wrote their description as a YAML block scalar (`description: >` and
indented lines), and the frontmatter parser read only one-line
`key: value`. The twelfth has no frontmatter at all and still fails, as it
should; `/skills` reports it.

The parser now reads block scalars, indented continuation lines and quoted
values, folds name and description onto one line, and skips keys it does
not use (`license`, `allowed-tools`, `metadata`) with their indented lines
instead of rejecting the skill.

The same check found a second gap: nine of those skills cite files beside
`SKILL.md` (`references/`, templates), and the skills directory is outside
the workspace, so nothing could read them. `skill_load` now lists a skill's
other files after its body and takes an optional `file` argument to read
one, confined to that skill's directory once symlinks are followed, with
dotted paths refused and a 256 KiB cap. A file-only read does not mark the
skill loaded in `/skills`. `skill_install` still writes only `SKILL.md`.

## One invalid UTF-8 string drops a whole batch of traces

Open. Found during the #67 live run. The daemon logged `traces export:
failed to marshal request body in protobuf: string field contains invalid
UTF-8`. `internal/trace` puts tool arguments, tool results, prompts and
completions into span attributes as they are (`StartTool`,
`RecordToolResult`, `EndLLM`). The OTLP exporter refuses a string that is not
valid UTF-8, and spans are exported in batches (`WithBatcher`), so one bad
string fails the whole request, and every span in that batch is lost, not
only the one that carried it.

This time it was an `fs_grep` match inside a binary file. #67 made grep skip
binary files, but other sources remain: `fs_read` with `raw` on a binary
file, `web_fetch` of a non-UTF-8 body, and shell output.

Open questions:

1. **Where to clean.** Once, in the `internal/trace` helpers
   (`strings.ToValidUTF8`), or at the source tools? The helpers are the only
   place that covers every source, including ones not written yet.
2. **Replace or mark.** Replace bad bytes with U+FFFD, or drop the attribute
   and record its byte length and a flag? A replaced binary blob is noise in
   Phoenix either way.
3. **Size.** Attributes are also unbounded, and a 4 MB raw read becomes one
   attribute. Should the same change cap attribute length, as `redact`
   already does for content?

## go_run programs cannot parse Go

Open. Found during the #67 live run. Asked which functions in
`internal/kernel` are longer than 40 lines, the model's first move was a
program importing `go/ast` and `go/parser`. Those packages are not in
`kernel.Allowed`, so it tried shelling out to `go run` (declined) and writing
a helper file (declined), then fell back to counting braces by hand. That
took 7 calls and up to 300 s. One run hit the test cap before answering.

`go/ast`, `go/parser` and `go/token` cannot simply be added to `Allowed`.
`parser.ParseFile` reads the file from disk when `src` is nil, and
`parser.ParseDir` walks a directory, both straight through `os`. A program
could read any file the daemon can, with no `fs_read` call for policy to
judge.

Open questions:

1. **Wrapper or allow-list.** A `spore.ParseGo(src string)` helper (or a
   patched `go/parser` symbol table that exposes `ParseFile` only with a
   non-nil `src` and no `ParseDir`), or a higher-level helper such as
   `spore.GoFuncs(src)` returning name, start and end lines? The wrapper keeps
   the model's normal Go idioms; the helper is smaller and harder to misuse.
2. **Does yaegi run `go/ast` well enough?** `ast.Inspect` takes a callback and
   walks interface values. That needs a probe in the child before the
   design commits to it, the way `min`/`max` were probed for #67.
3. **Wider than Go?** The same gap applies to any structured text a program
   can only read as a string. Is Go source special enough for its own helper,
   given that code questions about this repo are the common case?
