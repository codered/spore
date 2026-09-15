# spore — MCP path containment

**Date:** 2026-09-15
**Status:** approved (brainstorming dialogue)
**Amends:** `2026-08-29-spore-design.md` section 6 (MCP, policy engine), section
11 (stages)
**Closes:** `docs/backlog.md`, "MCP path arguments are not checked against the
session workspace"

## 1. What this adds

Section 6 of the design spec promises that "path arguments to MCP tools are
evaluated by the policy engine against the calling session's workspace like any
other tool's". The implementation does not do this. `baselineDeny` in
`internal/config/config.go` bounds `fs_*` only, and `mcp__*` appears only in the
editable default `ask` list with no path predicate. An MCP call naming a path
outside the session's workspace resolves to `ask`, and if a human approves it,
it runs with nothing in the policy engine bounding it.

This change keeps the promise. One rule joins the baseline deny set, which no
approval, learned rule or profile can override:

```text
mcp__*(any path outside workspace)
```

It uses a new predicate, `any path outside workspace`, that is valid only on
MCP tool globs.

The threat is a model steered by prompt injection. The server is not the
threat: declaring a server in config is already the operator's authorization
to run it. A trusted filesystem server asked for `~/.ssh/id_ed25519` will
return it, and the approval prompt is the only thing in the way today. Scheduled
jobs have no human to ask, so their MCP calls are already denied at timeout.
Interactive sessions and sub-agents are the ones exposed. A sub-agent's asks go
to the root of its chain, far from the call that made them.

## 2. Decisions

| Question (from the backlog) | Answer |
|---|---|
| Baseline deny or editable default? | **Baseline deny.** Section 6 already promised a hard bound. An editable rule protects only operators who never edit it. |
| How are path arguments found? | **Names and shape.** Values under path-like key names at any depth, plus any string at any depth that looks like an absolute path. Name-only detection misses servers with unusual argument names. Schema-based detection trusts a schema written by the server being restricted, the same reason `readOnlyHint` is ignored. |
| What about relative paths? | **Judge them where they open.** A stdio server runs in the workspace ceiling, not the session root, so a relative path is joined to the server's working directory before the check. |
| Servers whose "paths" are not local files? | **A per-server opt-out**, `local_paths = false` on `[[mcp.server]]`, set by the operator who declared the server. |

## 3. Architecture

### Config

`MCPServer` gains one field:

```go
// LocalPaths says whether this server's path-shaped arguments name files on
// this machine. When unset it is true, and the baseline rule checks them.
// Set it to false for a server whose paths are remote, such as repository
// paths or object keys; its calls are then not checked for paths at all.
LocalPaths *bool `toml:"local_paths"`
```

It is a pointer because a plain `bool` decodes a missing key as `false`, which
would exempt every server by default. `validateMCP` accepts the field for both
transports.

`PolicyConfig` gains a table that `Load` derives. It is not decoded from the
file:

```go
// MCPPaths says, per declared MCP server, whether its calls are checked for
// paths and where it resolves a relative path. Load fills it from
// [[mcp.server]]; it cannot be set under [policy].
MCPPaths map[string]MCPPathMode `toml:"-"`

type MCPPathMode struct {
    Checked bool
    // Cwd is the directory the server resolves a relative path against. It is
    // the workspace ceiling for stdio servers and empty for http servers,
    // whose working directory spore does not know.
    Cwd string
}
```

`Load` fills `MCPPaths` beside the line that prepends `baselineDeny`, after
`policy.workspace` has been expanded. `baselineDeny` gains the rule string from
section 1.

### Policy engine

`Env` gains the table:

```go
type Env struct {
    Workspace string                        // the calling session's root
    MCP       map[string]config.MCPPathMode // from PolicyConfig.MCPPaths
}
```

`NewEngine` copies `cfg.MCPPaths` into `Env.MCP`. `Evaluate` still replaces
`Workspace` with the session's root and leaves `MCP` as it is. None of the
existing `NewEngine` call sites change.

`Result` gains `Detail string`. Only the new predicate fills it. It names the
first offending path, where it resolved and the session root. To carry it out,
a predicate may implement an optional interface:

```go
type explainer interface {
    explain(c Call, env Env) string // called only after match returned true
}
```

`Evaluate` calls it on the deny rule that matched.

### The predicate

`parsePredicate` accepts `any path outside workspace`. `ParseRule` refuses it
unless the tool glob starts with `mcp__`, so the shape detection in section 4
can never reach `fs_write`'s `content` argument. `match`:

1. Takes the server name from `mcp__<server>__<tool>`, and looks it up in
   `Env.MCP`. A server not in the table is treated as `{Checked: true,
   Cwd: ""}`, so an unknown server fails closed. If `Checked` is false,
   `match` returns false.
2. Collects candidates as section 4 describes.
3. Resolves each one as section 5 describes, and matches if any candidate
   fails to resolve or resolves outside `Env.Workspace`.

### Guard and `spore policy check`

When `res.Detail` is set, the guard's deny message is:

```text
denied by policy rule "mcp__*(any path outside workspace)": <Detail>
```

This message does not say "Do not retry this call". A retry with a corrected
path is the intended recovery. Every other deny keeps today's wording.
`spore policy check` prints `Detail` on a second line, indented, when it is set.

## 4. What counts as a path

