# spore — soul.md and agent.md

**Date:** 2026-09-22
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-08-29-spore-design.md` (the assembled request),
`2026-09-20-spore-prompt-caching-design.md` (the stable prefix)

## 1. What this adds

Spore's standing instructions live in one place: the `system_prompt` key in
`config.toml`. It is a multi-line string inside a TOML file, it is global, and
in practice it is written once and never touched again. There is nowhere to say
"be blunter with me" and nowhere at all to say "in this project, always run
make lint before pushing".

This change adds two markdown files that the prompt reads.

`soul.md` is personality: voice, values, how direct to be, what to refuse. It
is global and yours alone -- spore never writes it. `system_prompt` keeps its
job as the terse operational identity ("you are spore, never name the model"),
because the two change on entirely different schedules.

`agent.md` is standing instructions for one workspace. It is the first thing in
spore scoped to a single project: memory facts are global, and a `project`-type
fact is global information that happens to be about a project.

Neither file is a fact and neither carries frontmatter.

## 2. Files and locations

| file | path | written by | scope |
|---|---|---|---|
| soul.md | `<DataDir>/soul.md`, i.e. `~/.spore/soul.md` | the user only | global |
| agent.md | `<workspace>/.spore/agent.md` | the user, or spore via `agent_note` | one workspace |

Both are optional. An absent file renders no section and is never an error: a
machine with neither file behaves exactly as spore does today.

Two accessors join `DBPath()`, `PidPath()` and `MemoryDir()` on `Config`:

```go
func (c *Config) SoulPath() string
func (c *Config) AgentPath(workspace string) string  // "" when workspace is ""
```

`agent.md` is **always** at `<workspace>/.spore/`, and is deliberately not
governed by `Skills.Scope`. Skills have a scope setting because a skill is a
library that might reasonably be shared or kept globally. Standing instructions
for this project are meaningless anywhere else, so there is no second place
they could live and no setting to get wrong.

It sits under `.spore/` rather than at the workspace root, which settles a
trust question rather than answering it. A committed `agent.md` at the root
would mean that cloning a repository and opening a session in it feeds a
stranger's instructions into the prompt. Under `.spore/` -- beside the
workspace-scoped skills directory, gitignored by convention -- the file is the
user's own and nobody else's, and no approval gate is needed because nothing
untrusted can arrive there.

## 3. The prompt

`Assemble` gains two blocks. The full stable prefix becomes:

```
system prompt          config.toml, operational
soul.md                "## Who you are"
self section           where spore keeps its files
skills index
memory facts
agent.md               "## Working in this project"
summary
———————————————————————— cache breakpoint
conversation
environment            per turn, after the moving breakpoint
```

`soul.md` sits directly under the system prompt because it is identity, and
identity belongs with identity. `agent.md` sits last in the prefix because it
is the most situational thing in it: closest to the conversation it governs.

Both are read per turn rather than held in a cache with an invalidation path.
They are a couple of kilobytes, `Snapshot` already reads SQLite on every turn,
and reading per turn has no staleness bug to fix and no `Reload()` to get
wrong. The cost is honest and small: editing either file mid-session changes
the cached prefix, so that turn pays a cache miss and the next one is cached
again. `agent_note` writing the file costs the same miss.

`Snapshot` gains `Soul` and `Agent` string fields, filled in `Agent.Snapshot`
from the same session root the environment section already resolves through
`policy.WorkspaceFrom(ctx)`.

### What the user can ask for

Naming the paths tells the model where things live. It does not tell it that
the user may simply ask for them to be changed, and a model that knows only the
path answers "your skills go in ~/.spore/skills" when the user wanted a skill
written. The self section therefore gains a short closing paragraph naming the
three requests and the tool each one reaches for:

```
The user can ask you to do these things directly. "Write me a skill for X"
is skill_install. "From now on in this project, always X" is agent_note.
"Remember that X" is memory. Each asks for their approval before it writes.

