# Discord Bridge (Plan 4a) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one allowlisted person drive spore from Discord — send prompts, watch the turn stream, and answer approval prompts with buttons — from a machine behind NAT.

**Architecture:** The bridge is a supervised goroutine inside the daemon and a *client* of machinery the web UI already uses. It subscribes to `daemon.Hub` for a session's `WireEvent`s, starts turns through the server, and answers approvals through `daemon.Broker.Answer` / `policy.Guard.Resolve`. It never implements `policy.Approver` itself, because `Guard.Resolve` and `Broker.Answer` carry the session-ownership check that stops a `remote` session answering a `local` session's approval. Everything Discord-specific sits behind a `Client` interface with one real (`discordgo`) implementation and a fake for tests, so no test touches the network.

**Tech Stack:** Go 1.26, `github.com/bwmarrin/discordgo`, SQLite (`mattn/go-sqlite3`, `sqlite_fts5` build tag), `BurntSushi/toml`.

**Spec:** `docs/superpowers/specs/2026-08-29-spore-design.md` — §6 Policy engine (the pattern-suppression bullet) and §8 Bridges.

## Global Constraints

- Module path is `github.com/codered/spore`. Go directive: `go 1.26`.
- **Every build and test needs the FTS5 tag.** Use `make build`, `make test`, `make vet` — never a bare `go test ./...`. The raw form is `go test -tags sqlite_fts5 ./...`.
- No live network calls in any test (spec §10). Discord is reached only through the `Client` interface; tests use the fake.
- Any test that exercises policy must obtain its config through `config.Load` on a real temp TOML file, **never** `config.Default()`. `Load` is what appends `baselineDeny`; a config built from `Default` has no baseline, so an allow rule matches by tool name regardless of arguments and the test proves nothing.
- The daemon knows the core; the core never knows the daemon (spec invariant 1). `internal/bridge/discord` may import `internal/daemon`, `internal/policy`, `internal/store` and `internal/config`. Nothing imports `internal/bridge`.
- A turn outlives the client that started it (spec invariant 2). The bridge must never hand a per-message context to a turn.
- Wire event type strings in `internal/daemon/event.go` are the API and are **append-only**.
- Every exported symbol gets a doc comment; match the surrounding comment density, which is high and explains *why* rather than *what*.
- Commit after every task. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `refactor:`, `docs:`).

## Reviewer's note on evidence

Implementer reports on this project have contained reconstructed rather than
pasted command output. If you did not capture a command's output, write "ran
`make test`, it passed, output not captured" — that is the preferred report.
Do not retype output from memory. The orchestrator re-runs every claim.

## File Structure

**New:**

| File | Responsibility |
|---|---|
| `internal/bridge/discord/admit.go` | `Admitter` — the only place membership is decided. Pure. |
| `internal/bridge/discord/client.go` | `Client` interface + the `discordgo` adapter. The only file that imports `discordgo`. |
| `internal/bridge/discord/render.go` | `WireEvent` stream → throttled message edits, chunking, tool embeds. |
| `internal/bridge/discord/approve.go` | Approval event → message components; component interaction → an answer. |
| `internal/bridge/discord/bridge.go` | Inbound message → session (thread create / DM rolling), dedupe, `/new`. |
| `internal/bridge/discord/supervise.go` | Reconnect with backoff; the `Run(ctx)` the daemon supervises. |
| `internal/bridge/discord/fake_test.go` | The fake `Client` shared by every test in the package. |

**Modified:**

| File | Change |
|---|---|
| `internal/config/config.go` | `BridgeConfig` / `DiscordConfig`, defaults, validation in `Load`. |
| `internal/store/schema.go` | `bridge_bindings` and `bridge_seen` tables. |
| `internal/store/store.go` | `BindExternal`, `SessionForExternal`, `MarkSeen`, `PendingCallByID`. |
| `internal/policy/guard.go` | `PatternFor` returns `(string, bool)`; `Run` and `Resolve` downgrade a degraded pattern answer. |
| `internal/daemon/approver.go` | `pendingApprovalEvents` uses the two-value `PatternFor`. |
| `internal/daemon/sessions.go` | `startTurn` takes a `policy.Profile`; new exported `StartTurn` for the bridge. |
| `cmd/spore/approve.go` | Terminal prompt hides `[p]attern` when the pattern is empty. |
| `cmd/spore/wire.go`, `cmd/spore/serve.go` | Build and supervise the bridge. |
| `README.md` | Status and a Discord setup section. |

---

### Task 1: Suppress the approval pattern when it degrades

Deriving a policy pattern needs a path-shaped argument. Without one, `PatternFor` falls back to the bare tool name, so "always allow this pattern" on a `shell_exec` prompt writes an allow for *every* `shell_exec`. Make the degradation visible in the signature so callers cannot ignore it, hide the option in every client, and enforce it in the guard so presentation is not the only defence.

