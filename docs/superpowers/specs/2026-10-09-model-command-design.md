# `/model`: choose a model per operation

Date: 2026-10-09. Status: approved in conversation; staged in three PRs.

## Goal

Let the operator see and change which model each operation uses, from every
surface, without editing `config.toml` and without typing a model ref. Every
choice is picked from a list of models the configured providers actually
serve.

## Decisions (from the brainstorm)

1. Operations are the router's call sites: `chat`, `compaction`, `title`,
   `classify`, `refinement`, `subagent`.
2. `chat` and `subagent` are chosen **per session**. `compaction`, `title`,
   `classify`, `refinement` are **daemon-wide**, stored in a spore-managed
   block of `config.toml`, and take effect without a restart.
3. A sub-agent uses, in order: the `model` argument of `agent_run` /
   `agent_spawn`; else the parent session's `subagent_model`; else the
   parent's effective chat model. The `[[route]] when = "subagent"` rule no
   longer selects anything.
4. Only models in the live catalog can be chosen. No path accepts a free-text
   ref.
5. Surfaces: a tabbed modal in the TUI and the web UI, select menus in
   Discord, and a numbered text form in plain chat.
6. Display: the selected model is marked `->`, the default `*`.

## 1. Resolution

| Op | Selected (`->`) | Default (`*`) | Stored |
|---|---|---|---|
| chat | session `chat_model` | `Router.Model("chat")` | session row |
| compaction, title, classify, refinement | managed override | hand-written route, else `default_model` | managed block |
| subagent | session `subagent_model` | the session's effective chat model | session row |

- "Effective chat model" of a session = `chat_model` if set, else
  `Router.Model("chat")`.
- Choosing the default entry clears the override (stores `""`, or removes the
  key from the managed block). Selected therefore equals default whenever no
  override exists.
- A child session is created with `chat_model` set to the model resolved by
  decision 3. Its turns use that model through the normal session lookup and
  still run on the `subagent` call site, so `/usage` and tracing attribute
  them as before. Its own children inherit from it by the same rule.
- Router: the existing `*router.Router` keeps its pointer identity (title,
  refine, agent, compaction all hold it). It gains an `RWMutex`, a set of
  per-site overrides that win over the rules, `SetOverride(site, ref)`,
  and `Default(site)` (the answer without overrides). `Model(site)` becomes
  override → first matching rule → `default_model`. For `subagent`,
  `Model` keeps its current answer for callers that have no session; the
  agent never asks it for that site.
- Agent (`agent.go`, the `ref := a.Router.Model(site)` line): for `chat` and
  `subagent` sites the ref comes from the session's `chat_model` when set,
  else `Router.Model("chat")`. Other sites unchanged.
- At load, a `[[route]]` whose pattern matches `subagent` logs one warning:
  the rule no longer selects the sub-agent model.

## 2. Catalog (what is available)

New package `internal/catalog`.

- Providers that can list implement an optional interface
  `provider.Lister { ListModels(ctx) ([]string, error) }`. `openaicompat`
  implements it with `GET {base_url}/models` (OpenAI shape: `data[].id`;
  llama-server and LiteLLM both answer this).
- `Catalog.List(ctx)` fans out to every registered provider in parallel, 3s
  timeout each, and returns `[]Group{Provider, Refs []string, Err string}`
  sorted by provider name, refs sorted. Results are cached 60s;
  `List(ctx, fresh=true)` bypasses the cache.
- Providers without `Lister` (Anthropic) offer the refs `config.toml` names
  for them: `default_model` and every route model with that provider prefix.
- A selected or default ref whose provider failed or did not list it is still
  shown, marked unavailable, so the overview never hides what is in use.
- `Catalog.Has(ctx, ref)` checks against a fresh listing. A provider that
  errored makes `Has` false for its refs: a model cannot be chosen while its
  provider is down.

## 3. Storage

- `sessions` gains `chat_model TEXT NOT NULL DEFAULT ''` and
  `subagent_model TEXT NOT NULL DEFAULT ''`, added by the same guarded
  `ALTER TABLE` pattern as `workspace`/`source`. `store.Session` gains
  `ChatModel`, `SubagentModel`; `SetSessionModel(ctx, id, op, ref)` writes
  one of them.
- `CreateChildSession` takes the resolved model and stores it as the child's
  `chat_model`.
- Managed block in `config.toml`, separate from the policy block:

  ```toml
  # >>> spore-managed routing — written by /model; edit or delete freely
  [routing.override]
  title = "jetson/gemma-4-e2b-qat"
  # <<< spore-managed routing
  ```

  `config.SetRoutingOverride(path, site, ref)` writes it (ref `""` removes
  the key; an empty table removes the block), reusing the managed-block
  splitting in `write.go`. `config.ReadRoutingOverrides(path)` reads it.
  `config.Load` applies it, so overrides survive restarts. Only the four
  daemon-wide sites are accepted as keys.

## 4. API

