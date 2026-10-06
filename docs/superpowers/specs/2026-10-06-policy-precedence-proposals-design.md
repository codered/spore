# Policy precedence, pattern proposals and daemon route auth

Date: 2026-10-06. Status: approved in conversation, sections 1-5.

Closes two backlog entries: "A pattern answer cannot outrank a hand-written
ask" and "Operator routes share the daemon's unauthenticated API".

## 1. Problem

`p` ("always allow this pattern") writes a learned allow rule. Learned rules
are evaluated after the config's allow and ask lists, and the default config
puts a bare `ask` on every tool `p` can be offered for (`fs_write`, `fs_edit`,
`mcp__*`). So a `p` answer is written and never applies. The operator's own
config has no `[policy]` lists at all: `config.Load` fills them from
`config.Default()`, so the rules shadowing the learned one were never typed
by anyone.

Fixing the order alone would make `p` a one-key, permanent loosening of
policy from inside a session's ask window -- the lever a tired human or a
prompt injection reaches for. And the obvious place to move that decision,
the refinement review, sits behind an unauthenticated HTTP route the model
can `curl` once `shell_exec` is approved.

## 2. Goals and non-goals

Goals:

- A learned allow with a path condition decides calls inside that path, even
  when a bare tool-name ask covers the same tool.
- A narrow ask the operator wrote (`fs_read(path matches **/secrets/**)`)
  is never overridden by a broader allow, learned or written.
- Rule position in the file does not affect any decision.
- No policy change is made from inside a session's ask window. `p` proposes;
  the operator approves in review.
- The review step, and every other daemon route, cannot be called by a
  process that does not hold the daemon token.

Non-goals:

- An operation model (read / write / delete). Deleting happens through
  `shell_exec`, which path rules cannot bound; a follow-up may add operation
  labels over the `fs_*` tools.
- Making the token unreadable to a model that has `shell_exec`. spore runs as
  the operator's user; `python -c 'open(...)'` reads any file that user can.
  Closing that needs a separate OS user and is out of scope. The baseline
  deny stops `fs_read` and `cat`; that limit is documented, not hidden.
- Moving existing learned rules into review on upgrade.

## 3. Precedence

`Engine.Evaluate` decides one call as follows:

1. **Any matching deny wins.** Unchanged: config deny, profile deny, learned
   deny and the baseline deny, in any order.
2. **Otherwise the matching allow and ask rules are grouped by tier, and the
   most specific non-empty tier decides:**
   - **Tier 1 (narrowest):** rules with an argument predicate (`path
     matches`, `word matches`, `matches`, `path outside workspace`,
     `any path outside workspace`).
   - **Tier 2:** rules with no predicate whose tool glob has no `*`
     (`fs_write`, `mcp__time__now`).
   - **Tier 3 (broadest):** rules with no predicate whose tool glob has a
     `*` (`mcp__*`, `web_*`).
3. **Within the deciding tier, ask wins if any ask rule matched; otherwise
   allow.** The `Result.Rule` named is the first ask rule matched in that
   tier, or else the first allow rule, in list order -- the order only picks
   which rule is named, never the decision.
4. **No match:** the profile fallback, with `go_run`'s existing allow
   fallback unchanged.

Source (`config` or `learned`) plays no part. Learned allow and ask rules are
still built only into the base ruleset, never a profile's, so a rule learned
at the terminal never reaches `remote`.

Why tiers, not path depth: depth cannot be compared across globs written
relative, absolute or starting with `**`. A depth ranking would put a learned
`allow fs_read(path matches /home/u/repo/**)` (four literal segments) above
`ask fs_read(path matches **/secrets/**)` (none) and make the secrets folder
readable. Tiers with ask winning ties fail safe.

The cost: an allow cannot be carved inside an ask that has its own
predicate. `ask fs_write(path matches repo/**)` plus `allow fs_write(path
matches repo/build/**)` decides ask. The intended shape is a broad grant as a
bare rule or the fallback, with narrow rules inside it.

Behaviour change for existing configs: when an allow and an ask in one tier
both match, today the allow wins (allow rules are listed first); after this,
ask wins. That only tightens. The operator's current config has no such pair.

`Engine.Rules()` (the `P` view) keeps listing rules per profile, deny first,
then allow and ask grouped by tier (1, 2, 3), then the default -- the order a
reader needs to predict a decision.

## 4. `p` proposes a rule

### 4.1 In the ask window

`p` still answers the call, once, with the decision chosen. It no longer
writes `config.toml`. The guard's `learn` hook is removed (`NewGuard` loses
its last parameter, and `SetLearn` goes); the guard already holds the store
and adds one refinement row itself:

| column      | value                                                        |
|-------------|--------------------------------------------------------------|
| `round_id`  | `approval-<pendingID>`                                       |
| `session_id`| the session the call belongs to                              |
| `trigger`   | `approval` (new `refine.TriggerApproval`)                    |
| `kind`      | `policy.allow` or `policy.deny` (new `refine.KindPolicyAllow`, `refine.KindPolicyDeny`) |
| `target`    | the rule text (what the refinements view's TARGET column shows) |
| `before`    | NULL                                                         |
| `after`     | the rule text                                                |
| `rationale` | `<tool> on <path> in session <id>`                           |
| `status`    | `proposed`                                                   |

A proposal for a rule that already has a `proposed` row of the same kind adds
nothing. Both the `Run` path and the out-of-band `Resolve` path propose; both
keep the existing invariant that a failed write is recorded on the span and
never changes the answer.

Proposed denies go to review too. One rule -- `p` never edits policy -- is
easier to trust than one that depends on the direction answered. The call is
denied either way, and `s` covers repeats this session.

The audit row's scope stays `pattern`, now meaning "answered once and
proposed". Clients that still send `scope: pattern` get the new behaviour
from the server.

### 4.2 When `p` is offered

`PatternFor` still derives the candidate. The guard then checks that the
candidate would decide this call: it evaluates the call against the session's
ruleset with an allow rule for the candidate added, and offers the pattern
only if the result is allow. Otherwise the pending row stores an empty
pattern, which every client already reads as "do not offer `p`", and a `p`
answer on it is recorded as once (existing behaviour).

This covers a narrow ask the operator wrote, another rule in the same tier,
and the `remote` profile, where learned rules never apply -- so a Discord
user cannot queue rules for the operator's local policy.

`Engine` gains `WouldAllow(s Session, c Call, rule string) bool` for the
check, which parses the rule, adds it to a copy of the session's ruleset as a
learned allow, and evaluates.

### 4.3 Labels

Every approval surface changes its `p` text from "always" to say it
proposes: TUI, plain chat loop, web UI, Discord. Wording: `allow once +
propose <pattern>`; the web UI's fine print says the rule goes to the
refinements view for review.

## 5. Review and apply

- `Refiner` gains `ApplyPolicy func(decision, rule string) error`, set in
  `cmd/spore/wire.go` to the reloader's existing write-and-reload path
  (`Reloader.Learn`, which writes the managed block through
  `config.LearnRule` and swaps a rebuilt engine). `refine` takes a string
  decision so it does not import `policy`.
- `Refiner.Accept` handles `policy.*` rows in their own branch before the
  file-content checks: it claims the row (`proposed` → `applied` through
  `SetRefinementStatus`, so two accepts cannot both apply), then calls
  `ApplyPolicy(decision, *row.After)`. `config.LearnRule` already deduplicates, so a rule
  already in the block is applied without a second line. A failed write marks
  the row `failed` and returns the error. A nil `ApplyPolicy` is an error,
  not a silent no-op.
- `Refiner.Reject` is unchanged.
- `Refiner.Rollback` skips `policy.*` rows; the `P` view's revoke removes a
  rule. `Store.LatestAppliedRound` ignores `policy.*` rows, so `/refine
  rollback` never picks an approval round.
- `pathFor` is not reached for `policy.*` rows.
- The TUI refinements view and the web refinements view render a `policy.*`
  row as decision, rule and rationale instead of a file diff.

## 6. Daemon route authentication

### 6.1 The token

On start the daemon reads `<data dir>/daemon.token`, creating it if absent:
32 bytes from `crypto/rand`, hex encoded, written with mode 0600. It persists
across restarts so a browser tab survives one; deleting the file rotates it.
The file is created before the listener opens, so a client that started the
daemon can read it as soon as `/healthz` answers.

### 6.2 What it guards

Every `/api/*` route, reads included: reads expose transcripts and memory,
and one rule ("all of `/api`") is easier to keep true than an allowlist.
`/healthz`, `/` and `/static/*` stay open. Routes are registered through one
helper that wraps the check, so a route cannot be added without it.

A request is authorised by either `Authorization: Bearer <token>` or the
`spore_token` cookie, compared in constant time. Otherwise 401 with a JSON
error naming `spore web`.

This also closes `web_fetch` reading the API: it has no loopback guard and
`web_*` is allowed by default.

### 6.3 Host check

A request whose `Host` names any host other than `localhost`, `127.0.0.1`,
`::1` or the configured daemon address's host is refused with 403, on every
route. The port is not compared: rebinding works through a hostname the
attacker controls, so the hostname is what must be checked.

`SameSite=Strict` does not cover a page on another port of the same host:
SameSite is per site, and `127.0.0.1:3000` and `127.0.0.1:7777` are one site,
so a page there gets the cookie and can send a simple POST without a
preflight. A model with `shell_exec` could start such a page and open it in
the operator's browser. So every `/api` request whose `Origin` header is
present and is not the daemon's own origin (`http://` + `Host`) is refused
with 403. Browsers always send `Origin` on cross-origin requests; the CLI
sends none.
This stops DNS rebinding.

### 6.4 Clients

- **CLI and TUI:** `newClient` in `cmd/spore/client.go` reads the token file
  from the data directory and sends the bearer header on every request,
  including the event stream. A missing file sends nothing (the daemon then
  answers 401, which the client reports as "daemon token missing").
- **Web UI:** a new `spore web` command opens `http://<addr>/?token=<token>`
  in the browser. It prints the URL only when stdout is a terminal, so a
  model running it through `shell_exec` is not handed the token. `GET /` with a valid `token`
  query sets `spore_token` (`HttpOnly; SameSite=Strict; Path=/`) and
  redirects to `/`, dropping the token from the address bar. `GET /` without
  a valid cookie serves a short page saying to run `spore web`. `app.js`
  needs no header: the cookie rides every `fetch` and the `EventSource`. A
  401 from `api()` shows the same instruction.
- **Discord bridge:** in-process; unchanged.

### 6.5 Keeping the model away from the token

`**/daemon.token` is added to the baseline deny's credential lists: the
`fs_*` `path matches` list and the `shell_exec` `word matches` list, and a
new baseline rule `shell_exec(matches spore web)` keeps the model from
running the command that opens a signed-in browser. That stops `fs_read`
and `cat`. It does not stop an interpreter run through
`shell_exec`; see section 2.

## 7. Migration

- Existing learned rules stay. The operator's `fs_write(path matches
  uuid-server/**)` becomes live on upgrade, which is what pressing `p` asked
  for. In general an upgrade activates learned rules that were shadowed; the
  release note says so and points at the `P` view.
- Same-tier allow/ask overlaps now decide ask (section 3); release note.
- No schema change: `kind` and `trigger` are text.
- Old browser tabs get 401 and the `spore web` instruction.
- Docs: close both backlog entries; README gains `spore web` (replacing "open
  http://127.0.0.1:7777") and the bearer header on the jobs `curl` example;
  `assets/demo/record.sh` probes `/healthz`; section 6 of the main design
  spec describes the new precedence and the proposal flow.

## 8. Testing

Engine:

- Table: deny beats every tier; a tier-1 learned allow beats a tier-2
  config ask (the original bug); a tier-1 `**/secrets/**` ask beats a tier-1
  absolute repo allow; tier 2 beats tier 3 (`allow mcp__time__now` vs `ask
  mcp__*`); a same-tier allow and ask decide ask; no match uses the
  fallback; `go_run` keeps its allow fallback; `remote` never sees a learned
  allow.
- Order independence: shuffle the allow, ask and learned lists (fixed seeds,
  many permutations) and assert every decision in a call corpus is unchanged.
- `WouldAllow`: true for the original bug's case, false under a narrow
  written ask, false on `remote`.

Guard:

- A `p` answer runs the call once, adds one `proposed` refinement row with
  the stored pattern, and leaves `config.toml` byte-identical.
- A second `p` for the same rule adds no row.
- `p` is not offered (pending pattern empty) when `WouldAllow` is false.
- A `pattern` answer on a call with no pattern is recorded as once.
- `Resolve` with `pattern` proposes too.

Refine:

- Accepting a `policy.allow` row writes the block and the next `Evaluate`
  allows without a restart; accepting one already present is applied with no
  second line; a nil `ApplyPolicy` fails the accept.
- Reject leaves the config untouched; Rollback skips policy rows;
  `LatestAppliedRound` ignores them.

Auth:

- Walk every registered `/api` route: 401 without credentials, not 401 with
  the bearer header, not 401 with the cookie.
- `/healthz`, `/`, `/static/*` open; `/?token=<good>` sets the cookie and
  redirects; `/?token=<bad>` sets nothing.
- Foreign `Host` gets 403.
- Token file is created 0600, reused on restart, recreated when deleted.
- The baseline deny refuses `fs_read` of `~/.spore/daemon.token` and
  `shell_exec` of `cat ~/.spore/daemon.token`.

Mutation check: for the precedence table, the no-write-on-`p` test and the
route walk, remove the behaviour and confirm the test fails.

Manual, on the operator's config: `spore policy check` shows `fs_write` into
`uuid-server/` as allow; TUI `p` → proposal in the refinements view → accept
→ the next call runs without asking; `spore web` opens and works.

Gate: vet, fmtcheck, lint, vulncheck, tidy, test.