**Files:**
- Modify: `internal/policy/guard.go:248-258` (`PatternFor`), `:165-175` (the `Ask` in `Run`), `:271-296` (`Resolve`)
- Modify: `internal/store/store.go` (add `PendingCallByID`)
- Modify: `internal/daemon/approver.go:171-181` (`pendingApprovalEvents`)
- Modify: `cmd/spore/approve.go:32-36` (the prompt line)
- Test: `internal/policy/guard_test.go`, `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `policy.PatternFor(c Call) (pattern string, ok bool)` — `ok` is false when no single path-shaped argument was found.
  - `store.PendingCallByID(ctx context.Context, id int64) (PendingCall, bool, error)`
  - The convention every later task relies on: **`policy.Ask.Pattern == ""` means "do not offer the pattern option".**

- [ ] **Step 1: Write the failing tests for `PatternFor`**

Add to `internal/policy/guard_test.go`:

```go
func TestPatternForReportsDegradation(t *testing.T) {
	cases := []struct {
		name    string
		call    Call
		want    string
		wantOK  bool
	}{
		{
			name:   "a single path generalises to its directory",
			call:   Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"/w/src/main.go"}`)},
			want:   "fs_read(path matches /w/src/**)",
			wantOK: true,
		},
		{
			name: "no path-shaped argument degrades",
			// This is the case the whole task exists for: the only pattern
			// derivable from a shell command is the bare tool name, which
			// would allow every shell_exec there is.
			call:   Call{Tool: "shell_exec", Args: json.RawMessage(`{"cmd":"ls -l"}`)},
			want:   "",
			wantOK: false,
		},
		{
			name:   "two paths are ambiguous and degrade",
			call:   Call{Tool: "fs_edit", Args: json.RawMessage(`{"from":"/w/a.go","to":"/w/b.go"}`)},
			want:   "",
			wantOK: false,
		},
		{
			name:   "a bare filename has no directory to generalise to",
			call:   Call{Tool: "fs_read", Args: json.RawMessage(`{"path":"notes.md"}`)},
			want:   "",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := PatternFor(tc.call)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("PatternFor = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run TestPatternForReportsDegradation -v`
Expected: FAIL to compile — `assignment mismatch: 2 variables but PatternFor returns 1 value`.

- [ ] **Step 3: Change `PatternFor` to report degradation**

Replace `PatternFor` in `internal/policy/guard.go`:

```go
// PatternFor proposes the rule an "always allow this pattern" answer would
// write, and reports whether a real pattern exists. Deriving one needs a
// single path-shaped argument. Without one the only thing left is the bare
// tool name, and a rule that broad is not a pattern — it is a blanket allow
// for the tool, bounded only by the baseline deny list. Rather than return
// something that reads like a narrow rule and behaves like a wide one, this
// reports false and callers suppress the option.
func PatternFor(c Call) (string, bool) {
	paths := argPaths(c)
	if len(paths) != 1 {
		return "", false
	}
	dir := filepath.Dir(paths[0])
	if dir == "." || dir == string(filepath.Separator) {
		return "", false
	}
	return fmt.Sprintf("%s(path matches %s/**)", c.Tool, strings.TrimSuffix(dir, "/")), true
}
```

- [ ] **Step 4: Fix the three call sites so the package compiles**

In `internal/policy/guard.go`, inside `Run`, replace `pattern := PatternFor(c)` with:

```go
	// An empty pattern is the wire signal to every approver — terminal,
	// browser and bridge — that the "always this pattern" option must not
	// be offered for this call.
	pattern, patternOK := PatternFor(c)
```

Still inside `Run`, in the `if claimed { ... }` block, replace the scope handling:

```go
	if claimed {
		// A pattern answer for a call with no derivable pattern is recorded
		// as "once". A client that offers the option anyway — an old build,
		// or a crafted request straight to the API — must not be able to
		// widen policy by asking. Presentation is not the enforcement.
		scope := answer.Scope
		if scope == ScopePattern && !patternOK {
			scope = ScopeOnce
			sporetrace.RecordPolicy(ctx, string(decision), "pattern answer degraded to once: no pattern for this call")
		}
		_ = g.store.RecordApproval(book, sessionID, call.Name, call.Input, string(decision), string(scope))

		if scope == ScopePattern && g.learn != nil {
			if err := g.learn(decision, pattern); err != nil {
				// Failing to persist the rule must not change this call's
				// outcome; the user simply gets asked again next time.
				sporetrace.RecordPolicy(ctx, string(decision), "learned rule not persisted: "+err.Error())
			}
		}
	}
```

In `internal/daemon/approver.go`, inside `pendingApprovalEvents`, replace the `Pattern:` line. The variable must be declared before the struct literal:

```go
	for _, p := range pending {
		// Ignore the ok flag: an empty pattern is exactly what the client
		// needs to see to hide the option.
		pattern, _ := policy.PatternFor(policy.Call{Tool: p.Tool, Args: p.ArgsJSON})
		out = append(out, WireEvent{
			Type: WireApproval, PendingID: p.ID, Tool: p.Tool,
			Args: string(p.ArgsJSON), Rule: p.Rule, Pattern: pattern,
		})
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/policy/ ./internal/daemon/ -v`
Expected: `TestPatternForReportsDegradation` PASSes. Other tests in these packages may fail if they assert the old bare-tool-name fallback — update those assertions to expect `("", false)`; that is the behaviour change this task is making, not a regression.

- [ ] **Step 6: Commit**

```bash
git add internal/policy/guard.go internal/policy/guard_test.go internal/daemon/approver.go
git commit -m "feat(policy): report when an approval pattern degrades to a bare tool name"
```

- [ ] **Step 7: Write the failing test for `store.PendingCallByID`**

`Guard.Resolve` needs the call's arguments *before* it claims the suspension, because `ClaimPendingCall` writes the audit row with the scope it is given and that scope must already be corrected. Add to `internal/store/store_test.go`:

```go
func TestPendingCallByID(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddPendingCall(ctx, PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "remote", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	got, found, err := st.PendingCallByID(ctx, id)
	if err != nil || !found {
		t.Fatalf("PendingCallByID = (_, %v, %v), want (_, true, nil)", found, err)
	}
	if got.Tool != "shell_exec" || string(got.ArgsJSON) != `{"cmd":"ls"}` {
		t.Fatalf("got %+v, want tool shell_exec with its args", got)
	}

	if _, found, err := st.PendingCallByID(ctx, id+999); err != nil || found {
		t.Fatalf("missing id: got (found=%v, err=%v), want (false, nil)", found, err)
	}
}
```

Note: `openTestStore` is the existing helper in this package. If it is named differently, use whatever the neighbouring tests use — do not add a second helper.

- [ ] **Step 8: Run it to watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run TestPendingCallByID -v`
Expected: FAIL to compile — `st.PendingCallByID undefined`.

- [ ] **Step 9: Implement `PendingCallByID`**

Add to `internal/store/store.go`, next to `PendingCalls`:

```go
// PendingCallByID reads one suspension without claiming it. Resolve needs the
// arguments before it claims, because the claim writes the audit row and the
// scope on that row must already be correct.
func (s *Store) PendingCallByID(ctx context.Context, id int64) (PendingCall, bool, error) {
	var p PendingCall
	var args, created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, session_id, tool_use_id, tool, args, profile, rule, created_at
		 FROM pending_calls WHERE id = ?`, id).
		Scan(&p.ID, &p.SessionID, &p.ToolUseID, &p.Tool, &args, &p.Profile, &p.Rule, &created)
	if err == sql.ErrNoRows {
		return PendingCall{}, false, nil
	}
	if err != nil {
		return PendingCall{}, false, fmt.Errorf("read pending call %d: %w", id, err)
	}
	p.ArgsJSON = []byte(args)
	p.CreatedAt, _ = time.Parse(timeFormat, created)
	return p, true, nil
}
```

- [ ] **Step 10: Run it to watch it pass**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run TestPendingCallByID -v`
Expected: PASS.

- [ ] **Step 11: Write the failing test for the `Resolve` downgrade**

Add to `internal/policy/guard_test.go`. Build the guard the way the neighbouring tests do; the only new thing is asserting on the `learn` callback and the recorded scope.

```go
func TestResolveDowngradesADegradedPatternAnswer(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}
	learned := []string{}
	g := NewGuard(nil, newTestEngine(t), nil, st, func(d Decision, rule string) error {
		learned = append(learned, string(d)+" "+rule)
		return nil
	})

	id, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: sid, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "remote", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A client asks for ScopePattern on a call that has no pattern.
	if err := g.Resolve(ctx, sid, id, Answer{Allow: true, Scope: ScopePattern}); err != nil {
		t.Fatal(err)
	}

	if len(learned) != 0 {
		t.Fatalf("a rule was learned for a call with no pattern: %v", learned)
	}
	// The audit row must say what actually happened, not what was asked for.
	scope, err := lastApprovalScope(t, st, sid)
	if err != nil {
		t.Fatal(err)
	}
	if scope != string(ScopeOnce) {
		t.Fatalf("audit scope = %q, want %q", scope, ScopeOnce)
	}
}
```

Add this helper to the same test file:

```go
// lastApprovalScope reads the scope of the newest audit row for a session.
func lastApprovalScope(t *testing.T, st *store.Store, sessionID string) (string, error) {
	t.Helper()
	rows, err := st.Approvals(context.Background(), sessionID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("no approval rows for session %s", sessionID)
	}
	return rows[len(rows)-1].Scope, nil
}
```

If `store.Approvals` does not exist, read the row with a direct query through whatever accessor the store package already exposes to its tests rather than adding a new exported method for a test's benefit.

- [ ] **Step 12: Run it to watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/policy/ -run TestResolveDowngrades -v`
Expected: FAIL — a rule was learned, and the audit scope is `pattern`.

- [ ] **Step 13: Make `Resolve` downgrade before it claims**

In `internal/policy/guard.go`, replace the body of `Resolve` down to the `learn` call:

```go
func (g *Guard) Resolve(ctx context.Context, sessionID string, pendingID int64, ans Answer) error {
	decision := DecisionDeny
	if ans.Allow {
		decision = DecisionAllow
	}
	// Correct the scope BEFORE claiming: the claim writes the audit row, and
	// an audit row that says "pattern" when no rule was learned is a lie in
	// the log. Reading the row first is safe — a suspension's arguments never
	// change, and the claim itself is still the atomic step.
	if ans.Scope == ScopePattern {
		p, found, err := g.store.PendingCallByID(ctx, pendingID)
		if err != nil {
			return err
		}
		if found {
			if _, ok := PatternFor(Call{Tool: p.Tool, Args: p.ArgsJSON}); !ok {
				ans.Scope = ScopeOnce
			}
		}
	}
	// One transaction claims the suspension and writes its audit row together.
	// Two clients answering at once cannot both record an answer, and a
	// failure part-way cannot leave a resolved row with no audit entry.
	claimed, won, err := g.store.ClaimPendingCall(ctx, pendingID, sessionID, string(decision), string(ans.Scope))
	if err != nil {
		return err
	}
	if !won {
		return fmt.Errorf("no pending call %d in session %s (already answered, or another session's)", pendingID, sessionID)
	}
	if ans.Scope == ScopePattern && g.learn != nil {
		pattern, ok := PatternFor(Call{Tool: claimed.Tool, Args: claimed.ArgsJSON})
		if ok {
			if err := g.learn(decision, pattern); err != nil {
```

Keep the rest of the existing function body — the error handling after `g.learn` and everything below it — unchanged, adjusting the closing braces for the new `if ok {` level.

- [ ] **Step 14: Run the whole policy and daemon suites**

Run: `make test`
Expected: PASS. If a pre-existing test asserted that a `shell_exec` pattern answer learns `shell_exec`, that test encoded the bug — update it to assert no rule is learned, and say so in the commit message.

- [ ] **Step 15: Mutation-test the suppression**

This is the check that proves the new tests are load-bearing rather than decorative.

```bash
# Temporarily restore the degraded fallback:
#   in PatternFor, change `return "", false` (the len(paths) != 1 branch)
#   to `return c.Tool, true`
go test -tags sqlite_fts5 ./internal/policy/ -run 'TestPatternForReportsDegradation|TestResolveDowngrades' -v
# EXPECTED: both tests FAIL. If either passes, the test is not testing anything.
# Then revert the mutation and re-run to confirm PASS.
```

Record in the task report which tests failed under mutation.

- [ ] **Step 16: Hide the option in the terminal approver**

The web UI already guards on `if (ev.pattern)` at `web/app.js:147`, so it needs no change. The terminal prompt does. In `cmd/spore/approve.go`, replace the prompt block inside the `for` loop:

```go
		// An empty Pattern means the guard found no pattern to generalise to,
		// so the option is not offered. Do not print a key the user can press
		// that would silently be treated as "once".
		if a.Pattern == "" {
			fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)\n> ", a.Tool)
		} else {
			fmt.Fprintf(t.out, "allow? [y]es once  [n]o  [s]ession (always %s this session)  [p]attern (always %s)\n> ",
				a.Tool, a.Pattern)
		}
```

And in the `switch` below, replace the `case "p", "pattern":` arm:

```go
		case "p", "pattern":
			if a.Pattern == "" {
				fmt.Fprintln(t.out, "there is no pattern to generalise this call to; answer y, n or s")
				continue
			}
			return policy.Answer{Allow: true, Scope: policy.ScopePattern}, nil
```

Also fix the stale comment at the top of the file: "the daemon and the Telegram bridge implement the same interface over SSE and inline keyboards in Plans 3 and 4" → "the daemon implements it over SSE in Plan 3; the Discord bridge answers through Guard.Resolve rather than implementing this interface at all".

- [ ] **Step 17: Verify and commit**

Run: `make vet && make test`
Expected: PASS.

```bash
git add internal/policy internal/store internal/daemon cmd/spore/approve.go
git commit -m "feat(policy): never learn a blanket rule from a degraded pattern answer"
```

---

### Task 2: Discord configuration

One `[bridge.discord]` block. Validation is strict on purpose: this is the first config that decides who may reach the agent, and a half-filled block must fail loudly at load rather than quietly admit nobody or — worse — everybody.

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
```go
type BridgeConfig struct{ Discord DiscordConfig `toml:"discord"` }

type DiscordConfig struct {
	Enabled    bool     `toml:"enabled"`
	Token      string   `toml:"token"`
	GuildID    string   `toml:"guild_id"`
	ChannelIDs []string `toml:"channel_ids"`
	UserIDs    []string `toml:"user_ids"`
	AllowDMs   bool     `toml:"allow_dms"`
}
```
  reachable as `cfg.Bridge.Discord`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`. Use the file-writing helper the existing tests use (they already write temp TOML and call `Load`).

```go
func TestLoadDiscordBridge(t *testing.T) {
	t.Setenv("SPORE_TEST_DISCORD_TOKEN", "s3cret")
	cfg := loadTestConfig(t, `
[bridge.discord]
enabled     = true
token       = "${SPORE_TEST_DISCORD_TOKEN}"
guild_id    = "111"
channel_ids = ["222", "333"]
user_ids    = ["444"]
allow_dms   = true
`)
	d := cfg.Bridge.Discord
	if !d.Enabled || d.Token != "s3cret" || d.GuildID != "111" || !d.AllowDMs {
		t.Fatalf("discord config not loaded: %+v", d)
	}
	if len(d.ChannelIDs) != 2 || d.ChannelIDs[0] != "222" {
		t.Fatalf("channel_ids = %v", d.ChannelIDs)
	}
	if len(d.UserIDs) != 1 || d.UserIDs[0] != "444" {
		t.Fatalf("user_ids = %v", d.UserIDs)
	}
}

func TestDiscordBridgeValidation(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "enabled with no token",
			toml: "[bridge.discord]\nenabled = true\nguild_id = \"1\"\nchannel_ids = [\"2\"]\nuser_ids = [\"3\"]\n",
			want: "bridge.discord.token",
		},
		{
			// An empty user allowlist admits nobody, which is safe but is
			// always a mistake — the bridge would sit there ignoring you.
			// Failing at load beats debugging silence.
			name: "enabled with no users",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nchannel_ids = [\"2\"]\n",
			want: "bridge.discord.user_ids",
		},
		{
			name: "enabled with a guild but no channels",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nguild_id = \"1\"\nuser_ids = [\"3\"]\n",
			want: "bridge.discord.channel_ids",
		},
		{
			name: "enabled with no surface at all",
			toml: "[bridge.discord]\nenabled = true\ntoken = \"t\"\nuser_ids = [\"3\"]\n",
			want: "guild_id",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadTestConfigErr(t, tc.toml)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDisabledDiscordBridgeSkipsValidation(t *testing.T) {
	// A block left behind with enabled = false must not block startup.
	if _, err := loadTestConfigErr(t, "[bridge.discord]\nenabled = false\n"); err != nil {
		t.Fatalf("a disabled bridge should not be validated: %v", err)
	}
}
```

If `loadTestConfig` / `loadTestConfigErr` do not exist, add them beside the existing test helpers: both write `toml` to `filepath.Join(t.TempDir(), "config.toml")` and call `Load` on it; the first `t.Fatal`s on error, the second returns it.

- [ ] **Step 2: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run Discord -v`
Expected: FAIL to compile — `cfg.Bridge undefined`.

- [ ] **Step 3: Add the types**

In `internal/config/config.go`, add `Bridge BridgeConfig \`toml:"bridge"\`` to the `Config` struct (after `Daemon`), and the types after `DaemonConfig`:

```go
// BridgeConfig groups the chat bridges. Only Discord exists; Telegram is the
// same interface implemented again and is deferred.
type BridgeConfig struct {
	Discord DiscordConfig `toml:"discord"`
}

// DiscordConfig is both the connection and the trust boundary. GuildID,
// ChannelIDs and UserIDs are an allowlist, not a filter: the bridge is the
// first surface someone other than the local human can reach, so anything
// not named here is dropped. UserIDs applies to guild messages and DMs
// alike — the two surfaces must not be able to drift apart.
type DiscordConfig struct {
	Enabled bool   `toml:"enabled"`
	Token   string `toml:"token"`
	// GuildID is the one server the bot serves. Membership of a private
	// guild is the outer boundary; the user allowlist is the inner one.
	GuildID string `toml:"guild_id"`
	// ChannelIDs are the channels a session may be started from. A thread's
	// parent channel is what is matched, so threads need no entry.
	ChannelIDs []string `toml:"channel_ids"`
	UserIDs    []string `toml:"user_ids"`
	// AllowDMs opens the direct-message surface to the same user allowlist.
	AllowDMs bool `toml:"allow_dms"`
}
```

- [ ] **Step 4: Add validation to `Load`**

Find where `Load` validates the daemon address and add alongside it:

```go
	if err := validateDiscord(cfg.Bridge.Discord); err != nil {
		return nil, err
	}
```

And the function:

```go
// validateDiscord fails a half-filled bridge block at load. Every message
// this returns names the exact key to fix, because the failure mode it
// prevents — a bridge that starts and then silently ignores you — is
// otherwise very hard to diagnose from the outside.
func validateDiscord(d DiscordConfig) error {
	if !d.Enabled {
		return nil
	}
	if strings.TrimSpace(d.Token) == "" {
		return fmt.Errorf("bridge.discord.token is required when the bridge is enabled")
	}
	if len(d.UserIDs) == 0 {
		return fmt.Errorf("bridge.discord.user_ids must name at least one Discord user ID; an empty allowlist admits nobody")
	}
	if d.GuildID == "" && !d.AllowDMs {
		return fmt.Errorf("bridge.discord needs a surface: set guild_id, or allow_dms = true, or both")
	}
	if d.GuildID != "" && len(d.ChannelIDs) == 0 {
		return fmt.Errorf("bridge.discord.channel_ids must name at least one channel when guild_id is set")
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config
git commit -m "feat(config): add the [bridge.discord] block and validate its allowlist"
```

---

### Task 3: Durable bridge bindings and event dedupe

A thread must still be its session after a daemon restart, and the Discord gateway redelivers events when it resumes, so the same message can arrive twice.

**Files:**
- Modify: `internal/store/schema.go`, `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
```go
func (s *Store) BindExternal(ctx context.Context, bridge, externalID, sessionID string) error
func (s *Store) SessionForExternal(ctx context.Context, bridge, externalID string) (string, bool, error)
func (s *Store) MarkSeen(ctx context.Context, bridge, eventID string) (fresh bool, err error)
func (s *Store) PruneSeen(ctx context.Context, olderThan time.Duration) error
```
  `MarkSeen` returns true the first time an event ID is presented and false every time after.

- [ ] **Step 1: Write the failing tests**

```go
func TestBridgeBindings(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	sid, err := st.CreateSession(ctx, "t")
	if err != nil {
		t.Fatal(err)
	}

	if _, found, err := st.SessionForExternal(ctx, "discord", "thread-1"); err != nil || found {
		t.Fatalf("unbound thread: (found=%v, err=%v), want (false, nil)", found, err)
	}
	if err := st.BindExternal(ctx, "discord", "thread-1", sid); err != nil {
		t.Fatal(err)
	}
	got, found, err := st.SessionForExternal(ctx, "discord", "thread-1")
	if err != nil || !found || got != sid {
		t.Fatalf("got (%q, %v, %v), want (%q, true, nil)", got, found, err, sid)
	}

	// Two bridges may use the same external id without colliding.
	if _, found, _ := st.SessionForExternal(ctx, "telegram", "thread-1"); found {
		t.Fatal("bindings leaked across bridges")
	}

	// Rebinding is idempotent, not an error: the DM surface rebinds its
	// rolling session every time /new is used.
	sid2, _ := st.CreateSession(ctx, "t2")
	if err := st.BindExternal(ctx, "discord", "thread-1", sid2); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.SessionForExternal(ctx, "discord", "thread-1")
	if got != sid2 {
		t.Fatalf("rebind: got %q, want %q", got, sid2)
	}
}

func TestMarkSeenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	fresh, err := st.MarkSeen(ctx, "discord", "msg-1")
	if err != nil || !fresh {
		t.Fatalf("first sighting: (%v, %v), want (true, nil)", fresh, err)
	}
	// The gateway redelivers on resume. The second sighting must not run a
	// turn, so it must report false.
	fresh, err = st.MarkSeen(ctx, "discord", "msg-1")
	if err != nil || fresh {
		t.Fatalf("redelivery: (%v, %v), want (false, nil)", fresh, err)
	}
	fresh, _ = st.MarkSeen(ctx, "discord", "msg-2")
	if !fresh {
		t.Fatal("a different message id must be fresh")
	}
}

func TestPruneSeen(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	if _, err := st.MarkSeen(ctx, "discord", "old"); err != nil {
		t.Fatal(err)
	}
	// Nothing is old enough to prune yet.
	if err := st.PruneSeen(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.MarkSeen(ctx, "discord", "old"); fresh {
		t.Fatal("PruneSeen removed a row that was inside the window")
	}
	// Everything is older than zero.
	if err := st.PruneSeen(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := st.MarkSeen(ctx, "discord", "old"); !fresh {
		t.Fatal("PruneSeen did not remove a row outside the window")
	}
}
```

- [ ] **Step 2: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'Bridge|MarkSeen|PruneSeen' -v`
Expected: FAIL to compile — the methods are undefined.

- [ ] **Step 3: Add the schema**

Append to `schemaSQL` in `internal/store/schema.go`:

```sql
-- bridge_bindings maps a chat surface's own identifier — a Discord thread or
-- DM channel id — to a spore session, so a thread you replied in yesterday is
-- still that session after the daemon restarts. bridge namespaces the id,
-- because two bridges may hand out the same-looking string.
CREATE TABLE IF NOT EXISTS bridge_bindings (
  bridge      TEXT NOT NULL,
  external_id TEXT NOT NULL,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  created_at  TEXT NOT NULL,
  PRIMARY KEY (bridge, external_id)
);

-- bridge_seen deduplicates inbound events. A gateway that resumes redelivers,
-- and running a turn twice for one message is worse than dropping one.
CREATE TABLE IF NOT EXISTS bridge_seen (
  bridge     TEXT NOT NULL,
  event_id   TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (bridge, event_id)
);

CREATE INDEX IF NOT EXISTS idx_bridge_seen_age ON bridge_seen(created_at);
```

- [ ] **Step 4: Implement the methods**

Add to `internal/store/store.go`:

```go
// BindExternal points a bridge's own identifier at a session. Rebinding an
// identifier is legal and replaces the old target: the DM surface rebinds its
// rolling session every time /new is used.
func (s *Store) BindExternal(ctx context.Context, bridge, externalID, sessionID string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO bridge_bindings (bridge, external_id, session_id, created_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(bridge, external_id) DO UPDATE SET session_id = excluded.session_id`,
		bridge, externalID, sessionID, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("bind %s/%s: %w", bridge, externalID, err)
	}
	return nil
}

// SessionForExternal resolves a bridge identifier to its session.
func (s *Store) SessionForExternal(ctx context.Context, bridge, externalID string) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM bridge_bindings WHERE bridge = ? AND external_id = ?`,
		bridge, externalID).Scan(&id)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("resolve %s/%s: %w", bridge, externalID, err)
	}
	return id, true, nil
}

// MarkSeen records an inbound event id and reports whether this is the first
// time it has been presented. The insert is the test: making the claim and
// checking it one statement means two concurrent deliveries cannot both win.
func (s *Store) MarkSeen(ctx context.Context, bridge, eventID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO bridge_seen (bridge, event_id, created_at) VALUES (?, ?, ?)`,
		bridge, eventID, time.Now().UTC().Format(timeFormat))
	if err != nil {
		return false, fmt.Errorf("mark seen %s/%s: %w", bridge, eventID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// PruneSeen drops dedupe rows older than the window. The gateway only
// redelivers recent events, so this table is a short memory, not a log.
func (s *Store) PruneSeen(ctx context.Context, olderThan time.Duration) error {
	cutoff := time.Now().UTC().Add(-olderThan).Format(timeFormat)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM bridge_seen WHERE created_at <= ?`, cutoff); err != nil {
		return fmt.Errorf("prune seen: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store
git commit -m "feat(store): durable bridge bindings and inbound event dedupe"
```

---

### Task 4: Let the daemon start a turn under a chosen trust profile

`startTurn` hardcodes `policy.ProfileLocal` (`internal/daemon/sessions.go:145`). Nothing has ever set `ProfileRemote`, so the engine's profile support is untested in anger. The bridge needs it, and it needs an exported entry point that is not an HTTP handler.

**Files:**
- Modify: `internal/daemon/sessions.go`, `internal/daemon/server.go`
- Test: `internal/daemon/sessions_test.go` (create if absent)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
```go
// on *daemon.Server
func (s *Server) StartTurn(sessionID, text, client string, profile policy.Profile) error
func (s *Server) Store() *store.Store
func (s *Server) Guard() *policy.Guard
func (s *Server) Broker() *Broker
```
  `StartTurn` claims the session's turn slot itself and returns `ErrTurnRunning` when one is already in flight.

- [ ] **Step 1: Write the failing test**

```go
func TestStartTurnCarriesTheProfile(t *testing.T) {
	// The fake tool runner records the profile it saw on the context, which
	// is the only way to prove the bridge's turns are not running as local.
	seen := make(chan policy.Profile, 1)
	srv := newTestServerWithToolProbe(t, func(ctx context.Context) {
		_, p := policy.SessionFrom(ctx)
		seen <- p
	})
	sid := createTestSession(t, srv)

	if err := srv.StartTurn(sid, "hello", "discord", policy.ProfileRemote); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-seen:
		if p != policy.ProfileRemote {
			t.Fatalf("profile on the turn context = %q, want %q", p, policy.ProfileRemote)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the turn never ran a tool")
	}
}

func TestStartTurnRefusesASecondTurn(t *testing.T) {
	srv := newTestServerBlockingTurn(t)
	sid := createTestSession(t, srv)
	if err := srv.StartTurn(sid, "one", "discord", policy.ProfileRemote); err != nil {
		t.Fatal(err)
	}
	if err := srv.StartTurn(sid, "two", "discord", policy.ProfileRemote); !errors.Is(err, ErrTurnRunning) {
		t.Fatalf("second turn: err = %v, want ErrTurnRunning", err)
	}
}
```

Build the test servers with the scripted fake provider the daemon tests already use (see `internal/daemon/e2e_test.go` and `seam_test.go`). `newTestServerWithToolProbe` scripts a provider that emits one tool call; `newTestServerBlockingTurn` scripts one that blocks until the test releases it. Reuse the existing helpers wherever they already do this — do not build a second fake provider.

- [ ] **Step 2: Run to watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -run TestStartTurn -v`
Expected: FAIL to compile — `srv.StartTurn undefined`, `ErrTurnRunning undefined`.

- [ ] **Step 3: Parameterise `startTurn` and export an entry point**

In `internal/daemon/sessions.go`, change the signature and the one line that builds the context:

```go
// startTurn runs one turn on the SERVER's context and pumps its events into
// the hub. The caller must already hold the session's turn slot; startTurn
// releases it when the turn ends. The profile is the caller's trust level and
// decides which ruleset the policy engine applies — an HTTP client on
// loopback is local, a chat bridge is remote.
func (s *Server) startTurn(sessionID, text, client string, profile policy.Profile) error {
```

and

```go
	ctx := policy.WithSession(s.base, sessionID, profile)
```

Update the single existing caller in `handlePostMessage`:

```go
	if err := s.startTurn(id, text, "http", policy.ProfileLocal); err != nil {
```

Add the exported entry point at the end of the same file:

```go
// ErrTurnRunning reports that the session already has a turn in flight. Two
// clients posting at once must not interleave two turns into one transcript.
var ErrTurnRunning = errors.New("the session already has a turn running")

// StartTurn runs a turn for a non-HTTP client. It claims the session's turn
// slot, so callers must not call hub.Begin themselves. The turn runs on the
// server's context and outlives whatever started it (spec invariant 2), which
// is why no caller's context is accepted here.
func (s *Server) StartTurn(sessionID, text, client string, profile policy.Profile) error {
	if !s.hub.Begin(sessionID) {
		return ErrTurnRunning
	}
	if err := s.startTurn(sessionID, text, client, profile); err != nil {
		s.hub.End(sessionID)
		return err
	}
	return nil
}
```

Add `"errors"` to the file's imports.

- [ ] **Step 4: Expose the collaborators the bridge needs**

In `internal/daemon/server.go`, beside the existing `Hub()` accessor:

```go
// Store, Guard and Broker are the collaborators a bridge needs. They are
// accessors rather than constructor arguments because the daemon owns the
// approver the guard is built with, so a bridge cannot be wired before the
// server exists.
func (s *Server) Store() *store.Store    { return s.store }
func (s *Server) Guard() *policy.Guard   { return s.guard }
func (s *Server) Broker() *Broker        { return s.broker }
```

- [ ] **Step 5: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon/ -v`
Expected: PASS, including the pre-existing suite.

- [ ] **Step 6: Commit**

```bash
git add internal/daemon
git commit -m "feat(daemon): start turns under a caller-chosen trust profile"
```

---

### Task 5: The Discord client seam

Everything Discord-specific lives behind one interface so no test needs the network. This is the only file in the codebase that imports `discordgo`.

**Files:**
- Create: `internal/bridge/discord/client.go`, `internal/bridge/discord/fake_test.go`
- Modify: `go.mod`, `go.sum`
- Test: `internal/bridge/discord/client_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
```go
package discord

// Inbound is one message the gateway delivered, flattened to what the bridge
// needs. Every field is a Discord snowflake or plain text; nothing here is a
// discordgo type, so the rest of the package never sees the library.
type Inbound struct {
	MessageID string
	UserID    string
	Bot       bool
	GuildID   string // "" for a direct message
	ChannelID string
	ParentID  string // set when ChannelID is a thread
	Content   string
}

// Interaction is one component (button) press.
type Interaction struct {
	ID        string
	Token     string
	UserID    string
	GuildID   string
	ChannelID string
	ParentID  string
	CustomID  string // the button's identity; see approve.go
}

// Button is one message component the bridge asks the client to render.
type Button struct {
	CustomID string
	Label    string
	Danger   bool
}

// Embed is a boxed block beside a message's text. Tool calls render as embeds
// so a long transcript stays skimmable — the prose and the machinery are
// visually separate.
type Embed struct {
	Title       string
	Description string
	// Error tints the embed red. A failed tool call must be obvious at a
	// glance on a phone.
	Error bool
}

// Message is everything the bridge can put on screen at once.
type Message struct {
	Content string
	Embeds  []Embed
	Buttons []Button
}

type Client interface {
	// Open connects and starts delivering to the handlers, blocking until
	// the connection is established. It returns when connected, not when
	// closed.
	Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error
	Close() error
	Send(ctx context.Context, channelID string, m Message) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID string, m Message) error
	CreateThread(ctx context.Context, channelID, messageID, name string) (threadID string, err error)
	// Respond acknowledges an interaction with an ephemeral message. Discord
	// requires an acknowledgement within three seconds or the button shows
	// as failed, so this is called before any slow work.
	Respond(ctx context.Context, interactionID, token, content string) error
}
```

- [ ] **Step 1: Add the dependency**

```bash
go get github.com/bwmarrin/discordgo@latest
go mod tidy
```

Record the resolved version in the commit message. If `go get` cannot reach the network in this environment, stop and report that — do not vendor or stub the library.

- [ ] **Step 2: Write `client.go`**

Declare the types above, then the adapter. Its job is translation only; no policy, no session logic.

```go
// Package discord bridges spore to one Discord bot. It is a CLIENT of the
// daemon — it subscribes to the hub, starts turns through the server, and
// answers approvals through the broker and guard — so the session-ownership
// check that stops a remote session answering a local one's approval applies
// to it unchanged. It is deliberately not a policy.Approver.
package discord

// gatewayClient is the real Client, over discordgo. It is the only place in
// spore that knows the library exists; everything above it is testable
// offline against a fake.
type gatewayClient struct {
	sess *discordgo.Session
}

// NewGatewayClient dials nothing yet; Open does that.
func NewGatewayClient(token string) (Client, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("build discord session: %w", err)
	}
	// MessageContent is a privileged intent and must also be enabled in the
	// bot's settings in Discord's developer portal. Without it, message text
	// arrives empty in guilds and the bridge silently does nothing.
	s.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent
	return &gatewayClient{sess: s}, nil
}
```

Implement each method by translating to and from `discordgo`:

- `Open` registers `AddHandler` callbacks for `*discordgo.MessageCreate` and `*discordgo.InteractionCreate`, converts them to `Inbound` / `Interaction`, and calls `s.sess.Open()`. For a message, set `ParentID` by looking up the channel: `ch, err := s.sess.State.Channel(m.ChannelID)`, falling back to `s.sess.Channel(m.ChannelID)`, and use `ch.ParentID` when `ch.IsThread()`. Set `Bot` from `m.Author.Bot`.
- `Send` builds `&discordgo.MessageSend{Content: m.Content, Embeds: embedsFor(m.Embeds), Components: componentsFor(m.Buttons)}`.
- `Edit` builds `&discordgo.MessageEdit{Channel: ..., ID: ..., Content: &m.Content, Embeds: &embeds, Components: &comps}`.
- `embedsFor` maps each `Embed` to a `*discordgo.MessageEmbed` with `Title`, `Description`, and `Color` `0xCC3333` when `Error` else `0x5865F2`. Truncate `Description` to 4096 characters, Discord's limit.
- `CreateThread` calls `MessageThreadStartComplex`. Truncate `name` to 100 characters — Discord rejects longer ones.
- `Respond` calls `InteractionRespond` with `discordgo.InteractionResponseChannelMessageWithSource` and `Flags: discordgo.MessageFlagsEphemeral`.
- `componentsFor` maps `[]Button` to one `discordgo.ActionsRow`, `ButtonStyle` `DangerButton` when `Danger` else `SecondaryButton`. Discord allows at most five buttons per row; split into rows of five, and cap at five rows.

- [ ] **Step 3: Write the fake used by every later test**

`internal/bridge/discord/fake_test.go`:

```go
// fakeClient records what the bridge asked Discord to do and lets a test
// deliver gateway events by hand. Every test in this package uses it; there
// is no other Client implementation under test.
type fakeClient struct {
	mu sync.Mutex

	sent     []sentMessage
	edits    []sentMessage
	threads  []createdThread
	responds []respondCall
	nextID   int

	onMessage     func(Inbound)
	onInteraction func(Interaction)

	// failNext makes the next call of the named method return an error, so
	// tests can exercise the bridge's error paths.
	failNext map[string]error
}

type sentMessage struct {
	ChannelID string
	MessageID string
	Message   Message
}

type createdThread struct{ ChannelID, MessageID, Name, ThreadID string }
type respondCall struct{ InteractionID, Token, Content string }

func newFakeClient() *fakeClient { return &fakeClient{failNext: map[string]error{}} }

func (f *fakeClient) Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("Open"); err != nil {
		return err
	}
	f.onMessage, f.onInteraction = onMessage, onInteraction
	return nil
}

// deliver feeds one gateway message to the bridge, as the real gateway would.
func (f *fakeClient) deliver(in Inbound) {
	f.mu.Lock()
	h := f.onMessage
	f.mu.Unlock()
	if h != nil {
		h(in)
	}
}

// press feeds one button press to the bridge.
func (f *fakeClient) press(i Interaction) {
	f.mu.Lock()
	h := f.onInteraction
	f.mu.Unlock()
	if h != nil {
		h(i)
	}
}
```

Implement `Close`, `Send`, `Edit`, `CreateThread`, `Respond` to append to the slices under the mutex and return generated ids (`fmt.Sprintf("m%d", f.nextID)`), honouring `failNext`. Add accessor helpers `f.sentTo(channelID) []sentMessage` and `f.lastEdit(channelID) (sentMessage, bool)` that copy under the lock — tests must never touch the slices directly, because the render goroutine writes to them concurrently.

- [ ] **Step 4: Write a test that pins the seam**

`internal/bridge/discord/client_test.go`:

```go
func TestFakeClientSatisfiesClient(t *testing.T) {
	// A compile-time assertion with a runtime home, so the failure is a test
	// failure rather than a confusing error in an unrelated file.
	var _ Client = newFakeClient()
}

func TestGatewayClientSatisfiesClient(t *testing.T) {
	// NewGatewayClient must not dial: it is called during daemon startup and
	// a network round trip there would make startup fail on a flaky link
	// rather than on a bad token.
	c, err := NewGatewayClient("not-a-real-token")
	if err != nil {
		t.Fatal(err)
	}
	if c == nil {
		t.Fatal("nil client")
	}
}
```

- [ ] **Step 5: Run**

Run: `go test -tags sqlite_fts5 ./internal/bridge/... -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/bridge
git commit -m "feat(bridge): add the Discord client seam and its test fake"
```

---

### Task 6: Admission

The one place membership is decided. Pure, table-tested, and applied identically to guild messages, DMs and button presses so the surfaces cannot drift apart.

**Files:**
- Create: `internal/bridge/discord/admit.go`, `internal/bridge/discord/admit_test.go`

**Interfaces:**
- Consumes: `config.DiscordConfig` (Task 2), `Inbound` / `Interaction` (Task 5).
- Produces:
```go
type Admitter struct{ /* unexported */ }
func NewAdmitter(cfg config.DiscordConfig) Admitter
func (a Admitter) AdmitMessage(in Inbound) bool
func (a Admitter) AdmitInteraction(i Interaction) bool
```

- [ ] **Step 1: Write the failing table test**

```go
func TestAdmitMessage(t *testing.T) {
	cfg := config.DiscordConfig{
		Enabled: true, Token: "t",
		GuildID: "G", ChannelIDs: []string{"C1", "C2"}, UserIDs: []string{"U"},
		AllowDMs: true,
	}
	a := NewAdmitter(cfg)

	cases := []struct {
		name string
		in   Inbound
		want bool
	}{
		{"allowlisted user in an allowlisted channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "C1"}, true},
		{"a thread is admitted on its parent channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "T9", ParentID: "C2"}, true},
		{"a stranger in an allowlisted channel",
			Inbound{UserID: "STRANGER", GuildID: "G", ChannelID: "C1"}, false},
		{"the allowlisted user in another channel",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "C9"}, false},
		{"the allowlisted user in another guild",
			Inbound{UserID: "U", GuildID: "OTHER", ChannelID: "C1"}, false},
		{"a thread whose parent is not allowlisted",
			Inbound{UserID: "U", GuildID: "G", ChannelID: "T9", ParentID: "C9"}, false},
		{"a DM from the allowlisted user",
			Inbound{UserID: "U", GuildID: "", ChannelID: "DM1"}, true},
		{"a DM from a stranger",
			Inbound{UserID: "STRANGER", GuildID: "", ChannelID: "DM2"}, false},
		// A bot echo is how a bridge talks to itself forever. The bot's own
		// messages come back over the gateway, so this is not hypothetical.
		{"a bot, even the allowlisted user id",
			Inbound{UserID: "U", Bot: true, GuildID: "G", ChannelID: "C1"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.AdmitMessage(tc.in); got != tc.want {
				t.Fatalf("AdmitMessage(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestAdmitDMsOffByDefault(t *testing.T) {
	a := NewAdmitter(config.DiscordConfig{
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}, AllowDMs: false,
	})
	if a.AdmitMessage(Inbound{UserID: "U", ChannelID: "DM1"}) {
		t.Fatal("a DM was admitted with allow_dms = false")
	}
}

func TestAdmitInteractionUsesTheSameRules(t *testing.T) {
	// The button press is a second entrance to the same house. If it were
	// checked differently from the message path, an attacker who could not
	// send a prompt could still answer an approval.
	cfg := config.DiscordConfig{
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}, AllowDMs: true,
	}
	a := NewAdmitter(cfg)

	if !a.AdmitInteraction(Interaction{UserID: "U", GuildID: "G", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("the allowlisted user's press in an allowlisted thread was rejected")
	}
	if a.AdmitInteraction(Interaction{UserID: "STRANGER", GuildID: "G", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("a stranger's button press was admitted")
	}
	if a.AdmitInteraction(Interaction{UserID: "U", GuildID: "OTHER", ChannelID: "T1", ParentID: "C1"}) {
		t.Fatal("a press from another guild was admitted")
	}
}

func TestAdmitEmptyAllowlistAdmitsNobody(t *testing.T) {
	// config.Load rejects this shape, but the zero value must still fail
	// closed: a future caller that builds the struct directly gets the safe
	// behaviour, not an open door.
	a := NewAdmitter(config.DiscordConfig{})
	if a.AdmitMessage(Inbound{UserID: "U", ChannelID: "C1"}) {
		t.Fatal("the zero-value admitter admitted a message")
	}
	if a.AdmitInteraction(Interaction{UserID: "U", ChannelID: "C1"}) {
		t.Fatal("the zero-value admitter admitted an interaction")
	}
}
```

- [ ] **Step 2: Run to watch it fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run Admit -v`
Expected: FAIL to compile — `NewAdmitter undefined`.

- [ ] **Step 3: Implement**

```go
// Admitter decides who may reach the agent through Discord. It is the whole
// trust boundary of the bridge, which is why it is one small pure type with
// no I/O: it can be read in full and table-tested exhaustively.
//
// Guild membership is the outer boundary — a Discord bot exists only in
// servers it has been invited to — and the user allowlist is the inner one.
// Both apply to every surface. Anything not admitted is dropped without a
// reply: answering would confirm the bot exists to whoever probed it.
type Admitter struct {
	guildID  string
	channels map[string]struct{}
	users    map[string]struct{}
	allowDMs bool
}

func NewAdmitter(cfg config.DiscordConfig) Admitter {
	a := Admitter{
		guildID:  cfg.GuildID,
		channels: make(map[string]struct{}, len(cfg.ChannelIDs)),
		users:    make(map[string]struct{}, len(cfg.UserIDs)),
		allowDMs: cfg.AllowDMs,
	}
	for _, c := range cfg.ChannelIDs {
		a.channels[c] = struct{}{}
	}
	for _, u := range cfg.UserIDs {
		a.users[u] = struct{}{}
	}
	return a
}

// AdmitMessage reports whether an inbound message may start or continue a
// session.
func (a Admitter) AdmitMessage(in Inbound) bool {
	// A bot's own messages come back over the gateway. Replying to them is
	// how a bridge talks to itself until the rate limiter stops it.
	if in.Bot {
		return false
	}
	return a.admit(in.UserID, in.GuildID, in.ChannelID, in.ParentID)
}

// AdmitInteraction reports whether a button press may be acted on. It applies
// exactly the rules AdmitMessage applies: a press is a second entrance to the
// same house, and an approval answered by someone who could not have sent the
// prompt would be the worst kind of hole.
func (a Admitter) AdmitInteraction(i Interaction) bool {
	return a.admit(i.UserID, i.GuildID, i.ChannelID, i.ParentID)
}

func (a Admitter) admit(userID, guildID, channelID, parentID string) bool {
	if _, ok := a.users[userID]; !ok {
		return false
	}
	if guildID == "" {
		return a.allowDMs
	}
	if a.guildID == "" || guildID != a.guildID {
		return false
	}
	// A thread is admitted on its parent, so threads the bridge opens itself
	// need no entry in the config.
	key := channelID
	if parentID != "" {
		key = parentID
	}
	_, ok := a.channels[key]
	return ok
}
```

- [ ] **Step 4: Run**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -v`
Expected: PASS.

- [ ] **Step 5: Mutation-test the boundary**

```bash
# In admit(), delete the `if _, ok := a.users[userID]; !ok { return false }` guard.
go test -tags sqlite_fts5 ./internal/bridge/discord/ -run Admit -v
# EXPECTED: the "a stranger in an allowlisted channel", "a DM from a stranger"
# and "a stranger's button press" cases FAIL. Revert and re-run to confirm PASS.
```

Report which cases failed.

- [ ] **Step 6: Commit**

```bash
git add internal/bridge/discord
git commit -m "feat(bridge): admit only allowlisted users on every Discord surface"
```

---

### Task 7: Render a turn into Discord

A turn emits many small text deltas. Posting one Discord message per delta would be unreadable and would hit the rate limiter immediately, so the renderer accumulates text and edits a single message on a throttle, starting a new message when the 2000-character limit is reached.

**Files:**
- Create: `internal/bridge/discord/render.go`, `internal/bridge/discord/render_test.go`

**Interfaces:**
- Consumes: `Client`, `Message`, `Embed` (Task 5); `daemon.WireEvent` (existing).
- Produces:
```go
const messageLimit = 2000

type renderer struct{ /* unexported */ }

// newRenderer renders one session's events into one Discord channel.
func newRenderer(c Client, channelID string, throttle time.Duration) *renderer

// Consume drains events until the channel closes or ctx is done, then flushes
// whatever is left. It is the renderer's whole public surface.
func (r *renderer) Consume(ctx context.Context, events <-chan daemon.WireEvent)

// onApproval is set by Task 8 to render an approval prompt. Left nil, an
// approval event is rendered as plain text.
func (r *renderer) onApproval(func(daemon.WireEvent))
```

- [ ] **Step 1: Write the failing tests**

```go
func drain(t *testing.T, r *renderer, evs ...daemon.WireEvent) {
	t.Helper()
	ch := make(chan daemon.WireEvent, len(evs))
	for _, e := range evs {
		ch <- e
	}
	close(ch)
	r.Consume(context.Background(), ch)
}

func TestRendererCoalescesTextIntoOneMessage(t *testing.T) {
	f := newFakeClient()
	// A zero throttle makes every flush immediate, so the test is not timing
	// dependent; coalescing is proved by the event loop, not by the clock.
	r := newRenderer(f, "C1", 0)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "Hello, "},
		daemon.WireEvent{Type: daemon.WireText, Text: "world."},
		daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"},
	)

	sent := f.sentTo("C1")
	if len(sent) != 1 {
		t.Fatalf("sent %d messages, want 1: %+v", len(sent), sent)
	}
	last, _ := f.lastEdit("C1")
	final := last.Message.Content
	if final == "" {
		final = sent[0].Message.Content
	}
	if final != "Hello, world." {
		t.Fatalf("final content = %q, want %q", final, "Hello, world.")
	}
}

func TestRendererStartsANewMessageAtTheLimit(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	// 1500 + 900 characters cannot fit in one 2000-character message.
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("a", 1500)},
		daemon.WireEvent{Type: daemon.WireText, Text: strings.Repeat("b", 900)},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	sent := f.sentTo("C1")
	if len(sent) != 2 {
		t.Fatalf("sent %d messages, want 2", len(sent))
	}
	for i, m := range sent {
		if len(m.Message.Content) > messageLimit {
			t.Fatalf("message %d is %d characters, over the %d limit", i, len(m.Message.Content), messageLimit)
		}
	}
	// Nothing may be lost at the seam.
	var all strings.Builder
	for _, m := range f.finalContents("C1") {
		all.WriteString(m)
	}
	if got := all.String(); got != strings.Repeat("a", 1500)+strings.Repeat("b", 900) {
		t.Fatalf("content lost at the message boundary: got %d characters, want 2400", len(got))
	}
}

func TestRendererShowsToolCallsAsEmbeds(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)

	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "checking"},
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "fs_read", Args: `{"path":"/w/a.go"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "package main"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)

	var embeds []Embed
	for _, m := range append(f.sentTo("C1"), f.editsTo("C1")...) {
		embeds = append(embeds, m.Message.Embeds...)
	}
	if len(embeds) == 0 {
		t.Fatal("the tool call produced no embed")
	}
	found := false
	for _, e := range embeds {
		if strings.Contains(e.Title, "fs_read") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no embed names the tool: %+v", embeds)
	}
}

func TestRendererMarksAFailedToolCall(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "shell_exec", Args: `{"cmd":"nope"}`},
		daemon.WireEvent{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "not found", IsError: true},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)
	for _, m := range append(f.sentTo("C1"), f.editsTo("C1")...) {
		for _, e := range m.Message.Embeds {
			if e.Error {
				return
			}
		}
	}
	t.Fatal("a failed tool call was not marked as an error")
}

func TestRendererReportsATurnError(t *testing.T) {
	f := newFakeClient()
	r := newRenderer(f, "C1", 0)
	drain(t, r, daemon.WireEvent{Type: daemon.WireError, Error: "provider exploded"})

	for _, c := range f.finalContents("C1") {
		if strings.Contains(c, "provider exploded") {
			return
		}
	}
	t.Fatal("the turn error never reached Discord; a silent failure on a phone is indistinguishable from a hang")
}

func TestRendererSurvivesASendFailure(t *testing.T) {
	// Discord is a network. A failed edit must not kill the goroutine
	// draining the hub, or the session stops receiving events forever.
	f := newFakeClient()
	f.failNext["Send"] = errors.New("rate limited")
	r := newRenderer(f, "C1", 0)
	drain(t, r,
		daemon.WireEvent{Type: daemon.WireText, Text: "first"},
		daemon.WireEvent{Type: daemon.WireText, Text: "second"},
		daemon.WireEvent{Type: daemon.WireTurnDone},
	)
	// The point is that Consume returned rather than panicked or blocked.
}
```

Add `finalContents(channelID) []string` and `editsTo(channelID) []sentMessage` to `fakeClient`: `finalContents` returns, per message id in send order, the content of its latest edit if any, else the content it was sent with.

- [ ] **Step 2: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run Renderer -v`
Expected: FAIL to compile — `newRenderer undefined`.

- [ ] **Step 3: Implement the renderer**

```go
// messageLimit is Discord's hard cap on a message's content.
const messageLimit = 2000

// defaultThrottle bounds how often a streaming turn edits its message.
// Discord's rate limits are per channel and unforgiving; a turn that emits a
// hundred deltas a second must still only edit a few times a second.
const defaultThrottle = 1500 * time.Millisecond

// renderer turns one session's event stream into Discord messages. A turn
// emits many small text deltas, so it accumulates them and edits a single
// message on a throttle rather than posting per delta — which would be both
// unreadable and instantly rate limited.
//
// Every Discord call's error is logged and swallowed. Discord is a network:
// a failed edit must never stop the goroutine draining the hub, because a
// stalled drain means the session silently stops updating.
type renderer struct {
	client    Client
	channelID string
	throttle  time.Duration

	// buf is the text not yet on screen; msgID is the message it belongs to,
	// empty when the next flush must Send rather than Edit.
	buf      strings.Builder
	msgID    string
	onScreen int // characters already committed to msgID

	pendingCalls map[string]string // tool_use_id -> tool name
	approvalFn   func(daemon.WireEvent)

	// stopAfterTurn makes Consume return once the turn it was started for
	// ends. The bridge sets it: one goroutine per turn, ending with the turn,
	// so a long-lived session does not accumulate one renderer per prompt.
	stopAfterTurn bool
}
```

A throttle of zero or less means "flush on every event", which is what the
tests use so they never wait on a clock. `newRenderer` applies no default of
its own — the caller decides.

`Consume` is a `select` over the event channel, a `time.Ticker` at `throttle` (skip the ticker entirely when `throttle <= 0` and flush on every event), and `ctx.Done()`. On each event:

- `WireText`: append to `buf`; flush immediately if `throttle <= 0` or if `onScreen+buf.Len() >= messageLimit`.
- `WireToolCall`: `flush()`, remember `pendingCalls[ToolUseID] = Tool`, and send an embed titled `"⚙ " + Tool` whose description is the arguments in a fenced code block, truncated to 1000 characters. Starting a fresh text message after an embed is correct — the embed is a boundary in the transcript.
- `WireToolResult`: send an embed titled `"↳ " + pendingCalls[ToolUseID]` with the result truncated to 1000 characters and `Error: ev.IsError`; delete the map entry.
- `WireApproval`: `flush()`, then `r.approvalFn(ev)` if set, else render the tool and rule as text.
- `WireResolved`: append a one-line note (`"→ " + ev.Decision`) as text.
- `WireTurnDone`: `flush()`, reset `msgID`/`onScreen` so the next turn starts a new message, and `return` when `stopAfterTurn` is set.
- `WireError`: `flush()`, send an error embed titled `"turn failed"`, and `return` when `stopAfterTurn` is set — an error ends the turn too, and a goroutine that waited for a `turn_done` that will never come would leak.

After the loop ends (channel closed or ctx done), call `flush()` one final time.

`flush()`:

```go
// flush puts the buffered text on screen, splitting at the message limit.
// Splitting prefers the last newline in the overflowing chunk so a code block
// or paragraph is not cut mid-line when there is a reasonable place to cut.
func (r *renderer) flush(ctx context.Context) {
	for r.buf.Len() > 0 {
		room := messageLimit - r.onScreen
		if r.msgID == "" {
			room = messageLimit
		}
		text := r.buf.String()
		if len(text) <= room {
			r.write(ctx, text)
			r.buf.Reset()
			return
		}
		head, tail := splitAt(text, room)
		r.write(ctx, head)
		// Whatever did not fit belongs to a new message.
		r.msgID, r.onScreen = "", 0
		r.buf.Reset()
		r.buf.WriteString(tail)
	}
}
```

`write` sends when `msgID` is empty and edits otherwise, recording the new `msgID` and adding to `onScreen`; on error it logs with `slog.Warn("discord render", "err", err)` and returns without changing state. `splitAt(s string, n int)` cuts at the last `'\n'` at or before `n` when one exists past `n/2`, else exactly at `n`, and must be rune-safe — never split inside a multi-byte rune.

Add `func (r *renderer) onApproval(fn func(daemon.WireEvent)) { r.approvalFn = fn }`.

- [ ] **Step 4: Run**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -v`
Expected: PASS.

- [ ] **Step 5: Run with the race detector**

The renderer runs in its own goroutine while the fake's slices are read by the test.

Run: `go test -tags sqlite_fts5 -race ./internal/bridge/discord/ -v`
Expected: PASS with no race report.

- [ ] **Step 6: Commit**

```bash
git add internal/bridge/discord
git commit -m "feat(bridge): render a turn's events into throttled Discord messages"
```

---

### Task 8: Approvals as message components

An approval prompt becomes buttons. The answer goes through the broker and the guard — never around them — so the session-ownership check applies.

**Files:**
- Create: `internal/bridge/discord/approve.go`, `internal/bridge/discord/approve_test.go`

**Interfaces:**
- Consumes: `Client`, `Button`, `Message`, `Interaction` (Task 5); `Admitter` (Task 6); `daemon.WireEvent`, `daemon.Broker`, `policy.Guard` (Tasks 1 and 4).
- Produces:
```go
// customID encodes which approval a button answers, because Discord hands
// back only this string when the button is pressed.
func encodeCustomID(sessionID string, pendingID int64, allow bool, scope policy.Scope) string
func decodeCustomID(s string) (sessionID string, pendingID int64, ans policy.Answer, err error)

// approvalMessage renders an approval request as a message with buttons.
func approvalMessage(sessionID string, ev daemon.WireEvent) Message

// answerer resolves an approval on behalf of a Discord button press.
type answerer struct{ /* unexported */ }
func newAnswerer(broker *daemon.Broker, guard *policy.Guard) *answerer
func (a *answerer) answer(ctx context.Context, sessionID string, pendingID int64, ans policy.Answer) (string, error)
```
  `answer` returns the short human-readable outcome to show in the ephemeral reply.

- [ ] **Step 1: Write the failing tests**

```go
func TestCustomIDRoundTrip(t *testing.T) {
	cases := []policy.Answer{
		{Allow: true, Scope: policy.ScopeOnce},
		{Allow: false, Scope: policy.ScopeOnce},
		{Allow: true, Scope: policy.ScopeSession},
		{Allow: true, Scope: policy.ScopePattern},
	}
	for _, want := range cases {
		id := encodeCustomID("sess-1", 42, want.Allow, want.Scope)
		if len(id) > 100 {
			t.Fatalf("custom id is %d characters; Discord's limit is 100", len(id))
		}
		sid, pid, got, err := decodeCustomID(id)
		if err != nil {
			t.Fatal(err)
		}
		if sid != "sess-1" || pid != 42 || got != want {
			t.Fatalf("round trip: (%q, %d, %+v), want (sess-1, 42, %+v)", sid, pid, got, want)
		}
	}
}

func TestDecodeCustomIDRejectsGarbage(t *testing.T) {
	// The custom id arrives from Discord and is therefore untrusted input.
	for _, bad := range []string{"", "nonsense", "a|b|c", "spore|sess|notanumber|allow|once", "other|sess|1|allow|once"} {
		if _, _, _, err := decodeCustomID(bad); err == nil {
			t.Fatalf("decodeCustomID(%q) succeeded, want an error", bad)
		}
	}
}

func TestApprovalMessageOffersThePatternOptionOnlyWhenThereIsOne(t *testing.T) {
	withPattern := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 7, Tool: "fs_write",
		Args: `{"path":"/w/a.go"}`, Rule: "fs_write",
		Pattern: "fs_write(path matches /w/**)",
	})
	if len(withPattern.Buttons) != 4 {
		t.Fatalf("got %d buttons, want 4 (once, deny, session, pattern)", len(withPattern.Buttons))
	}

	// The event the guard sends for a call with no derivable pattern. The
	// button must be absent, not merely relabelled: this is a one-tap control
	// on a phone that would otherwise write a blanket allow for the tool.
	degraded := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 8, Tool: "shell_exec",
		Args: `{"cmd":"ls"}`, Rule: "shell_exec", Pattern: "",
	})
	if len(degraded.Buttons) != 3 {
		t.Fatalf("got %d buttons, want 3 (once, deny, session)", len(degraded.Buttons))
	}
	for _, b := range degraded.Buttons {
		if _, _, ans, err := decodeCustomID(b.CustomID); err == nil && ans.Scope == policy.ScopePattern {
			t.Fatalf("a pattern button was offered for a call with no pattern: %+v", b)
		}
	}
}

func TestApprovalMessageLabelsTheSessionScopeHonestly(t *testing.T) {
	// "session" approves the TOOL for the rest of the session, not these
	// arguments. A vaguer label would understate what the tap does.
	m := approvalMessage("s1", daemon.WireEvent{
		Type: daemon.WireApproval, PendingID: 7, Tool: "shell_exec", Rule: "shell_exec",
	})
	found := false
	for _, b := range m.Buttons {
		if strings.Contains(b.Label, "shell_exec") && strings.Contains(b.Label, "session") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no button names the tool and the session scope: %+v", m.Buttons)
	}
}

func TestApprovalMessageMarksDenyAsDanger(t *testing.T) {
	m := approvalMessage("s1", daemon.WireEvent{Type: daemon.WireApproval, PendingID: 7, Tool: "fs_write"})
	for _, b := range m.Buttons {
		if _, _, ans, err := decodeCustomID(b.CustomID); err == nil && !ans.Allow {
			if !b.Danger {
				t.Fatal("the deny button is not marked as danger")
			}
			return
		}
	}
	t.Fatal("no deny button")
}
```

- [ ] **Step 2: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run 'CustomID|ApprovalMessage' -v`
Expected: FAIL to compile.

- [ ] **Step 3: Implement `approve.go`**

```go
// customIDPrefix namespaces our components. Discord hands the custom id back
// verbatim on a press and nothing else, so it is the entire message from the
// button to the handler — and it arrives from the network, so it is parsed as
// untrusted input rather than trusted because we wrote it.
const customIDPrefix = "spore"

// encodeCustomID packs the answer into Discord's 100-character custom id.
func encodeCustomID(sessionID string, pendingID int64, allow bool, scope policy.Scope) string {
	verdict := "deny"
	if allow {
		verdict = "allow"
	}
	return strings.Join([]string{customIDPrefix, sessionID, strconv.FormatInt(pendingID, 10), verdict, string(scope)}, "|")
}

// decodeCustomID parses a custom id from a button press. An unknown prefix,
// a bad number, or an unknown scope is an error, never a default: a scope
// that silently became "once" would be confusing, and one that silently
// became "pattern" would be a hole.
func decodeCustomID(s string) (string, int64, policy.Answer, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 5 || parts[0] != customIDPrefix {
		return "", 0, policy.Answer{}, fmt.Errorf("not a spore component id: %q", s)
	}
	pendingID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, policy.Answer{}, fmt.Errorf("bad pending id in %q: %w", s, err)
	}
	var allow bool
	switch parts[3] {
	case "allow":
		allow = true
	case "deny":
	default:
		return "", 0, policy.Answer{}, fmt.Errorf("bad verdict in %q", s)
	}
	scope := policy.Scope(parts[4])
	switch scope {
	case policy.ScopeOnce, policy.ScopeSession, policy.ScopePattern:
	default:
		return "", 0, policy.Answer{}, fmt.Errorf("bad scope in %q", s)
	}
	if parts[1] == "" {
		return "", 0, policy.Answer{}, fmt.Errorf("empty session in %q", s)
	}
	return parts[1], pendingID, policy.Answer{Allow: allow, Scope: scope}, nil
}
```

`approvalMessage` builds the content (`"**spore wants to run " + ev.Tool + "**"` plus `matched policy rule …` when `ev.Rule` is set), one embed holding the arguments in a fenced code block, and the buttons:

```go
	buttons := []Button{
		{CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopeOnce), Label: "allow once"},
		{CustomID: encodeCustomID(sessionID, ev.PendingID, false, policy.ScopeOnce), Label: "deny", Danger: true},
		// Say what this actually does. It approves the TOOL for the rest of
		// the session, not these arguments.
		{CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopeSession),
			Label: truncateLabel("allow " + ev.Tool + " for this session")},
	}
	// An empty pattern is the guard saying there is nothing to generalise to.
	// Offering the button anyway would put a one-tap blanket allow for the
	// whole tool on a phone screen.
	if ev.Pattern != "" {
		buttons = append(buttons, Button{
			CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopePattern),
			Label:    truncateLabel("always allow " + ev.Pattern),
		})
	}
```

`truncateLabel` caps at 80 characters, Discord's button-label limit, with an ellipsis.

`answerer.answer` mirrors the daemon's `handleResolveApproval` exactly — the live-waiter path first, then the already-answered check, then `Guard.Resolve`:

```go
// answer delivers a Discord button press to the suspended turn. It takes the
// same two paths the HTTP handler takes and in the same order: a live waiter
// is the normal case, and Guard.Resolve is only for a suspension whose turn
// is gone. Taking both would write two audit rows for one decision.
//
// The sessionID comes from the button, and both Broker.Answer and
// Guard.Resolve verify it against the suspension's own session — which is
// what stops one session answering another's approvals.
func (a *answerer) answer(ctx context.Context, sessionID string, pendingID int64, ans policy.Answer) (string, error) {
	if a.broker.Answer(sessionID, pendingID, ans) {
		return verdictText(ans), nil
	}
	if a.broker.AlreadyAnswered(pendingID) {
		return "", fmt.Errorf("that approval was already answered")
	}
	if a.guard == nil {
		return "", fmt.Errorf("no approval %d is waiting", pendingID)
	}
	if err := a.guard.Resolve(ctx, sessionID, pendingID, ans); err != nil {
		return "", err
	}
	return verdictText(ans) + " (recorded after a restart)", nil
}
```

`verdictText` renders e.g. `"allowed once"`, `"denied"`, `"allowed shell_exec for this session"` — keep it short; it goes in an ephemeral reply.

- [ ] **Step 4: Write the failing test for the ownership check**

This is the test that proves the bridge cannot be used to answer another session's approval. It needs a real store and guard, so build the config through `config.Load` on a temp TOML.

```go
func TestAnswererRefusesAnotherSessionsApproval(t *testing.T) {
	ctx := context.Background()
	// config.Load, never config.Default: Load is what appends the baseline
	// deny rules, and a guard without them proves nothing.
	st, guard := newLoadedGuard(t)
	victim, _ := st.CreateSession(ctx, "victim")
	attacker, _ := st.CreateSession(ctx, "attacker")

	pendingID, err := st.AddPendingCall(ctx, store.PendingCall{
		SessionID: victim, ToolUseID: "tu1", Tool: "shell_exec",
		ArgsJSON: []byte(`{"cmd":"ls"}`), Profile: "local", Rule: "shell_exec",
	})
	if err != nil {
		t.Fatal(err)
	}

	a := newAnswerer(daemon.NewBroker(daemon.NewHub()), guard)

	// The attacker's session forges the victim's pending id.
	if _, err := a.answer(ctx, attacker, pendingID, policy.Answer{Allow: true, Scope: policy.ScopeOnce}); err == nil {
		t.Fatal("one session answered another session's approval")
	}
	// And the suspension is still open for its real owner.
	pending, err := guard.Pending(ctx, victim)
	if err != nil || len(pending) != 1 {
		t.Fatalf("the victim's suspension was consumed: %d pending, err %v", len(pending), err)
	}
}
```

`newLoadedGuard` writes a temp `config.toml` with `[policy] workspace`, `default = "ask"` and `ask = ["shell_exec"]`, calls `config.Load`, opens a temp store, and builds the guard the way `cmd/spore/wire.go` does. Put it in this package's test files.

- [ ] **Step 5: Run**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/bridge/discord
git commit -m "feat(bridge): answer approvals from Discord buttons through the guard"
```

---

### Task 9: The bridge — messages in, sessions out

Route an admitted message to a session, opening a thread for a new one; deduplicate redelivered events; handle `/new`; wire the renderer and the answerer together.

**Files:**
- Create: `internal/bridge/discord/bridge.go`, `internal/bridge/discord/bridge_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–8.
- Produces:
```go
// bridgeName is the namespace used for every store binding and dedupe row.
const bridgeName = "discord"

// Turns is what the bridge needs from the daemon. It is an interface rather
// than *daemon.Server so the bridge's tests need no HTTP server, and so the
// dependency is legible: the bridge starts turns and watches events, and can
// do nothing else to the daemon.
type Turns interface {
	StartTurn(sessionID, text, client string, profile policy.Profile) error
	Subscribe(sessionID string) (<-chan daemon.WireEvent, func())
}

type Options struct {
	Cfg    config.DiscordConfig
	Client Client
	Turns  Turns
	Store  *store.Store
	Broker *daemon.Broker
	Guard  *policy.Guard
	// Throttle overrides the render throttle. Zero means defaultThrottle
	// (New substitutes it); a negative value means "flush on every event"
	// and is what tests pass so they never wait on a clock.
	Throttle time.Duration
}

type Bridge struct{ /* unexported */ }

func New(o Options) (*Bridge, error)

// Start opens the gateway and returns. Events arrive on the client's
// goroutines from then on.
func (b *Bridge) Start(ctx context.Context) error
func (b *Bridge) Close() error
```
  `daemon.Server` satisfies `Turns` already: it has `StartTurn` from Task 4, and `Subscribe` is added here as a thin forward to `s.hub.Subscribe`.

- [ ] **Step 1: Add `Subscribe` to the daemon server**

In `internal/daemon/server.go`:

```go
// Subscribe attaches a non-HTTP client to a session's event stream. It is the
// same subscription the SSE handler uses; a bridge is not a special case.
func (s *Server) Subscribe(sessionID string) (<-chan WireEvent, func()) {
	return s.hub.Subscribe(sessionID)
}
```

- [ ] **Step 2: Write the failing tests**

```go
// testDiscordConfig is the allowlist every bridge test is built on: one
// guild, one channel, one user, DMs open.
func testDiscordConfig() config.DiscordConfig {
	return config.DiscordConfig{
		Enabled: true, Token: "test-token",
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"},
		AllowDMs: true,
	}
}

// newBridgeOver builds a bridge on a caller-supplied client and a fresh
// store. Supervise's tests use it; they care about the connection, not the
// routing.
func newBridgeOver(t *testing.T, f *fakeClient) *Bridge {
	t.Helper()
	b, _, _ := bridgeWithStore(t, f, openTestStore(t))
	return b
}

// newTestBridge wires a bridge over the fake client and a fake Turns, with a
// REAL store so bindings and dedupe are exercised for real rather than
// against a map that cannot survive a restart.
func newTestBridge(t *testing.T) (*Bridge, *fakeClient, *fakeTurns, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	f := newFakeClient()
	b, turns, _ := bridgeWithStore(t, f, st)
	return b, f, turns, st
}

// restartBridge builds a second bridge over the SAME store and a fresh
// client, which is what a daemon restart looks like from the store's side.
func restartBridge(t *testing.T, st *store.Store) (*Bridge, *fakeClient, *fakeTurns) {
	t.Helper()
	f := newFakeClient()
	b, turns, _ := bridgeWithStore(t, f, st)
	return b, f, turns
}

func bridgeWithStore(t *testing.T, f *fakeClient, st *store.Store) (*Bridge, *fakeTurns, *daemon.Broker) {
	t.Helper()
	turns := newFakeTurns()
	broker := daemon.NewBroker(daemon.NewHub())
	b, err := New(Options{
		Cfg: testDiscordConfig(), Client: f, Turns: turns, Store: st,
		Broker: broker, Guard: nil, // no tools run in these tests
		Throttle: -1, // flush on every event; never wait on a clock
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, turns, broker
}

func TestAMessageInAChannelOpensAThreadAndASession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "what time is it?"})
	turns.waitForTurn(t)

	threads := f.createdThreads()
	if len(threads) != 1 {
		t.Fatalf("created %d threads, want 1", len(threads))
	}
	// The thread's name comes from the prompt, so the channel reads as an
	// index of what you asked.
	if !strings.Contains(threads[0].Name, "what time is it") {
		t.Fatalf("thread name %q does not come from the prompt", threads[0].Name)
	}

	sid, found, err := st.SessionForExternal(context.Background(), bridgeName, threads[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("the thread was not bound to a session: (found=%v, err=%v)", found, err)
	}
	if got := turns.lastStart(); got.sessionID != sid || got.text != "what time is it?" {
		t.Fatalf("started %+v, want session %s with the prompt", got, sid)
	}
	// The bridge is the untrusted surface. This is the assertion that keeps
	// it that way.
	if got.profile != policy.ProfileRemote {
		t.Fatalf("turn profile = %q, want %q", got.profile, policy.ProfileRemote)
	}
}

func TestAReplyInAThreadContinuesItsSession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "first"})
	turns.waitForTurn(t)
	thread := f.createdThreads()[0].ThreadID
	want, _, _ := st.SessionForExternal(context.Background(), bridgeName, thread)

	f.deliver(Inbound{MessageID: "m2", UserID: "U", GuildID: "G", ChannelID: thread, ParentID: "C1", Content: "second"})
	turns.waitForTurn(t)

	if len(f.createdThreads()) != 1 {
		t.Fatal("a reply in a thread created a second thread")
	}
	if got := turns.lastStart(); got.sessionID != want || got.text != "second" {
		t.Fatalf("reply started %+v, want session %s", got, want)
	}
}