- `GET /api/models?session=<id>` →

  ```json
  {"ops": [{"op": "chat", "scope": "session", "selected": "…", "default": "…"}, …],
   "groups": [{"provider": "jetson", "refs": ["jetson/gemma-4-e2b-qat"], "error": ""}, …]}
  ```

  Ops are always the six sites in the order above. Without `session`, the
  `chat`/`subagent` rows report defaults only. `?fresh=1` bypasses the cache.
- `PUT /api/sessions/{id}/model` `{"op": "chat"|"subagent", "ref": "…"}`.
- `PUT /api/routing` `{"op": "compaction"|"title"|"classify"|"refinement", "ref": "…"}`.
- Both PUTs: `ref` must satisfy `Catalog.Has`, or equal the op's default, or be
  `""`. Default or `""` clears. Errors are 400 with a message naming the op
  and listing nothing (the client already has the list). Both return the same
  body as `GET /api/models` for that session, so a client redraws from one
  response.
- The routes sit behind the existing `/api` token and Origin checks.

## 5. `agent_run` / `agent_spawn` `model` argument

- Optional `model` string in both schemas, described as "a provider/model ref
  from the models available to /model; omit to use this session's sub-agent
  model".
- Checked with `Catalog.Has` before the child is created. A miss is a tool
  error that lists the available refs, and no child, run row or session is
  created.
- Supervisor `Run`/`Spawn` take the resolved model; resolution of decision 3
  happens in the supervisor so both tools share it.

## 6. Surfaces

Shared text and layout come from `internal/modelcmd`: given the
`GET /api/models` body, it renders the overview and each op's list, and
numbers the options. Option order for an op: selected first, then default (if
different), then the rest grouped by provider.

### Plain chat (stage 1)

- `/model` prints the overview (one line per op: `op  -> selected  (* default)`),
  then each op's numbered list.
- `/model <op> <n>` selects option `n` of that op. Anything else prints usage.

### TUI (stage 2)

- `/model` (and `:model`) opens a modal with tabs: `overview`, then one per
  op. `tab`/`shift+tab` or `h`/`l` switch tabs; `j`/`k` move; `enter` selects
  the option under the cursor; `r` refreshes the catalog; `esc` closes.
- Overview tab: one row per op, selected model, `*` when it is the default,
  `(session)` or `(global)` scope.
- Op tab: rows are the op's options with `->` on the selected and `*` on the
  default; unavailable refs dimmed and not selectable; provider errors as
  a dim line under the list.
- A selection PUTs and redraws from the response; failure shows the error in
  the modal footer and leaves the modal open.

### Web (stage 3)

- `/model` in the input opens the same tabbed modal (overview + one tab per
  op), with the same markers, built in `web/app.js` alongside the existing
  modals. Clicking an option PUTs and redraws.

### Discord (stage 3)

- `/model` replies with the overview text and one string-select menu per op
  (Discord allows 5 action rows per message, so ops are split across two
  messages: chat, subagent, compaction, title, then classify, refinement).
- Each select's options are that op's choices (max 25; overflow beyond 25 is
  truncated with the selected and default always kept), selected marked
  `->` in the label and set as the default option, default marked `*`.
- A choice applies to the thread's session (chat/subagent) or daemon-wide
  (others). The reply states which, and for a daemon-wide change says it
  affects every session.
- Selects work only for the users in `user_ids`, like approval buttons.

## 7. Errors

- A session whose stored model's provider was removed from the config fails
  its turn with: `session model "x/y": provider "x" is not configured — pick
  another with /model`. No silent fallback.
- A managed override whose provider was removed: `config.Load` rejects it the
  same way it rejects a bad route ref today.
- A catalog fetch failure never blocks `/model` from opening; the provider's
  group carries the error.

## 8. Testing

- router: override precedence, `Default`, concurrent `Model`/`SetOverride`
  (race detector).
- config: managed routing block round trip; removing the last key removes the
  block; the policy block is untouched; `Load` applies overrides; unknown site
  key rejected.
- store: migration on an old DB, `SetSessionModel`, child created with model.
- catalog: parallel fetch with one slow (timeout) and one failing provider,
  caching and `fresh`, Anthropic refs from config, `Has`.
- agent: chat site uses `chat_model`; subagent site uses the child's
  `chat_model`; removed provider fails loudly; other sites ignore the
  session.
- supervisor/tools: precedence of decision 3; invalid `model` creates no
  child.
- daemon: GET/PUT routes against an `httptest` provider serving `/v1/models`
  and an unreachable one; PUT with a ref outside the catalog is 400.
- modelcmd: overview and list rendering, numbering, `/model <op> <n>`
  parsing.
- TUI: modal key handling (tabs, cursor, select, esc) against a fake backend.
- Discord: select payload shape (rows, ≤25 options, default marked), and a
  select from a user outside `user_ids` is ignored.

## Stages

1. **Core + plain chat**: sections 1–5, `internal/modelcmd`, plain-chat
   `/model`.
2. **TUI modal**.
3. **Web modal + Discord selects**.

Each stage is its own plan and PR.