soul.md is theirs, not yours: you cannot write it. When they ask you to
change how you behave in general rather than in one project, tell them the
path and what to add, and let them make the edit.
```

The asymmetry is the point. Two of the three are things spore does on request;
the third is a file spore reads and the user owns, and a model that does not
know the difference will either attempt a write that no tool offers or refuse a
request it could have satisfied by pointing at a path.

The same paragraph says how to install a skill from somewhere:

```
skill_install takes the skill's text, not a location. To install one from
a file or a URL, read it first -- web_fetch for a URL, fs_read for a file
in the workspace -- and pass what you read to skill_install. A file outside
the workspace cannot be read at all, whoever approves it: say so and offer
to install it if the user moves it into the workspace or starts a session
rooted where it lives.
```

`skill_install` takes `name`, `description` and `body`: there is no path or
URL argument, so installing from a location is a two-step the model has to know
about rather than infer. The closing sentence is the load-bearing one.
`fs_*(path outside workspace)` is in `baselineDeny`, which is always in force
and which `Guard.Run` never escalates to a human, so a skill sitting outside
the session's workspace is refused outright and no approval can talk past it.
Left unexplained that reads as an arbitrary failure; the prompt gives the way
around it instead.

This extends the self section added by the prompt self-knowledge change, which
must land first.

## 4. The `internal/persona` package

Pure I/O, no rendering:

```go
// Load reads a persona file. A missing file returns "" and no error.
func Load(path string) (string, error)
```

A file beyond `warnBytes` (16 KiB, roughly 4000 tokens -- large enough that no
deliberate soul.md reaches it by accident) is still loaded whole -- these are the user's own
standing instructions, deliberate in a way a fact is not, and silently cutting
them in half is worse than the tokens they cost. It logs `slog.Warn` naming the
path and the size, so an accident is visible in the daemon log without the
prompt lying about what it contains.

Rendering stays in `agent/context.go` beside `selfSection` and `factsSection`.
This is the split skills and memory already use: the package loads, context
renders.

## 5. The `agent_note` tool

Append-only. `{"text": "..."}` appends the line `- <text>\n` to
`<workspace>/.spore/agent.md`, creating `.spore/` at 0700 and the file at 0600
if they do not exist. A file created this way holds only the bullets: the
`## Working in this project` heading is supplied by the renderer, so the file
never carries one and appending never has to find its place under one. There is
no edit or delete verb: the file is plain markdown the user can open, and a
second verb earns its place only once appending proves insufficient.

It fails when the session has no workspace, in the style `dirFor` already uses
for the workspace-scoped skills directory.

Three policy placements, each mirroring `skill_install`:

- **default profile `Ask`** -- writing standing instructions is never silent.
- **`remote` profile `Deny`** -- a Discord user must not be able to rewrite the
  operator's standing instructions. This follows the same reasoning that
  already denies `memory` and `skill_install` there.
- **`nonLearnable`** -- an approval must never widen into a standing rule. The
  existing comment on that map is exactly the argument: a skill written once
  shapes every later turn in every session, so each write is approved on its
  own. A file of standing instructions shapes every later turn in this
  workspace, and earns the same treatment.

## 6. What goes where

Spore now has two places to record something about a project, so the rule has
to be one a model can actually apply turn by turn:

> `agent.md` holds standing instructions -- things the user said to always do
> here. Facts hold what spore observed or was told.

"Always run make lint before pushing" is `agent.md`. "This repo uses Turborepo"
is a fact. The test is whether it is an order or an observation, and it is
stated in the prompt so the model applies the same rule the design does.

`project`-type facts keep their place: they remain the way to record something
about a project spore is not currently rooted in.

## 7. Testing

`persona.Load`: a missing file, an ordinary file, an oversized file that warns
and still loads whole.

Assembly: both sections present and in the documented order; an absent file
rendering nothing at all; `agent.md` absent when the session has no workspace;
the new blocks landing before the cache breakpoint rather than after it.

The tool: appending to an existing file, creating the directory and file when
neither exists, refusing when there is no workspace.

Policy: the `remote` profile denies `agent_note`, and `PatternFor` refuses to
learn a rule from it.

That policy test must build its config through `config.Load`, never
`config.Default()`. `Load` adds the baseline deny that `Default` does not, and
a policy test built on `Default` silently loses the security assertion it
exists to make. This has already cost this repository once.

## 8. Out of scope

- Reading `agent.md` from parent directories. One workspace, one file.
- Any committed or shared variant of `agent.md`, and therefore any approval
  gate for untrusted instructions.
- Editing or deleting notes through a tool.
- A token budget or truncation for either file.
- Spore writing `soul.md`. A model revising its own standing instructions is a
  feedback loop worth being deliberate about, and nothing yet needs it.