func TestABindingSurvivesARestart(t *testing.T) {
	// The binding lives in SQLite, not in memory, so a thread you replied in
	// yesterday is still that session after the daemon restarts.
	b, f, turns, st := newTestBridge(t)
	b.Start(context.Background())
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "first"})
	turns.waitForTurn(t)
	thread := f.createdThreads()[0].ThreadID
	want, _, _ := st.SessionForExternal(context.Background(), bridgeName, thread)
	b.Close()

	// A brand-new Bridge over the SAME store, as a restart would build.
	b2, f2, turns2 := restartBridge(t, st)
	defer b2.Close()
	b2.Start(context.Background())
	f2.deliver(Inbound{MessageID: "m9", UserID: "U", GuildID: "G", ChannelID: thread, ParentID: "C1", Content: "after restart"})
	turns2.waitForTurn(t)

	if got := turns2.lastStart(); got.sessionID != want {
		t.Fatalf("after restart the thread mapped to %q, want %q", got.sessionID, want)
	}
	if len(f2.createdThreads()) != 0 {
		t.Fatal("the restarted bridge opened a new thread for an existing one")
	}
}

func TestRedeliveredMessagesRunOneTurn(t *testing.T) {
	// The gateway redelivers on resume. Running the prompt twice is worse
	// than dropping it: it can repeat a side effect the user approved once.
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	in := Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "do it"}
	f.deliver(in)
	turns.waitForTurn(t)
	f.deliver(in)

	turns.expectNoFurtherTurn(t, 200*time.Millisecond)
	if n := turns.startCount(); n != 1 {
		t.Fatalf("started %d turns for one message, want 1", n)
	}
}