**Keys checked whatever their value looks like.** A string, or an array of
strings, under one of these keys at any depth: `path`, `paths`, `dir`,
`directory`, `file`, `filename`, `filepath`, `file_path`, `source`,
`destination`, `root`, `cwd`, `uri`. Keys are compared after lower-casing and
removing `_` and `-`, so `filePath`, `file_path` and `file-path` are one key.
Glob and pattern keys, such as the filesystem server's `pattern` and
`excludePatterns`, are deliberately absent. They are relative to a searched
directory that is itself checked.

**Strings checked by shape, under any key and at any depth.** A string counts
when it has no whitespace, is one line, and either:

- starts with `/` but not `//`;
- is `~` or starts with `~/`;
- or starts with `file://`.

The whitespace and `//` exclusions keep content such as `// TODO` or
`/fix the typo` from being judged as a path. A path containing a space is still
checked when it sits under a named key.

**Not paths, under a named key or not:** the empty string, values with any URL
scheme other than `file` (matching `^[A-Za-z][A-Za-z0-9+.-]*://`, such as
`https://` or `s3://`), `git@host:` remotes, and Windows drive paths (spore
runs on Unix only). Relative strings found only by shape are also skipped: a
relative value is checked only under a named key.

## 5. Resolution

For each candidate:

1. `file://` URIs are URL-decoded to their path. A URI with a host other than
   empty or `localhost` fails to resolve.
2. A glob is cut at its first `*`, `?`, `[` or `{`, and `filepath.Dir` of what
   comes before it is checked. `/tmp/*.log` and `/tmp/a*.log` are both judged
   as `/tmp`.
3. `~` and `~/` expand to the home directory.
4. A relative path is joined to the server's `Cwd`. If `Cwd` is empty, as for
   an http or unknown server, the candidate fails to resolve and the call is
   denied, with a detail asking for an absolute path.
5. The result is checked with `policy.Inside(env.Workspace, p)`, the same
   check the filesystem tools use. It resolves symlinks on the longest
   existing prefix, so a symlink out of the session root is outside.

## 6. Error handling

- Arguments that are not a JSON object are already refused by `Evaluate` as
  `policy.malformed-arguments`, before any rule runs. Nothing changes.
- A call with no path candidates does not match. It falls through to the
  editable rules, the default `ask` on `mcp__*`, or the `remote` profile's
  deny, exactly as today.
- An exempt server's calls are not checked at all. The opt-out is per server
  and is visible in its `[[mcp.server]]` block.
- The detail never includes argument values other than the offending path.

## 7. Testing

Every new test is seen failing before the code that makes it pass.

| File | Covers |
|---|---|
| `internal/policy/rule_test.go` | Detection: every named key, key variants, nesting, arrays. Shape hits: `/abs`, `~/x`, `file:///x`. Shape misses: `// TODO`, `/fix the typo`, `https://x`, multi-line text. Skipped under a named key too: `""`, `https://x`, `s3://b/k`. Globs judged up to their first wildcard. A `file://` URI with a remote host denied. `ParseRule` refusing the predicate on a non-`mcp__` glob. |
| `internal/policy/engine_test.go` | A relative path joined to the server's `Cwd`, not the session root: `notes.txt` is denied in a session below the ceiling. A relative path to an http or unknown server denied. An exempt server skipped. A symlink out of the session root denied. `Result.Detail` set only by this rule. |
| `internal/config/config_test.go` | `local_paths` accepted on both transports. A missing key means checked. `Load` fills `MCPPaths`, with `Cwd` set to the ceiling for stdio and empty for http. `baselineDeny` contains the rule, and `deny = []` cannot remove it. |
| `internal/config/mcp_policy_test.go` | The same behaviour through `Load`, `NewEngine` and `Evaluate`, which is the path the daemon uses. `TestOperatorCanOverrideTheRemoteMCPDeny` stays green, because its call carries no paths. |
| `internal/policy/guard_test.go` | The deny message carries the detail and omits "Do not retry". Other denies keep today's wording. |
| `internal/mcp/e2e_test.go` | A local-profile call with an out-of-workspace path is denied and never reaches the in-memory server. An in-workspace call does reach it. |
| `cmd/spore` | `spore policy check` prints the detail line when it is set. |

## 8. Documentation

- **README, `### MCP servers`:** the containment rule, `local_paths = false`,
  and the advice that models should send absolute paths to MCP tools.
- **README, `### Policy & tools`:** the new line in the baseline deny set.
- **Design spec, section 6:** the paragraph ending "the bound is per-session
  even though the process is not" names the rule and the predicate. "Policy
  needs no new mechanism" becomes: policy needed one baseline rule with an
  MCP-only predicate, and the default `ask` and `remote` deny remain ordinary
  editable lines.
- **Design spec, section 11:** stage 9, MCP path containment, pointing at this
  document.
- **`docs/backlog.md`:** the entry is closed, with its three open questions
  answered as in section 2.

## 9. Not in scope

- **MCP roots.** The protocol lets a client tell servers which directories
  they may use. spore runs one process per server for every session, so a root
  list cannot be per session. It may still be worth sending the ceiling as a
  root later, as a second layer. It is not a replacement for this rule.
- **Arguments that name a path in free text.** A string such as
  `open the file at /etc/passwd please` contains whitespace, so the shape rule
  skips it. Catching it would mean parsing prose. A server that takes paths
  inside prose is outside what a path rule can bound.
- **Per-tool exemptions.** An operator can exempt a whole server only. Tool
  names change between server versions, and a per-tool list would silently
  stop matching.