func TestUnadmittedTrafficIsDroppedSilently(t *testing.T) {
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "m1", UserID: "STRANGER", GuildID: "G", ChannelID: "C1", Content: "hello?"})
	turns.expectNoFurtherTurn(t, 200*time.Millisecond)

	// Silence is the point. A reply — even "you are not allowed" — confirms
	// to whoever probed that the bot is live and listening.
	if len(f.sentTo("C1")) != 0 || len(f.createdThreads()) != 0 || len(f.responses()) != 0 {
		t.Fatal("the bridge answered an unadmitted message")
	}
}

func TestAnUnadmittedButtonPressIsDropped(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())
	sid, _ := st.CreateSession(context.Background(), "s")

	f.press(Interaction{
		ID: "i1", Token: "tok", UserID: "STRANGER", GuildID: "G", ChannelID: "C1",
		CustomID: encodeCustomID(sid, 1, true, policy.ScopeOnce),
	})
	if len(f.responses()) != 0 {
		t.Fatal("the bridge responded to a stranger's button press")
	}
}

func TestSlashNewStartsAFreshDMSession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "d1", UserID: "U", ChannelID: "DM1", Content: "first"})
	turns.waitForTurn(t)
	first, _, _ := st.SessionForExternal(context.Background(), bridgeName, "DM1")

	f.deliver(Inbound{MessageID: "d2", UserID: "U", ChannelID: "DM1", Content: "/new"})
	// /new starts no turn — it only rebinds — so wait on the binding.
	waitFor(t, func() bool {
		got, _, _ := st.SessionForExternal(context.Background(), bridgeName, "DM1")
		return got != first
	})

	f.deliver(Inbound{MessageID: "d3", UserID: "U", ChannelID: "DM1", Content: "second"})
	turns.waitForTurn(t)
	if got := turns.lastStart(); got.sessionID == first {
		t.Fatal("/new did not start a fresh session")
	}
	if turns.startCount() != 2 {
		t.Fatalf("started %d turns, want 2 (/new is not a prompt)", turns.startCount())
	}
}

func TestABusySessionTellsTheUser(t *testing.T) {
	// A second prompt while a turn is running is refused by the hub. Saying
	// nothing would look like the bot had died.
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())
	turns.nextError = daemon.ErrTurnRunning

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "go"})
	waitFor(t, func() bool { return len(f.allSent()) > 0 })

	var joined strings.Builder
	for _, m := range f.allSent() {
		joined.WriteString(m.Message.Content)
	}
	if !strings.Contains(strings.ToLower(joined.String()), "already") {
		t.Fatalf("the user was not told the session is busy: %q", joined.String())
	}
}
```

Add `waitFor(t, cond func() bool)` polling every 10ms up to 5s, and the `fakeClient` accessors used above (`createdThreads`, `responses`, `allSent`).

- [ ] **Step 3: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run 'Message|Thread|Redeliver|Unadmitted|SlashNew|Busy|Binding' -v`
Expected: FAIL to compile — `New`, `Bridge`, `fakeTurns` undefined.

- [ ] **Step 4: Write `fakeTurns`**

In `bridge_test.go`:

```go
type startedTurn struct {
	sessionID, text, client string
	profile                 policy.Profile
}

// fakeTurns records what the bridge asked the daemon to run. It does not run
// anything: turn behaviour is the daemon's tested elsewhere, and mixing the
// two would make these tests about the agent loop instead of the bridge.
type fakeTurns struct {
	mu      sync.Mutex
	starts  []startedTurn
	events  map[string]chan daemon.WireEvent
	started chan struct{}

	// nextError is returned by the next StartTurn, then cleared.
	nextError error
}
```

`StartTurn` appends under the lock, signals `started`, and returns/clears `nextError`. `Subscribe` returns a per-session buffered channel a test can publish into, plus a no-op cancel. `waitForTurn` reads from `started` with a 5s timeout; `expectNoFurtherTurn` asserts nothing arrives within the given window; `lastStart` and `startCount` copy under the lock.

- [ ] **Step 5: Implement `bridge.go`**

```go
// Bridge connects one Discord bot to spore. It owns no agent machinery: it
// resolves a Discord conversation to a session, asks the daemon to run a
// turn, and renders what comes back. Everything that decides whether a call
// may run stays in the policy engine, and everything that decides whether a
// person may be here stays in the Admitter.
type Bridge struct {
	cfg      config.DiscordConfig
	admit    Admitter
	client   Client
	turns    Turns
	store    *store.Store
	answer   *answerer
	throttle time.Duration

	// ctx bounds the render goroutines. It is the BRIDGE's context, never a
	// message's: a turn outlives the message that started it, and so must
	// the goroutine rendering it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}
```

`New` validates that `o.Client`, `o.Turns` and `o.Store` are non-nil (return an error naming the missing one) and builds the `Admitter` and `answerer`.

`Start(ctx)` stores a cancellable child of `ctx` and calls `b.client.Open(ctx, b.handleMessage, b.handleInteraction)`.

`handleMessage(in Inbound)`:

```go
func (b *Bridge) handleMessage(in Inbound) {
	// Admission first, before anything is written, logged at debug only.
	// A dropped message produces no reply of any kind.
	if !b.admit.AdmitMessage(in) {
		return
	}
	if strings.TrimSpace(in.Content) == "" {
		return
	}
	// The gateway redelivers on resume, so the claim comes before the work.
	fresh, err := b.store.MarkSeen(b.ctx, bridgeName, in.MessageID)
	if err != nil {
		slog.Warn("discord dedupe failed", "err", err)
		return // Failing closed: better a dropped prompt than a repeated one.
	}
	if !fresh {
		return
	}
	...
}
```

Then:

1. `/new`: when the trimmed content is exactly `/new`, create a session, `BindExternal` it to `in.ChannelID`, send a short confirmation, and return without starting a turn.
2. Resolve the session. Look up `SessionForExternal(bridgeName, in.ChannelID)`. If found, use it. If not found and `in.ParentID != ""` the message is in a thread spore did not open — treat it as a new session bound to the thread. If not found and this is a plain channel message, create a session, call `b.client.CreateThread(ctx, in.ChannelID, in.MessageID, threadName(in.Content))`, and bind the returned thread id. If `CreateThread` fails, fall back to binding `in.ChannelID` itself and say so in a message — a bridge that cannot make threads must still work.
3. Attach the renderer **before** starting the turn, or the first events are lost:

```go
	events, cancel := b.turns.Subscribe(sessionID)
	r := newRenderer(b.client, replyChannel, b.throttle)
	r.onApproval(func(ev daemon.WireEvent) { b.postApproval(sessionID, replyChannel, ev) })
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer cancel()
		r.Consume(b.ctx, events)
	}()

	if err := b.turns.StartTurn(sessionID, text, bridgeName, policy.ProfileRemote); err != nil {
		cancel()
		if errors.Is(err, daemon.ErrTurnRunning) {
			b.say(replyChannel, "that session is already running a turn; wait for it to finish")
			return
		}
		b.say(replyChannel, "could not start the turn: "+err.Error())
		return
	}
```

The renderer is built with `stopAfterTurn` set (Task 7), so `Consume` returns on `WireTurnDone` or `WireError` and the deferred `cancel` detaches the subscription. That is what keeps the goroutine count at one per running turn rather than one per prompt ever sent.

`New` maps the throttle: `throttle := o.Throttle; if throttle == 0 { throttle = defaultThrottle }`. A negative value is passed through unchanged, which `newRenderer` reads as "flush on every event".

`handleInteraction(i Interaction)`:

```go
func (b *Bridge) handleInteraction(i Interaction) {
	// The same admission rules as a message. A button press is a second
	// entrance to the same house.
	if !b.admit.AdmitInteraction(i) {
		return
	}
	sessionID, pendingID, ans, err := decodeCustomID(i.CustomID)
	if err != nil {
		return // Not one of ours, or malformed. Silence again.
	}
	// Discord fails the interaction visibly if it is not acknowledged within
	// three seconds, so answer, then report.
	msg, err := b.answer.answer(b.ctx, sessionID, pendingID, ans)
	if err != nil {
		msg = "could not record that: " + err.Error()
	}
	if err := b.client.Respond(b.ctx, i.ID, i.Token, msg); err != nil {
		slog.Warn("discord interaction response", "err", err)
	}
}
```

`threadName(prompt string)` takes the first line, collapses whitespace, truncates to 90 runes, and falls back to `"spore session"` when empty.

`say(channelID, text string)` is a logged best-effort `Send`.

- [ ] **Step 6: Run**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -v`
Expected: PASS.

- [ ] **Step 7: Run with the race detector**

Run: `go test -tags sqlite_fts5 -race -count=2 ./internal/bridge/discord/`
Expected: PASS with no race report. `-count=2` because the goroutine lifecycle here is the part most likely to be flaky.

- [ ] **Step 8: Commit**

```bash
git add internal/bridge/discord internal/daemon/server.go
git commit -m "feat(bridge): route Discord messages to sessions, one thread per session"
```

---

### Task 10: Supervision and wiring

The gateway drops. The bridge must come back on its own without the daemon restarting, and it must not take the daemon down when it cannot connect at all.

**Files:**
- Create: `internal/bridge/discord/supervise.go`, `internal/bridge/discord/supervise_test.go`
- Modify: `cmd/spore/wire.go`, `cmd/spore/serve.go`

**Interfaces:**
- Consumes: `Bridge` (Task 9).
- Produces:
```go
// Supervise runs the bridge until ctx is done, reconnecting with backoff.
func Supervise(ctx context.Context, b *Bridge, log *slog.Logger)

// in cmd/spore
func buildBridge(cfg *config.Config, srv *daemon.Server) (*discord.Bridge, error)
```
  `buildBridge` returns `(nil, nil)` when the bridge is disabled.

- [ ] **Step 1: Write the failing tests**

```go
func TestSuperviseRetriesAFailedOpen(t *testing.T) {
	f := newFakeClient()
	f.failNext["Open"] = errors.New("gateway refused")
	b := newBridgeOver(t, f) // helper from bridge_test.go

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); Supervise(ctx, b, slog.Default()) }()

	// The first Open fails; failNext is one-shot, so the retry succeeds.
	waitFor(t, func() bool { return f.openCount() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after its context was cancelled")
	}
}

func TestSuperviseStopsOnContextCancel(t *testing.T) {
	f := newFakeClient()
	b := newBridgeOver(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Supervise(ctx, b, slog.Default()) }()
	waitFor(t, func() bool { return f.openCount() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise ignored cancellation")
	}
	if !f.closed() {
		t.Fatal("Supervise did not close the client on the way out")
	}
}
```

Give `fakeClient` an `openCount()` and a `closed()` accessor, and make `failNext["Open"]` one-shot (taken and cleared).

- [ ] **Step 2: Run to watch them fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run Supervise -v`
Expected: FAIL to compile — `Supervise undefined`.

- [ ] **Step 3: Implement `Supervise`**

```go
// Supervise keeps the bridge connected. A dropped gateway is normal — a
// laptop sleeps, a link flaps — so a failure to connect is a wait, never a
// fatal error for the daemon: spore's local web UI must keep working when
// Discord is unreachable.
//
// Backoff is capped rather than unbounded, because the thing on the other end
// is a service that comes back, and a bridge that has backed off to an hour
// is indistinguishable from a broken one.
func Supervise(ctx context.Context, b *Bridge, log *slog.Logger) {
	const (
		minBackoff = 2 * time.Second
		maxBackoff = 2 * time.Minute
	)
	backoff := minBackoff
	for {
		if err := b.Start(ctx); err != nil {
			log.Warn("discord bridge could not connect; retrying", "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				_ = b.Close()
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		log.Info("discord bridge connected")
		backoff = minBackoff
		// Connected. discordgo reconnects and resumes the session itself, so
		// there is nothing to poll here; wait for shutdown.
		<-ctx.Done()
		_ = b.Close()
		return
	}
}
```

- [ ] **Step 4: Wire it into the daemon**

In `cmd/spore/wire.go`:

```go
// buildBridge constructs the Discord bridge, or reports (nil, nil) when it is
// not configured. It must be built AFTER the server, because the bridge needs
// the broker and guard the server owns.
func buildBridge(cfg *config.Config, srv *daemon.Server) (*discord.Bridge, error) {
	d := cfg.Bridge.Discord
	if !d.Enabled {
		return nil, nil
	}
	client, err := discord.NewGatewayClient(d.Token)
	if err != nil {
		return nil, err
	}
	return discord.New(discord.Options{
		Cfg: d, Client: client, Turns: srv,
		Store: srv.Store(), Broker: srv.Broker(), Guard: srv.Guard(),
	})
}
```

In `cmd/spore/serve.go`, after the scheduler is started and before `srv.Run`:

```go
	bridge, err := buildBridge(cfg, srv)
	if err != nil {
		// A misconfigured bridge is a config error and should stop startup:
		// silently serving without the surface you asked for is worse.
		return err
	}
	if bridge != nil {
		go discord.Supervise(ctx, bridge, slog.Default())
		if !detach {
			fmt.Println("discord bridge enabled")
		}
	}
```

- [ ] **Step 5: Verify the whole build**

Run: `make vet && make build && make test`
Expected: PASS.

- [ ] **Step 6: Run the full suite with the race detector**

Run: `go test -tags sqlite_fts5 -race ./...`
Expected: PASS. Do not skip this; the bridge adds goroutines to a daemon that already had several.

- [ ] **Step 7: Commit**

```bash
git add internal/bridge/discord cmd/spore
git commit -m "feat(bridge): supervise the Discord gateway and wire it into spore serve"
```

---

### Task 11: End-to-end, and the docs

One test that boots the real daemon with the scripted fake provider and drives it entirely from the fake Discord client, including an approval answered by a button press. This is the test that would catch a wiring mistake none of the unit tests can see.

**Files:**
- Create: `internal/bridge/discord/e2e_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything.
- Produces: nothing.

- [ ] **Step 1: Write the end-to-end test**

```go
// TestEndToEndDiscordTurnWithApproval boots a real daemon, a real store, a
// real guard and a real policy engine, and drives the whole thing from the
// fake Discord client. The provider is the scripted fake; Discord is the only
// other thing faked. Config comes from a written file through config.Load,
// because Load is what appends the baseline deny rules — a config built from
// config.Default() has no baseline, and every security assertion below would
// then be vacuous.
func TestEndToEndDiscordTurnWithApproval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, `
default_model = "fake/model"

[providers.fake]
kind = "openai"
base_url = "http://127.0.0.1:1/unused"

[policy]
workspace = "`+dir+`"
default   = "ask"
allow     = ["fs_read"]
ask       = ["shell_exec"]

[bridge.discord]
enabled     = true
token       = "test-token"
guild_id    = "G"
channel_ids = ["C1"]
user_ids    = ["U"]
`)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// The provider script: one shell_exec (which policy says ask), then a
	// sentence once the tool result comes back.
	srv, st := newDaemonWithScriptedProvider(t, cfg, scriptShellThenText)

	f := newFakeClient()
	b, err := New(Options{
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv,
		Store: st, Broker: srv.Broker(), Guard: srv.Guard(),
		Throttle: -1, // flush every event; the test should not wait on a clock
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "run ls"})

	// The approval must arrive as buttons, and the pattern button must be
	// absent: shell_exec has no path-shaped argument to generalise to.
	var prompt sentMessage
	waitFor(t, func() bool {
		for _, m := range f.allSent() {
			if len(m.Message.Buttons) > 0 {
				prompt = m
				return true
			}
		}
		return false
	})
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Scope == policy.ScopePattern {
			t.Fatal("a one-tap blanket allow for shell_exec was offered on the phone surface")
		}
	}

	// Press "allow once".
	var allowOnce string
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			allowOnce = btn.CustomID
		}
	}
	if allowOnce == "" {
		t.Fatal("no allow-once button")
	}
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G",
		ChannelID: f.createdThreads()[0].ThreadID, ParentID: "C1", CustomID: allowOnce})

	// The turn resumes and its text reaches Discord.
	waitFor(t, func() bool {
		for _, c := range f.finalContents(f.createdThreads()[0].ThreadID) {
			if strings.Contains(c, "done") {
				return true
			}
		}
		return false
	})

	// No suspension is left open. The session id comes from the binding the
	// bridge wrote, which is also a check that the binding is correct.
	sessionID, found, err := st.SessionForExternal(context.Background(), bridgeName, f.createdThreads()[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("no session bound to the thread: (found=%v, err=%v)", found, err)
	}
	pending, err := srv.Guard().Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d suspensions left open after the button press", len(pending))
	}
}

// TestEndToEndAStrangerCannotAnswerAnApproval is the same stack, driven by a
// user id that is not on the allowlist. This is the case the bridge exists to
// make safe: the approval prompt is visible in a channel, so anyone who can
// see it can try to press its buttons.
func TestEndToEndAStrangerCannotAnswerAnApproval(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	writeFile(t, cfgPath, `
default_model = "fake/model"

[providers.fake]
kind = "openai"
base_url = "http://127.0.0.1:1/unused"

[policy]
workspace = "`+dir+`"
default   = "ask"
allow     = ["fs_read"]
ask       = ["shell_exec"]

[bridge.discord]
enabled     = true
token       = "test-token"
guild_id    = "G"
channel_ids = ["C1"]
user_ids    = ["U"]
`)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, st := newDaemonWithScriptedProvider(t, cfg, scriptShellThenText)

	f := newFakeClient()
	b, err := New(Options{
		Cfg: cfg.Bridge.Discord, Client: f, Turns: srv,
		Store: st, Broker: srv.Broker(), Guard: srv.Guard(), Throttle: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "run ls"})

	var prompt sentMessage
	waitFor(t, func() bool {
		for _, m := range f.allSent() {
			if len(m.Message.Buttons) > 0 {
				prompt = m
				return true
			}
		}
		return false
	})
	var allowOnce string
	for _, btn := range prompt.Message.Buttons {
		if _, _, ans, err := decodeCustomID(btn.CustomID); err == nil && ans.Allow && ans.Scope == policy.ScopeOnce {
			allowOnce = btn.CustomID
		}
	}
	if allowOnce == "" {
		t.Fatal("no allow-once button")
	}

	thread := f.createdThreads()[0].ThreadID
	sessionID, _, err := st.SessionForExternal(context.Background(), bridgeName, thread)
	if err != nil {
		t.Fatal(err)
	}

	// A user who is not on the allowlist presses the real button, with the
	// real custom id, in the real thread. Only the user id differs.
	f.press(Interaction{
		ID: "i1", Token: "tok", UserID: "STRANGER", GuildID: "G",
		ChannelID: thread, ParentID: "C1", CustomID: allowOnce,
	})

	// Silence: not even a refusal. A reply would confirm to whoever pressed
	// it that the bot is live and that the button is real.
	if n := len(f.responses()); n != 0 {
		t.Fatalf("the bridge sent %d responses to a stranger's press", n)
	}
	// And the decision is still the real user's to make.
	pending, err := srv.Guard().Pending(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d suspensions open, want 1 still waiting for its owner", len(pending))
	}
}
```

`newDaemonWithScriptedProvider(t, cfg, script)` builds a real `store`, `agent`, `policy.Guard` and `daemon.Server` the way `cmd/spore/wire.go` does, substituting the scripted fake provider the daemon's own end-to-end test already uses; reuse that helper rather than writing a third fake provider. `scriptShellThenText` replays one `shell_exec` tool call, then the text `"done"` once the tool result arrives. `writeFile` is a two-line `os.WriteFile` helper with `t.Fatal` on error.

- [ ] **Step 2: Run it**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord/ -run EndToEnd -v`
Expected: PASS.

- [ ] **Step 3: Mutation-test the two security claims**

```bash
# (a) Remove the admission check from handleInteraction.
go test -tags sqlite_fts5 ./internal/bridge/discord/ -run EndToEndAStranger -v
# EXPECTED: FAIL. Revert.

# (b) In approvalMessage, offer the pattern button unconditionally
#     (drop the `if ev.Pattern != ""` guard and label it with ev.Tool).
go test -tags sqlite_fts5 ./internal/bridge/discord/ -run EndToEndDiscordTurnWithApproval -v
# EXPECTED: FAIL. Revert.
```

Report both results. If either mutation leaves the suite green, the test is not testing what it claims and must be fixed before this task is done.

- [ ] **Step 4: Update the README**

Change the status line to name Plan 4a, and add a setup section after "Configure":

```markdown
## Discord

spore can be driven from Discord. Create an application and bot at
<https://discord.com/developers/applications>, enable the **Message Content**
privileged intent under Bot → Privileged Gateway Intents, and invite it to a
server only you are in with the `bot` scope and the Send Messages, Create
Public Threads, Send Messages in Threads, Read Message History and Embed Links
permissions.

    [bridge.discord]
    enabled     = true
    token       = "${DISCORD_BOT_TOKEN}"
    guild_id    = "your server id"
    channel_ids = ["the channel spore listens in"]
    user_ids    = ["your user id"]
    allow_dms   = true

`guild_id`, `channel_ids` and `user_ids` are an allowlist, not a filter:
anything not named is dropped without a reply. Turn on Discord's Developer
Mode (Settings → Advanced) to copy ids.

A message in an allowlisted channel opens a thread and a session; replies in
that thread continue it. A DM is one rolling session, reset with `/new`.
Approvals arrive as buttons.

Discord sessions run under the `remote` trust profile, so you can hold them to
a stricter ruleset than the local web UI:

    [policy.profile.remote]
    default = "ask"
    allow   = ["fs_read", "fs_list", "fs_glob", "fs_grep"]
```

- [ ] **Step 5: Final verification**

Run: `make vet && make test && go test -tags sqlite_fts5 -race ./...`
Expected: all PASS. Paste the real output into the task report, or say plainly that it passed and the output was not captured.

- [ ] **Step 6: Commit**

```bash
git add internal/bridge/discord README.md
git commit -m "test(bridge): end-to-end Discord turn with a button-answered approval"
```

---

## Done means

- `make vet`, `make test` and `go test -tags sqlite_fts5 -race ./...` all pass, verified by the orchestrator rather than reported.
- Both mutation checks in Task 11 fail as predicted, and the two in Tasks 1 and 6.
- A stranger can neither start a turn nor answer an approval, and neither attempt produces any reply.
- "Always allow this pattern" is offered nowhere — terminal, browser or Discord — for a call with no path-shaped argument, and the guard records a degraded pattern answer as `once`.
- Discord sessions run under `policy.ProfileRemote`.
- A thread's session survives a daemon restart, and a redelivered message runs one turn.
