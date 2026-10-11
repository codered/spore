# Companion PR 2a: heartbeat + outreach Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Spore starts conversations. A heartbeat reflects on candidate habits and unanswered messages, proposes watches with Discord buttons, nudges once, and lets a governor settle the cadence by engagement.

**Architecture:** Pure decision logic (governor, quiet hours, watch validation, reflection parse/validate) lives in `internal/companion` and is table-tested. `companion.Heartbeat` runs one tick over the store with three injected seams: a `Reflector` (the LLM call), a `Deliverer` (the daemon: companion session + channel), and a `SelfWriter` (the refiner's ledgered `self.md` write). `companion.Service` holds the deterministic user actions: proposal buttons, replies, feedback, pause, interest updates. The daemon implements the `Deliverer` and the proposal route. The Discord bridge implements `companion.Channel`, which opens a companion thread (or a DM) and renders proposal buttons. Watch jobs run under a new `companion` policy profile that `config.Load` injects.

**Tech Stack:** Go 1.x, SQLite (`modernc`/`database/sql` as in the repo), discordgo behind the bridge's `Client` interface, the existing provider registry and router.

**Spec:** `docs/superpowers/specs/2026-10-10-companion-design.md`. Read the section **"PR 2 decisions (2026-10-10)"** at the end first: it overrides §5.2 (Discord), splits PR 2 into 2a/2b, and adds the 30-day recency rule.

## Global Constraints

- `[companion] enabled = false` must leave behaviour unchanged: no heartbeat goroutine, no `interest_update`/`companion_feedback` tools, no companion admission rule on Discord.
- Policy tests build config with `config.Load`, never `config.Default()` (Default skips the baseline deny and the profile merge).
- Reflection actions in 2a: `propose`, `nudge`, `none`, plus `drops` and `self_md`. Every other action (`alert`, `finding`, `followup`, `digest`, `research`) is invalid output in 2a.
- Invalid reflection output skips the tick: nothing is sent and nothing is written.
- `message` is 1..1200 characters; watch `goal` 1..500; drop `reason` 1..200; watch `cron` is a five-field cron (not a one-off) firing at most hourly, evaluated in UTC.
- Unanswered after 12 h (`pending` → `unanswered`, never `ignored`); a nudge `pending` 24 h → nudge and original `ignored`; at most one nudge per outreach.
- Governor: replied/reacted `+0.25`, ignored `-0.5`, stop halves; floor 1, ceiling `start_budget`; the integer part is the day's count.
- "Not now" cools down 14 days; a dropped or nudge-ignored proposal returns its interest to `candidate` with a 7-day cooldown.
- Only the Yes button (or `spore companion answer … yes`) makes an interest `active`. `interest_update` can never do it.
- Watch jobs: `notify = each`, `checked_in = 1`, `profile = companion`, `origin_session_id` = the companion session.
- `companion` profile: allow `web_*`, `recall_search`, `fs_read`, `fs_list`, `fs_glob`, `fs_grep`, `self_note`; default deny; explicit deny `shell_exec`, `fs_write`, `fs_edit`, `memory`, `mcp__*`, `schedule_*`, `agent_*`, `skill_install`, `interest_update`, `companion_feedback`.
- Every companion Discord user must also be in `bridge.discord.user_ids`.
- Go style: match the surrounding code: doc comments on every exported name, comments that say why, errors wrapped with `%w` and naming the key or id involved.
- CI gate (all must pass before every commit that ends a task): `go vet ./...`, `make fmtcheck` (or `gofmt -l .` empty), `make lint`, `go test ./...`. Run the full gate before the final commit: `make vet fmtcheck lint vulncheck tidycheck` and `go test ./... -race`.
- Tests that touch temp dirs must pass with a symlinked TMPDIR (macOS). Compare paths with `filepath.EvalSymlinks` when a test compares a path it did not build itself.

## Review Focus

1. **The user replies mid-heartbeat.** A turn is running in the companion session when the heartbeat delivers. Expected: delivery refuses (`companion.ErrBusy`), nothing is appended into the running turn's history, and the tick reports "companion session busy" without counting a failure. Pinned in Task 5 (`TestTickBusyIsNotAFailure`) and Task 7 (`TestDeliverRefusesWhileATurnRuns`).
2. **The user deletes the companion session.** Expected: the next delivery opens a new session (and a new Discord thread), and active watches report into it. Pinned in Task 7 (`TestEnsureSessionRecreatesAndReattachesWatches`) and Task 9 (`TestCompanionChannelOpensANewThreadForANewSession`).
3. **A button pressed twice, or after "Not now".** Expected: the second press answers "not an open proposal" and creates no second job. Pinned in Task 6 (`TestAnswerProposalTwiceCreatesOneJob`) and Task 7 (`TestProposalRouteAnswersOnce`).
4. **A stranger in the companion thread.** A guild member who is a bridge user but not a companion user presses a proposal button or posts in the thread. Expected: silently not admitted. Pinned in Task 9 (`TestCompanionAdmission`).
5. **Old evidence revives an interest.** An interest seen on two days six weeks ago comes up once today. Expected: it stays `observing` (one recent day), not `candidate`. Pinned in Task 3 (`TestSweepCountsOnlyRecentDays`).

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/config/config.go` | `[companion.discord]`, channel resolution, `companion` profile injection, new default allows, `RoutingOverrideSites` | 1 |
| `internal/policy/rule.go` | `ProfileCompanion` | 1 |
| `internal/config/companion_discord_test.go` | config + policy tests via `config.Load` | 1 |
| `internal/store/schema.go` | `outreach`, `companion_state` tables | 2 |
| `internal/store/outreach.go` | outreach rows, companion state | 2 |
| `internal/store/jobs.go` | `profile` column; `CreateJob` honours notify/checked_in/profile; `UpdateJobSchedule` | 2 |
| `internal/store/interests.go` | `InterestByID`, `InterestDays`, `InterestDaysSince`, cooldown, activate, decline | 2 |
| `internal/store/refine.go` | `IdleSessions` excludes companion sessions | 2 |
| `internal/companion/recorder.go` | 30-day recency; retire interests whose watch is gone | 3 |
| `internal/companion/governor.go` | budget arithmetic | 4 |
| `internal/companion/quiet.go` | quiet hours, local day start | 4 |
| `internal/companion/watch.go` | cron/goal validation, watch prompt | 4 |
| `internal/companion/reflect.go` | `Decision`, `View`, parse, validate, prompt, input, fingerprint | 4 |
| `internal/companion/state.go` | `companion_state` keys, budget load/save, `Resolve`, spend | 5 |
| `internal/companion/heartbeat.go` | `Heartbeat.Tick`/`Run` | 5 |
| `internal/companion/service.go` | proposals, replies, feedback, pause, undecline, interest updates, status | 6 |
| `internal/companion/llm.go` | `LLMReflector` on the `companion` router site | 7 |
| `internal/router/router.go` | `SiteCompanion` | 7 |
| `internal/bridge/discord/model.go` | `/model` group gains `companion` | 7 |
| `internal/daemon/companion.go` | deliverer, proposal + status routes, reply hook | 7 |
| `internal/daemon/server.go`, `sessions.go`, `jobs.go` | fields, routes, hook call, job profile | 7 |
| `internal/tool/companiontool/companiontool.go` | `interest_update`, `companion_feedback` | 8 |
| `internal/bridge/discord/client.go`, `admit.go`, `bridge.go`, `companion.go` | DM channel, companion admission, channel, buttons | 9 |
| `cmd/spore/wire.go`, `serve.go`, `companion.go` | wiring, heartbeat goroutine, CLI | 10 |
| `internal/refine/refine.go` | `TriggerHeartbeat` | 10 |
| `internal/tui/sidebar.go` | companion session visible by default | 10 |
| `README.md`, spec "As built — PR 2a" | docs | 11 |

---

### Task 1: Config: `[companion.discord]`, the `companion` profile, default allows

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/policy/rule.go`
- Create: `internal/config/companion_discord_test.go`

**Interfaces:**
- Produces:
  - `config.CompanionDiscordConfig{Mode, ChannelID string; UserIDs []string}`; field `CompanionConfig.Discord`.
  - consts `config.CompanionDiscordThread = "thread"`, `config.CompanionDiscordDM = "dm"`, `config.CompanionProfile = "companion"`.
  - `func (c *Config) CompanionChannel() string`: returns `CompanionChannelDiscord` or `CompanionChannelTerminal`.
  - `policy.ProfileCompanion policy.Profile = "companion"`.
  - `interest_update` and `companion_feedback` in the default base `Allow`.

- [ ] **Step 1: Write the failing tests**

Create `internal/config/companion_discord_test.go`:

```go
package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

func loadCompanionTOML(t *testing.T, body string) (*config.Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(p, []byte("default_model = \"anthropic/claude-opus-5\"\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Load(p)
}

const companionBridgeTOML = `
[bridge.discord]
enabled = true
token = "t"
guild_id = "G"
channel_ids = ["C1"]
user_ids = ["U", "V"]
`

func TestCompanionDiscordDefaults(t *testing.T) {
	cfg, err := loadCompanionTOML(t, companionBridgeTOML+"\n[companion]\nenabled = true\n")
	if err != nil {
		t.Fatal(err)
	}
	d := cfg.Companion.Discord
	if d.Mode != config.CompanionDiscordThread {
		t.Errorf("mode = %q, want thread", d.Mode)
	}
	if !slices.Equal(d.UserIDs, []string{"U", "V"}) {
		t.Errorf("user_ids = %v, want the bridge's", d.UserIDs)
	}
	if got := cfg.CompanionChannel(); got != config.CompanionChannelTerminal {
		t.Errorf("auto with no channel_id = %q, want terminal", got)
	}

	cfg, err = loadCompanionTOML(t, companionBridgeTOML+
		"\n[companion]\nenabled = true\n[companion.discord]\nchannel_id = \"CC\"\nuser_ids = [\"U\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.CompanionChannel(); got != config.CompanionChannelDiscord {
		t.Errorf("auto with a channel_id = %q, want discord", got)
	}
}

func TestCompanionDiscordValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"bad mode", companionBridgeTOML + "[companion.discord]\nmode = \"sms\"\n", "companion.discord.mode"},
		{"stranger", companionBridgeTOML + "[companion.discord]\nuser_ids = [\"W\"]\n", "not in bridge.discord.user_ids"},
		{"discord without channel id", companionBridgeTOML + "[companion]\nenabled = true\nchannel = \"discord\"\n", "channel_id is required"},
		{"dm without allow_dms", companionBridgeTOML + "[companion]\nenabled = true\nchannel = \"discord\"\n[companion.discord]\nmode = \"dm\"\n", "allow_dms"},
		{"discord without bridge", "[companion]\nenabled = true\nchannel = \"discord\"\n", "[bridge.discord]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadCompanionTOML(t, tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// Watch jobs run unattended: read-only tools and spore's own notes, nothing
// else, and never the companion's own tools.
func TestCompanionProfilePolicy(t *testing.T) {
	if string(policy.ProfileCompanion) != config.CompanionProfile {
		t.Fatalf("policy.ProfileCompanion %q != config.CompanionProfile %q", policy.ProfileCompanion, config.CompanionProfile)
	}
	cfg, err := loadCompanionTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	sess := policy.Session{ID: "s", Profile: policy.ProfileCompanion, Workspace: cfg.Policy.Workspace}
	for tool, want := range map[string]policy.Decision{
		"shell_exec":         policy.DecisionDeny,
		"fs_write":           policy.DecisionDeny,
		"fs_edit":            policy.DecisionDeny,
		"memory":             policy.DecisionDeny,
		"schedule_create":    policy.DecisionDeny,
		"agent_spawn":        policy.DecisionDeny,
		"skill_install":      policy.DecisionDeny,
		"mcp__gh__issue":     policy.DecisionDeny,
		"interest_update":    policy.DecisionDeny,
		"companion_feedback": policy.DecisionDeny,
		"made_up_tool":       policy.DecisionDeny,
		"web_fetch":          policy.DecisionAllow,
		"recall_search":      policy.DecisionAllow,
		"self_note":          policy.DecisionAllow,
		"fs_list":            policy.DecisionAllow,
	} {
		args := `{}`
		if tool == "fs_list" {
			args = `{"path":"."}`
		}
		if got := e.Evaluate(sess, policy.Call{Tool: tool, Args: json.RawMessage(args)}); got.Decision != want {
			t.Errorf("companion %s = %q (rule %s), want %q", tool, got.Decision, got.Rule, want)
		}
	}
}

func TestCompanionToolsAllowedLocalAndDefaultRemote(t *testing.T) {
	cfg, err := loadCompanionTOML(t, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []policy.Profile{policy.ProfileLocal, policy.ProfileRemote} {
		for _, tool := range []string{"interest_update", "companion_feedback"} {
			got := e.Evaluate(policy.Session{ID: "s", Profile: p, Workspace: cfg.Policy.Workspace},
				policy.Call{Tool: tool, Args: json.RawMessage(`{}`)})
			if got.Decision != policy.DecisionAllow {
				t.Errorf("%s %s = %q, want allow", p, tool, got.Decision)
			}
		}
	}
}

// An operator's own [policy.profile.companion] replaces the default whole.
func TestOperatorCompanionProfileWins(t *testing.T) {
	cfg, err := loadCompanionTOML(t, "[policy.profile.companion]\ndefault = \"deny\"\nallow = [\"web_*\"]\n")
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Policy.Profiles[config.CompanionProfile].Allow; !slices.Equal(got, []string{"web_*"}) {
		t.Fatalf("companion allow = %v, want the operator's", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'Companion|OperatorCompanion' -v`
Expected: compile errors (`config.CompanionDiscordThread`, `cfg.CompanionChannel`, `policy.ProfileCompanion` undefined).

- [ ] **Step 3: Implement**

In `internal/policy/rule.go`, extend the profile constants:

```go
const (
	ProfileLocal  Profile = "local"
	ProfileRemote Profile = "remote"
	// ProfileCompanion is what the companion's unattended runs (watch jobs)
	// work under. It must equal config.CompanionProfile, which config.Load
	// fills in when the operator has not written one.
	ProfileCompanion Profile = "companion"
)
```

(Keep the existing doc comment above the block. If the block is shaped differently, add the constant to it the same way.)

In `internal/config/config.go`:

1. Add the field to `CompanionConfig`, after `AlertsInQuietHours`:

```go
	// Discord says how the companion reaches the user when its channel is
	// Discord.
	Discord CompanionDiscordConfig `toml:"discord"`
```

2. After the `CompanionChannel*` constants, add:

```go
// CompanionDiscordConfig says how the companion reaches the user on Discord.
type CompanionDiscordConfig struct {
	// Mode is "thread" (a thread opened in ChannelID; the default) or "dm"
	// (a direct message to the first of UserIDs).
	Mode string `toml:"mode"`
	// ChannelID is the channel the companion's thread opens in. Required in
	// thread mode. A private channel of the user's own is the intended use;
	// it need not be in bridge.discord.channel_ids.
	ChannelID string `toml:"channel_id"`
	// UserIDs may talk in the companion thread and press its buttons. Empty
	// means bridge.discord.user_ids. Each must also be a bridge user: the
	// companion cannot admit someone the bridge does not.
	UserIDs []string `toml:"user_ids"`
}

// Companion Discord modes.
const (
	CompanionDiscordThread = "thread"
	CompanionDiscordDM     = "dm"
)

// CompanionProfile is the policy profile watch jobs run under. It mirrors
// policy.ProfileCompanion, which config cannot import.
const CompanionProfile = "companion"

// companionProfile is the default [policy.profile.companion]: read-only
// tools and spore's own notes. Default deny covers everything else; the
// explicit deny list says so in the policy view, and keeps the companion's
// own tools out of an unattended run.
func companionProfile() ProfilePolicy {
	return ProfilePolicy{
		Default: "deny",
		Allow:   []string{"web_*", "recall_search", "fs_read", "fs_list", "fs_glob", "fs_grep", "self_note"},
		Deny: []string{"shell_exec", "fs_write", "fs_edit", "memory", "mcp__*", "schedule_*", "agent_*",
			"skill_install", "interest_update", "companion_feedback"},
	}
}
```

3. In `Default()`, append `"interest_update", "companion_feedback"` to `Policy.Allow` (after `"self_note"`), and set the companion Discord default:

```go
		Companion: CompanionConfig{Channel: CompanionChannelAuto, QuietHours: "22:00-08:00", HabitDays: 3,
			Heartbeat: "30m", SelfMaxBytes: 10240, StartBudget: 4, AlertBudget: 3,
			Discord: CompanionDiscordConfig{Mode: CompanionDiscordThread}},
```

4. In `fillCompanionDefaults`, add:

```go
	if c.Discord.Mode == "" {
		c.Discord.Mode = d.Discord.Mode
	}
```

5. In `Load`, directly after `if cfg.Policy.Profiles == nil { … }`, add:

```go
	// The companion profile is what watch jobs run under. An operator's own
	// [policy.profile.companion] replaces it whole; the baseline deny applies
	// to either.
	if _, ok := cfg.Policy.Profiles[CompanionProfile]; !ok {
		cfg.Policy.Profiles[CompanionProfile] = companionProfile()
	}
```

and directly after `fillCompanionDefaults(&cfg.Companion, d.Companion)`, add:

```go
	if len(cfg.Companion.Discord.UserIDs) == 0 {
		cfg.Companion.Discord.UserIDs = slices.Clone(cfg.Bridge.Discord.UserIDs)
	}
```

6. In `(c *Config) Validate()`, directly after the `c.Companion.validate()` check, add:

```go
	if err := c.validateCompanionDiscord(); err != nil {
		return err
	}
```

and add these methods near `CompanionConfig.validate`:

```go
// validateCompanionDiscord checks [companion.discord] against the bridge.
// The user check always runs: a companion user the bridge would refuse is a
// mistake whether or not the companion is on yet.
func (c *Config) validateCompanionDiscord() error {
	d := c.Companion.Discord
	if d.Mode != CompanionDiscordThread && d.Mode != CompanionDiscordDM {
		return fmt.Errorf("companion.discord.mode must be thread or dm, got %q", d.Mode)
	}
	for _, u := range d.UserIDs {
		if !slices.Contains(c.Bridge.Discord.UserIDs, u) {
			return fmt.Errorf("companion.discord.user_ids: %s is not in bridge.discord.user_ids; the companion cannot admit someone the bridge does not", u)
		}
	}
	if c.Companion.Enabled && c.Companion.Channel == CompanionChannelDiscord {
		return c.companionDiscordReady()
	}
	return nil
}

// companionDiscordReady reports why the companion cannot be delivered on
// Discord, or nil when it can.
func (c *Config) companionDiscordReady() error {
	d, b := c.Companion.Discord, c.Bridge.Discord
	if !b.Enabled {
		return fmt.Errorf("companion.channel = \"discord\" needs [bridge.discord] enabled")
	}
	switch d.Mode {
	case CompanionDiscordThread:
		if strings.TrimSpace(d.ChannelID) == "" {
			return fmt.Errorf("companion.discord.channel_id is required in thread mode: name the channel the companion's thread opens in")
		}
		if b.GuildID == "" {
			return fmt.Errorf("companion.discord thread mode needs bridge.discord.guild_id")
		}
	case CompanionDiscordDM:
		if !b.AllowDMs {
			return fmt.Errorf("companion.discord.mode = \"dm\" needs bridge.discord.allow_dms = true")
		}
	}
	if len(d.UserIDs) == 0 {
		return fmt.Errorf("companion.discord.user_ids is empty, and so is bridge.discord.user_ids")
	}
	return nil
}

// CompanionChannel is where unprompted messages go: CompanionChannelDiscord
// or CompanionChannelTerminal. "auto" picks Discord when the bridge is on and
// [companion.discord] is complete, and the terminal otherwise.
func (c *Config) CompanionChannel() string {
	switch c.Companion.Channel {
	case CompanionChannelDiscord:
		return CompanionChannelDiscord
	case CompanionChannelTerminal:
		return CompanionChannelTerminal
	}
	if c.companionDiscordReady() == nil {
		return CompanionChannelDiscord
	}
	return CompanionChannelTerminal
}
```

7. Change `RoutingOverrideSites` to `[]string{"compaction", "title", "classify", "refinement", "companion"}` (Task 7 adds the matching router site).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ ./internal/policy/ -v -run 'Companion|OperatorCompanion|SelfNote'` then `go test ./...`
Expected: PASS. The new profile shows up wherever profiles are listed (the policy view in `internal/daemon` and `internal/tui`). If a pre-existing test pins the exact default `Allow` list, the profile list, or the number of sites in `RoutingOverrideSites`, update it to include the new entries and say so in the commit message.

- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/policy/rule.go
git commit -m "config: [companion.discord], the companion policy profile, companion tool allows"
```

---

### Task 2: Store: outreach, companion state, job profile, interest helpers

**Files:**
- Modify: `internal/store/schema.go`
- Create: `internal/store/outreach.go`
- Modify: `internal/store/jobs.go`
- Modify: `internal/store/interests.go`
- Modify: `internal/store/refine.go` (`IdleSessions`)
- Create: `internal/store/outreach_test.go`

**Interfaces:**
- Produces (all on `*store.Store`):
  - `AddOutreach(ctx, Outreach) (int64, error)`, `SetOutreachChannel(ctx, id int64, channel string) error`, `OutreachByID(ctx, id int64) (Outreach, bool, error)`, `OutreachSince(ctx, since time.Time) ([]Outreach, error)`, `OutreachByOutcome(ctx, outcomes ...string) ([]Outreach, error)`, `NudgeFor(ctx, id int64) (Outreach, bool, error)`, `ResolveOutreach(ctx, id int64, from []string, to, reason string, at time.Time) (bool, error)`
  - `CompanionState(ctx, key string) (string, bool, error)`, `SetCompanionState(ctx, key, value string) error`
  - `InterestByID(ctx, id int64) (Interest, bool, error)`, `InterestDays(ctx, id int64) ([]string, error)`, `InterestDaysSince(ctx, id int64, sinceDay string) (int, error)`, `SetInterestCooldown(ctx, id int64, until time.Time) error`, `ActivateInterest(ctx, id, jobID int64) (bool, error)`, `DeclineInterest(ctx, id int64) (bool, error)`
  - `UpdateJobSchedule(ctx, id int64, spec, prompt string, next time.Time) error`
  - `store.Job.Profile string` (default `"local"`); `CreateJob` now stores `j.Notify` when `ValidNotify`, `j.CheckedIn`, and `j.Profile`.
  - Types/consts: `store.Outreach{ID, InterestID int64; Kind, Channel, Text, Payload string; SentAt time.Time; Outcome string; NudgeOf int64; DropReason string; ResolvedAt time.Time}`; kinds `OutreachProposal|Nudge|Alert|Finding|Followup|Digest`; outcomes `OutcomePending|Unanswered|Replied|Reacted|Ignored|Stop`; channels `OutreachChannelDiscord|Terminal|Fallback`.

- [ ] **Step 1: Write the failing tests**

Create `internal/store/outreach_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestOutreachLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	at := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachProposal, Channel: OutreachChannelTerminal,
		Text: "watch ZS?", Payload: `{"cron":"0 14 * * 1-5","goal":"g"}`, SentAt: at})
	if err != nil {
		t.Fatal(err)
	}
	o, ok, err := st.OutreachByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("OutreachByID: %v %v", ok, err)
	}
	if o.Outcome != OutcomePending || o.InterestID != in.ID || !o.SentAt.Equal(at) || o.NudgeOf != 0 {
		t.Fatalf("stored = %+v", o)
	}
	moved, err := st.ResolveOutreach(ctx, id, []string{OutcomePending}, OutcomeUnanswered, "", at.Add(13*time.Hour))
	if err != nil || !moved {
		t.Fatalf("pending -> unanswered: %v %v", moved, err)
	}
	moved, err = st.ResolveOutreach(ctx, id, []string{OutcomePending}, OutcomeIgnored, "", at)
	if err != nil || moved {
		t.Fatalf("a second resolve from pending moved it: %v %v", moved, err)
	}
	nid, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachNudge, Text: "still?", NudgeOf: id, SentAt: at.Add(14 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	n, ok, err := st.NudgeFor(ctx, id)
	if err != nil || !ok || n.ID != nid {
		t.Fatalf("NudgeFor = %+v %v %v", n, ok, err)
	}
	if _, ok, _ := st.NudgeFor(ctx, nid); ok {
		t.Fatal("a nudge has no nudge of its own")
	}
	un, err := st.OutreachByOutcome(ctx, OutcomeUnanswered)
	if err != nil || len(un) != 1 || un[0].ID != id {
		t.Fatalf("unanswered = %+v %v", un, err)
	}
	since, err := st.OutreachSince(ctx, at.Add(time.Hour))
	if err != nil || len(since) != 1 || since[0].ID != nid {
		t.Fatalf("since = %+v %v", since, err)
	}
	if err := st.SetOutreachChannel(ctx, nid, OutreachChannelFallback); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.OutreachByID(ctx, nid); n.Channel != OutreachChannelFallback {
		t.Fatalf("channel = %q", n.Channel)
	}
	if _, err := st.ResolveOutreach(ctx, id, []string{OutcomeUnanswered}, OutcomeIgnored, "moment passed", at); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := st.OutreachByID(ctx, id); o.DropReason != "moment passed" || o.Outcome != OutcomeIgnored {
		t.Fatalf("dropped = %+v", o)
	}
}

func TestCompanionState(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, ok, err := st.CompanionState(ctx, "daily_budget"); err != nil || ok {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	for _, v := range []string{"4", "3.5"} {
		if err := st.SetCompanionState(ctx, "daily_budget", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := st.CompanionState(ctx, "daily_budget"); err != nil || !ok || v != "3.5" {
		t.Fatalf("got %q %v %v", v, ok, err)
	}
}

func TestJobProfileNotifyAndCheckIn(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	next := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
	id, err := st.CreateJob(ctx, Job{Kind: "cron", Spec: "0 14 * * 1-5", Prompt: "watch", Enabled: true, NextRun: next,
		Notify: NotifyEach, CheckedIn: true, Profile: "companion"})
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := st.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Profile != "companion" || j.Notify != NotifyEach || !j.CheckedIn {
		t.Fatalf("watch job = %+v", j)
	}
	plain, err := st.CreateJob(ctx, Job{Kind: "cron", Spec: "0 9 * * *", Prompt: "p", Enabled: true, NextRun: next, Notify: NotifyAsk})
	if err != nil {
		t.Fatal(err)
	}
	if j, _, _ := st.Job(ctx, plain); j.Profile != "local" || j.Notify != NotifyAsk || j.CheckedIn {
		t.Fatalf("plain job = %+v", j)
	}
	later := next.Add(24 * time.Hour)
	if err := st.UpdateJobSchedule(ctx, id, "0 15 * * 1-5", "watch harder", later); err != nil {
		t.Fatal(err)
	}
	if j, _, _ := st.Job(ctx, id); j.Spec != "0 15 * * 1-5" || j.Prompt != "watch harder" || !j.NextRun.Equal(later) {
		t.Fatalf("updated = %+v", j)
	}
}

func TestInterestHelpers(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	for _, d := range []string{"2026-09-01", "2026-10-10", "2026-10-11", "2026-10-11"} {
		at, _ := time.Parse("2006-01-02", d)
		if err := st.AddInterestSignal(ctx, in.ID, sid, "asked", d, at); err != nil {
			t.Fatal(err)
		}
	}
	days, err := st.InterestDays(ctx, in.ID)
	if err != nil || len(days) != 3 || days[0] != "2026-09-01" || days[2] != "2026-10-11" {
		t.Fatalf("days = %v %v", days, err)
	}
	if n, err := st.InterestDaysSince(ctx, in.ID, "2026-10-01"); err != nil || n != 2 {
		t.Fatalf("days since = %d %v", n, err)
	}
	until := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := st.SetInterestCooldown(ctx, in.ID, until); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.InterestByID(ctx, in.ID)
	if err != nil || !ok || !got.CooldownUntil.Equal(until) || got.DaysSeen != 3 {
		t.Fatalf("by id = %+v %v %v", got, ok, err)
	}
	if moved, _ := st.ActivateInterest(ctx, in.ID, 7); moved {
		t.Fatal("an observing interest was activated; only a proposed one may be")
	}
	_, _ = st.SetInterestState(ctx, in.ID, InterestObserving, InterestProposed)
	if moved, err := st.ActivateInterest(ctx, in.ID, 7); err != nil || !moved {
		t.Fatalf("activate: %v %v", moved, err)
	}
	if got, _, _ := st.InterestByID(ctx, in.ID); got.State != InterestActive || got.WatchJobID != 7 {
		t.Fatalf("active = %+v", got)
	}
	other, _ := st.TouchInterest(ctx, "topic:go", "Go")
	_, _ = st.SetInterestState(ctx, other.ID, InterestObserving, InterestProposed)
	if moved, err := st.DeclineInterest(ctx, other.ID); err != nil || !moved {
		t.Fatalf("decline: %v %v", moved, err)
	}
	if got, _, _ := st.InterestByID(ctx, other.ID); got.State != InterestDeclined {
		t.Fatalf("declined = %+v", got)
	}
}

// The companion session is where the user answers spore; refinement must
// not review it as a chat (spec PR 1 parked item).
func TestIdleSessionsSkipsTheCompanionSession(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	comp, _ := st.CreateSessionFrom(ctx, "spore", "", SourceCompanion)
	chat, _ := st.CreateSessionFrom(ctx, "chat", "", SourceChat)
	for _, id := range []string{comp, chat} {
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: []byte(`[{"type":"text","text":"hi"}]`)}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := st.IdleSessions(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != chat {
		t.Fatalf("idle = %v, want only the chat %s", ids, chat)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'Outreach|CompanionState|JobProfile|InterestHelpers|IdleSessionsSkips' -v`
Expected: compile errors (undefined `Outreach`, `AddOutreach`, `Job.Profile`, …).

- [ ] **Step 3: Implement**

Append to the schema string in `internal/store/schema.go`, after the `interest_signals` index:

```sql
-- outreach is every message the companion started on its own. Only the
-- governor and the reflection read it. An outreach outlives the interest it
-- was about: deleting an interest only clears the link.
CREATE TABLE IF NOT EXISTS outreach (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  interest_id INTEGER REFERENCES interests(id) ON DELETE SET NULL,
  kind        TEXT NOT NULL,
  channel     TEXT NOT NULL DEFAULT '',
  text        TEXT NOT NULL,
  payload     TEXT NOT NULL DEFAULT '',
  sent_at     TEXT NOT NULL,
  outcome     TEXT NOT NULL DEFAULT 'pending',
  nudge_of    INTEGER REFERENCES outreach(id) ON DELETE SET NULL,
  drop_reason TEXT NOT NULL DEFAULT '',
  resolved_at TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_outreach_outcome ON outreach(outcome, sent_at);
CREATE INDEX IF NOT EXISTS idx_outreach_nudge ON outreach(nudge_of);

-- companion_state holds the companion's singletons: its session, the
-- governor's budget, a pause, and the last reflection's result.
CREATE TABLE IF NOT EXISTS companion_state (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
```

Create `internal/store/outreach.go`:

```go
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Outreach kinds: what the companion said on its own. PR 2a sends proposals
// and nudges; the others arrive with research and the digest.
const (
	OutreachProposal = "proposal"
	OutreachNudge    = "nudge"
	OutreachAlert    = "alert"
	OutreachFinding  = "finding"
	OutreachFollowup = "followup"
	OutreachDigest   = "digest"
)

// Outreach outcomes. See the companion spec, section 2.3: a message nobody
// answered is unanswered, never ignored, until reflection decides.
const (
	OutcomePending    = "pending"
	OutcomeUnanswered = "unanswered"
	OutcomeReplied    = "replied"
	OutcomeReacted    = "reacted"
	OutcomeIgnored    = "ignored"
	OutcomeStop       = "stop"
)

// Outreach channels: where a message was delivered. Fallback is the
// terminal after a Discord send failed.
const (
	OutreachChannelDiscord  = "discord"
	OutreachChannelTerminal = "terminal"
	OutreachChannelFallback = "terminal-fallback"
)

// Outreach is one message the companion started.
type Outreach struct {
	ID int64
	// InterestID is the interest it was about, or 0.
	InterestID int64
	Kind       string
	Channel    string
	Text       string
	// Payload is JSON the kind needs later: a proposal's watch.
	Payload string
	SentAt  time.Time
	Outcome string
	// NudgeOf is the outreach a nudge follows up, or 0.
	NudgeOf    int64
	DropReason string
	ResolvedAt time.Time
}

const outreachColumns = `id, COALESCE(interest_id, 0), kind, channel, text, payload, sent_at, outcome,
       COALESCE(nudge_of, 0), drop_reason, resolved_at`

func scanOutreach(r rowScanner) (Outreach, error) {
	var o Outreach
	var sent, resolved string
	if err := r.Scan(&o.ID, &o.InterestID, &o.Kind, &o.Channel, &o.Text, &o.Payload, &sent, &o.Outcome,
		&o.NudgeOf, &o.DropReason, &resolved); err != nil {
		return Outreach{}, err
	}
	o.SentAt, _ = time.Parse(timeFormat, sent)
	o.ResolvedAt, _ = time.Parse(timeFormat, resolved)
	return o, nil
}

// nullID stores 0 as NULL, so an outreach about no interest carries no
// foreign key to break.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// AddOutreach records a message as sent and pending.
func (s *Store) AddOutreach(ctx context.Context, o Outreach) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO outreach (interest_id, kind, channel, text, payload, sent_at, outcome, nudge_of)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		nullID(o.InterestID), o.Kind, o.Channel, o.Text, o.Payload, o.SentAt.UTC().Format(timeFormat),
		OutcomePending, nullID(o.NudgeOf))
	if err != nil {
		return 0, fmt.Errorf("add outreach: %w", err)
	}
	return res.LastInsertId()
}

// SetOutreachChannel records where a message was finally delivered.
func (s *Store) SetOutreachChannel(ctx context.Context, id int64, channel string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE outreach SET channel = ? WHERE id = ?`, channel, id); err != nil {
		return fmt.Errorf("set outreach %d channel: %w", id, err)
	}
	return nil
}

// OutreachByID reads one outreach; false when there is none.
func (s *Store) OutreachByID(ctx context.Context, id int64) (Outreach, bool, error) {
	o, err := scanOutreach(s.db.QueryRowContext(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Outreach{}, false, nil
	}
	if err != nil {
		return Outreach{}, false, fmt.Errorf("read outreach %d: %w", id, err)
	}
	return o, true, nil
}

func (s *Store) queryOutreach(ctx context.Context, q string, args ...any) ([]Outreach, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read outreach: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Outreach
	for rows.Next() {
		o, err := scanOutreach(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// OutreachSince lists outreach sent at or after since, oldest first.
func (s *Store) OutreachSince(ctx context.Context, since time.Time) ([]Outreach, error) {
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE sent_at >= ? ORDER BY id`,
		since.UTC().Format(timeFormat))
}

// OutreachByOutcome lists outreach in any of the given outcomes, oldest first.
func (s *Store) OutreachByOutcome(ctx context.Context, outcomes ...string) ([]Outreach, error) {
	if len(outcomes) == 0 {
		return nil, nil
	}
	args := make([]any, len(outcomes))
	for i, o := range outcomes {
		args[i] = o
	}
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach WHERE outcome IN (?`+
		strings.Repeat(`, ?`, len(outcomes)-1)+`) ORDER BY id`, args...)
}

// NudgeFor returns the nudge that follows up id, if there is one. There is
// at most one: reflection may nudge a message once.
func (s *Store) NudgeFor(ctx context.Context, id int64) (Outreach, bool, error) {
	o, err := scanOutreach(s.db.QueryRowContext(ctx,
		`SELECT `+outreachColumns+` FROM outreach WHERE nudge_of = ? ORDER BY id LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Outreach{}, false, nil
	}
	if err != nil {
		return Outreach{}, false, fmt.Errorf("read nudge of %d: %w", id, err)
	}
	return o, true, nil
}

// ResolveOutreach moves an outreach to outcome `to` when its outcome is one
// of from, and reports whether it moved. The from check makes a second,
// racing resolution -- two button presses, a reply and a timeout -- a no-op.
func (s *Store) ResolveOutreach(ctx context.Context, id int64, from []string, to, reason string, at time.Time) (bool, error) {
	if len(from) == 0 {
		return false, errors.New("resolve outreach: no outcome to move from")
	}
	args := []any{to, reason, at.UTC().Format(timeFormat), id}
	for _, f := range from {
		args = append(args, f)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE outreach SET outcome = ?, drop_reason = ?, resolved_at = ? WHERE id = ? AND outcome IN (?`+
			strings.Repeat(`, ?`, len(from)-1)+`)`, args...)
	if err != nil {
		return false, fmt.Errorf("resolve outreach %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// CompanionState reads one companion singleton; false when it was never set.
func (s *Store) CompanionState(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM companion_state WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read companion state %s: %w", key, err)
	}
	return v, true, nil
}

// SetCompanionState writes one companion singleton.
func (s *Store) SetCompanionState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO companion_state (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, nowString())
	if err != nil {
		return fmt.Errorf("write companion state %s: %w", key, err)
	}
	return nil
}
```

In `internal/store/jobs.go`:

1. Add to `Job`, after `CheckedIn`:

```go
	// Profile is the policy profile the job's runs work under: "local" for
	// a job a person made, "companion" for a watch the companion created.
	Profile string
```

2. In `migrateJobColumns`, add `{"profile", `TEXT NOT NULL DEFAULT 'local'`},` to the column list.

3. Replace `CreateJob`'s body with:

```go
func (s *Store) CreateJob(ctx context.Context, j Job) (int64, error) {
	enabled := 0
	if j.Enabled {
		enabled = 1
	}
	// A caller that already knows how runs are reported (a companion watch)
	// says so; everyone else starts at ask, before the first-run check-in.
	notify := j.Notify
	if !ValidNotify(notify) {
		notify = NotifyAsk
	}
	checkedIn := 0
	if j.CheckedIn {
		checkedIn = 1
	}
	profile := j.Profile
	if profile == "" {
		profile = "local"
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (kind, spec, prompt, enabled, next_run, last_session_id, created_at, origin_session_id, notify, checked_in, profile)
		 VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?)`,
		j.Kind, j.Spec, j.Prompt, enabled,
		j.NextRun.UTC().Format(timeFormat), time.Now().UTC().Format(timeFormat),
		j.OriginSessionID, notify, checkedIn, profile)
	if err != nil {
		return 0, fmt.Errorf("create job: %w", err)
	}
	return res.LastInsertId()
}
```

4. Append `, profile` to `jobColumns` and scan it last in `scanJobs` (`&j.OriginSessionID, &j.Notify, &checkedIn, &j.Profile`).

5. Add:

```go
// UpdateJobSchedule changes a job's schedule and prompt and moves its next
// run to match. The companion uses it when the user adjusts a watch.
func (s *Store) UpdateJobSchedule(ctx context.Context, id int64, spec, prompt string, next time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE jobs SET spec = ?, prompt = ?, next_run = ? WHERE id = ?`,
		spec, prompt, next.UTC().Format(timeFormat), id)
	if err != nil {
		return fmt.Errorf("update job %d schedule: %w", id, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("no job %d", id)
	}
	return nil
}
```

In `internal/store/interests.go`, add:

```go
// InterestByID returns the interest with id, and false when there is none.
func (s *Store) InterestByID(ctx context.Context, id int64) (Interest, bool, error) {
	x, err := scanInterest(s.db.QueryRowContext(ctx, interestSelect+` WHERE i.id = ? GROUP BY i.id`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Interest{}, false, nil
	}
	if err != nil {
		return Interest{}, false, err
	}
	return x, true, nil
}

// InterestDays lists the distinct local days an interest was seen on,
// oldest first: the evidence a proposal cites.
func (s *Store) InterestDays(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT day FROM interest_signals WHERE interest_id = ? ORDER BY day`, id)
	if err != nil {
		return nil, fmt.Errorf("interest %d days: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// InterestDaysSince counts the distinct local days an interest was seen on
// from sinceDay ("2006-01-02") on. Days are compared as strings, which is
// date order for this format.
func (s *Store) InterestDaysSince(ctx context.Context, id int64, sinceDay string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT day) FROM interest_signals WHERE interest_id = ? AND day >= ?`, id, sinceDay).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("interest %d days since %s: %w", id, sinceDay, err)
	}
	return n, nil
}

// SetInterestCooldown holds an interest back from being proposed until until.
func (s *Store) SetInterestCooldown(ctx context.Context, id int64, until time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE interests SET cooldown_until = ?, updated_at = ? WHERE id = ?`,
		until.UTC().Format(timeFormat), nowString(), id)
	if err != nil {
		return fmt.Errorf("set interest %d cooldown: %w", id, err)
	}
	return nil
}

// ActivateInterest moves a proposed interest to active with its watch job,
// and reports whether it did. Only a proposed interest moves: the user's Yes
// answers a proposal, nothing else.
func (s *Store) ActivateInterest(ctx context.Context, id, jobID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, watch_job_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		InterestActive, jobID, nowString(), id, InterestProposed)
	if err != nil {
		return false, fmt.Errorf("activate interest %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DeclineInterest moves a proposed interest to declined and stamps when.
func (s *Store) DeclineInterest(ctx context.Context, id int64) (bool, error) {
	now := nowString()
	res, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, declined_at = ?, updated_at = ? WHERE id = ? AND state = ?`,
		InterestDeclined, now, now, id, InterestProposed)
	if err != nil {
		return false, fmt.Errorf("decline interest %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
```

In `internal/store/refine.go`, change `IdleSessions`: the query's `s.source NOT IN (?, ?)` becomes `s.source NOT IN (?, ?, ?)`, the args gain `SourceCompanion` after `SourceJob`, and the doc comment gains: "The companion session is excluded too: it is where the user answers spore, and its turns are spore's own outreach."

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/`
Expected: PASS (the new tests and every existing store test).

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "store: outreach, companion state, job profile, interest helpers; idle sweep skips the companion"
```

---

### Task 3: Recorder: 30-day recency, retire interests whose watch is gone

**Files:**
- Modify: `internal/companion/recorder.go`
- Create: `internal/companion/sweep_test.go`

**Interfaces:**
- Consumes: `Store.InterestDaysSince`, `Store.Job` (Task 2).
- Produces: `companion.RecentWindow` (= `FadeAfter`); `(*Recorder).Sweep(ctx, now)` keeps its signature and now also retires active interests whose watch job is disabled or missing.

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/sweep_test.go`:

```go
package companion_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func sweepFixture(t *testing.T) (*store.Store, *config.Config, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	cfg.Companion.Timezone = "UTC"
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sid, err := st.CreateSessionFrom(context.Background(), "t", "", store.SourceChat)
	if err != nil {
		t.Fatal(err)
	}
	return st, cfg, sid
}

func sight(t *testing.T, st *store.Store, id int64, sid string, at time.Time) {
	t.Helper()
	if err := st.AddInterestSignal(context.Background(), id, sid, "asked", at.Format("2006-01-02"), at); err != nil {
		t.Fatal(err)
	}
}

func TestSweepCountsOnlyRecentDays(t *testing.T) {
	ctx := context.Background()
	st, cfg, sid := sweepFixture(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -45))
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -44))
	sight(t, st, in.ID, sid, now)
	r := companion.NewRecorder(st, cfg)
	if err := r.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestObserving || got.DaysSeen != 3 {
		t.Fatalf("after old evidence + one day = %s with %d days, want observing (one recent day)", got.State, got.DaysSeen)
	}
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -1))
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -2))
	if err := r.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("three recent days = %s, want candidate", got.State)
	}
}

func TestSweepDemotesACandidateWhoseEvidenceAgedOut(t *testing.T) {
	ctx := context.Background()
	st, cfg, sid := sweepFixture(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	in, _ := st.TouchInterest(ctx, "topic:go", "Go")
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -29))
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -28))
	sight(t, st, in.ID, sid, now.AddDate(0, 0, -1))
	r := companion.NewRecorder(st, cfg)
	if err := r.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "topic:go"); got.State != store.InterestCandidate {
		t.Fatalf("got %s, want candidate", got.State)
	}
	if err := r.Sweep(ctx, now.AddDate(0, 0, 2)); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "topic:go"); got.State != store.InterestObserving {
		t.Fatalf("two days later = %s, want observing (only two recent days)", got.State)
	}
}

func TestSweepRetiresAnActiveInterestWhoseWatchIsGone(t *testing.T) {
	ctx := context.Background()
	st, cfg, _ := sweepFixture(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	jobID, err := st.CreateJob(ctx, store.Job{Kind: "cron", Spec: "0 14 * * *", Prompt: "w", Enabled: true, NextRun: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	gone, _ := st.TouchInterest(ctx, "stock:net", "NET")
	for _, x := range []struct {
		in  store.Interest
		job int64
	}{{kept, jobID}, {gone, 0}} {
		_, _ = st.SetInterestState(ctx, x.in.ID, store.InterestObserving, store.InterestProposed)
		if ok, err := st.ActivateInterest(ctx, x.in.ID, x.job); err != nil || !ok {
			t.Fatalf("activate %s: %v %v", x.in.Key, ok, err)
		}
	}
	r := companion.NewRecorder(st, cfg)
	if err := r.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestActive {
		t.Fatalf("a running watch = %s, want active", got.State)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:net"); got.State != store.InterestRetired {
		t.Fatalf("no watch job = %s, want retired", got.State)
	}
	if err := st.SetJobEnabled(ctx, jobID, false); err != nil {
		t.Fatal(err)
	}
	if err := r.Sweep(ctx, now); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestRetired {
		t.Fatalf("a cancelled watch = %s, want retired", got.State)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/companion/ -run 'TestSweep' -v`
Expected: `TestSweepCountsOnlyRecentDays` FAILs (candidate after old evidence) and `TestSweepRetiresAnActiveInterestWhoseWatchIsGone` FAILs (still active).

- [ ] **Step 3: Implement**

In `internal/companion/recorder.go`, add after `FadeAfter`:

```go
// RecentWindow is how far back evidence counts toward a habit. It matches
// FadeAfter: an interest that went quiet for a month starts over, so two
// sightings in August and one today are not a habit.
const RecentWindow = FadeAfter
```

Replace `Sweep` with:

```go
// Sweep moves interests to match their evidence: an observing or candidate
// interest retires after FadeAfter without a signal, becomes a candidate
// once seen on habit_days distinct days within RecentWindow (unless cooling
// down), and goes back to observing if those recent days drop below the
// bar. An active interest whose watch job was cancelled or deleted retires:
// the user stopped the watch.
func (r *Recorder) Sweep(ctx context.Context, now time.Time) error {
	loc, err := r.Cfg.Companion.Location()
	if err != nil {
		return err
	}
	since := now.Add(-RecentWindow).In(loc).Format("2006-01-02")
	rows, err := r.Store.Interests(ctx, store.InterestObserving, store.InterestCandidate)
	if err != nil {
		return err
	}
	habit := r.Cfg.Companion.HabitDays
	for _, in := range rows {
		recent, err := r.Store.InterestDaysSince(ctx, in.ID, since)
		if err != nil {
			return err
		}
		to := in.State
		switch {
		case !in.LastSeen.IsZero() && now.Sub(in.LastSeen) > FadeAfter:
			to = store.InterestRetired
		case in.State == store.InterestObserving && recent >= habit && !now.Before(in.CooldownUntil):
			to = store.InterestCandidate
		case in.State == store.InterestCandidate && recent < habit:
			to = store.InterestObserving
		}
		if to == in.State {
			continue
		}
		if _, err := r.Store.SetInterestState(ctx, in.ID, in.State, to); err != nil {
			return err
		}
	}
	return r.retireStoppedWatches(ctx)
}

// retireStoppedWatches retires every active interest whose watch job is
// disabled or gone.
func (r *Recorder) retireStoppedWatches(ctx context.Context) error {
	rows, err := r.Store.Interests(ctx, store.InterestActive)
	if err != nil {
		return err
	}
	for _, in := range rows {
		job, found, err := r.Store.Job(ctx, in.WatchJobID)
		if err != nil {
			return err
		}
		if found && job.Enabled {
			continue
		}
		if _, err := r.Store.SetInterestState(ctx, in.ID, store.InterestActive, store.InterestRetired); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/companion/ ./internal/refine/`
Expected: PASS. If an existing recorder test relied on all-time counting (signals dated far before its `now`), move those signals inside the 30-day window and note it in the commit message.

- [ ] **Step 5: Commit**

```bash
git add internal/companion/
git commit -m "companion: count only the last 30 days toward a habit; retire interests whose watch stopped"
```

---
### Task 4: Pure decision logic: governor, quiet hours, watch validation, reflection

**Files:**
- Create: `internal/companion/governor.go`, `internal/companion/quiet.go`, `internal/companion/watch.go`, `internal/companion/reflect.go`
- Create: `internal/companion/logic_test.go`, `internal/companion/reflect_test.go`

**Interfaces:**
- Consumes: `config.ParseQuietHours`, `scheduler.Parse` (existing), store types from Task 2.
- Produces:
  - `BudgetFloor`; `ApplyOutcome(budget float64, outcome string, start int) float64`; `ApplyFeedback(budget float64, kind string, start int) float64`; `FeedbackMore|FeedbackLess|FeedbackStop`; `FeedbackKinds []string`; unexported `clampBudget`.
  - `InQuietHours(now time.Time, loc *time.Location, spec string) (bool, error)`; `LocalDayStart(now time.Time, loc *time.Location) time.Time`.
  - `Watch{Cron, Goal string}` (JSON `cron`, `goal`); `ValidateCron(spec string, now time.Time) error`; `ValidateGoal(goal string) error`; `ValidateWatch(w Watch, now time.Time) error`; `WatchPrompt(label, goal string) string`.
  - `ActionPropose|ActionNudge|ActionNone`; `Actions []string`; `Drop{Outreach int64; Reason string}`; `Decision{Action, Interest, Message string; Watch *Watch; SelfMD *string; NudgeOf int64; Drops []Drop}`; `View{…}` (fields below); `ParseDecision(text string) (Decision, error)`; `Validate(d Decision, v View) error`; `ReflectionPrompt string`; `RenderInput(v View) string`; `Fingerprint(v View) string`; `Eligible(v View) bool`; `(View).Interest(key) (store.Interest, bool)`; `(View).UnansweredByID(id) (store.Outreach, bool)`; unexported `clip(s string, n int) string`.

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/logic_test.go`:

```go
package companion_test

import (
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

func TestGovernor(t *testing.T) {
	const start = 4
	for _, tc := range []struct {
		name     string
		budget   float64
		outcomes []string
		want     float64
	}{
		{"reply never exceeds start", 4, []string{store.OutcomeReplied}, 4},
		{"reply raises", 2, []string{store.OutcomeReplied, store.OutcomeReacted}, 2.5},
		{"ignored lowers", 4, []string{store.OutcomeIgnored, store.OutcomeIgnored}, 3},
		{"stop halves", 4, []string{store.OutcomeStop}, 2},
		{"floor holds", 1.2, []string{store.OutcomeIgnored, store.OutcomeStop}, 1},
		{"unanswered changes nothing", 3, []string{store.OutcomeUnanswered, store.OutcomePending}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.budget
			for _, o := range tc.outcomes {
				b = companion.ApplyOutcome(b, o, start)
			}
			if b != tc.want {
				t.Fatalf("budget = %v, want %v", b, tc.want)
			}
		})
	}
	if got := companion.ApplyFeedback(3, companion.FeedbackMore, start); got != 4 {
		t.Errorf("more from 3 = %v, want 4", got)
	}
	if got := companion.ApplyFeedback(4, companion.FeedbackMore, start); got != 4 {
		t.Errorf("more from 4 = %v, want 4 (capped)", got)
	}
	if got := companion.ApplyFeedback(1.5, companion.FeedbackLess, start); got != 1 {
		t.Errorf("less from 1.5 = %v, want 1 (floor)", got)
	}
	if got := companion.ApplyFeedback(4, companion.FeedbackStop, start); got != 2 {
		t.Errorf("stop from 4 = %v, want 2", got)
	}
}

func TestQuietHours(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	at := func(h, m int) time.Time { return time.Date(2026, 10, 12, h, m, 0, 0, la) }
	for _, tc := range []struct {
		spec string
		now  time.Time
		want bool
	}{
		{"22:00-08:00", at(23, 30), true},
		{"22:00-08:00", at(2, 0), true},
		{"22:00-08:00", at(7, 59), true},
		{"22:00-08:00", at(8, 0), false},
		{"22:00-08:00", at(12, 0), false},
		{"22:00-08:00", at(22, 0), true},
		{"13:00-14:00", at(13, 30), true},
		{"13:00-14:00", at(14, 0), false},
	} {
		// The clock is read in la even when now arrives in UTC.
		got, err := companion.InQuietHours(tc.now.UTC(), la, tc.spec)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%s at %s = %v, want %v", tc.spec, tc.now.Format("15:04"), got, tc.want)
		}
	}
	if _, err := companion.InQuietHours(at(1, 0), la, "nonsense"); err == nil {
		t.Error("a bad spec must be an error")
	}
	start := companion.LocalDayStart(at(1, 30).UTC(), la)
	if !start.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, la)) {
		t.Errorf("day start = %v", start)
	}
}

func TestValidateWatch(t *testing.T) {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	ok := companion.Watch{Cron: "0 14 * * 1-5", Goal: "Check ZS; alert on a daily move over 5%."}
	if err := companion.ValidateWatch(ok, now); err != nil {
		t.Fatalf("valid watch: %v", err)
	}
	if err := companion.ValidateWatch(companion.Watch{Cron: "0 * * * *", Goal: "g"}, now); err != nil {
		t.Fatalf("hourly is allowed: %v", err)
	}
	for _, tc := range []struct {
		name string
		w    companion.Watch
		want string
	}{
		{"every 30 min", companion.Watch{Cron: "*/30 * * * *", Goal: "g"}, "more than once an hour"},
		{"twice in one hour once a week", companion.Watch{Cron: "0,30 9 * * 1", Goal: "g"}, "more than once an hour"},
		{"bad cron", companion.Watch{Cron: "every day", Goal: "g"}, "cron"},
		{"one-off", companion.Watch{Cron: "2026-11-01T10:00:00Z", Goal: "g"}, "recurring"},
		{"empty goal", companion.Watch{Cron: "0 14 * * *", Goal: "  "}, "goal"},
		{"long goal", companion.Watch{Cron: "0 14 * * *", Goal: strings.Repeat("x", 501)}, "goal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := companion.ValidateWatch(tc.w, now)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	if p := companion.WatchPrompt("Zscaler (ZS)", " Check ZS. "); !strings.Contains(p, "Zscaler (ZS)") || !strings.Contains(p, "Goal: Check ZS.") {
		t.Errorf("prompt = %q", p)
	}
}
```

Create `internal/companion/reflect_test.go`:

```go
package companion_test

import (
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

func reflectView() companion.View {
	now := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	return companion.View{
		Now: now, Loc: time.UTC, SelfReadable: true, SelfMax: 10240, BudgetLeft: 2,
		Interests: []store.Interest{
			{ID: 1, Key: "stock:zs", Label: "Zscaler (ZS)", State: store.InterestCandidate},
			{ID: 2, Key: "topic:go", Label: "Go", State: store.InterestCandidate, CooldownUntil: now.Add(48 * time.Hour)},
			{ID: 3, Key: "stock:net", Label: "NET", State: store.InterestActive},
		},
		Days:       map[int64][]string{1: {"2026-10-09", "2026-10-10", "2026-10-11"}},
		Keys:       map[int64]string{1: "stock:zs", 2: "topic:go", 3: "stock:net"},
		Unanswered: []store.Outreach{{ID: 10, Kind: store.OutreachProposal, Text: "watch NET?", SentAt: now.Add(-13 * time.Hour), Outcome: store.OutcomeUnanswered}, {ID: 11, Kind: store.OutreachProposal, Text: "old", SentAt: now.Add(-30 * time.Hour), Outcome: store.OutcomeUnanswered}},
		Nudged:     map[int64]bool{11: true},
	}
}

const goodPropose = `{"action":"propose","interest":"stock:zs","message":"You've checked ZS three days running. Want me to watch it?","watch":{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a move over 5%."}}`

func TestParseDecision(t *testing.T) {
	d, err := companion.ParseDecision("Here you go:\n```json\n" + goodPropose + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if d.Action != companion.ActionPropose || d.Interest != "stock:zs" || d.Watch == nil || d.Watch.Cron != "0 14 * * 1-5" {
		t.Fatalf("decision = %+v", d)
	}
	if _, err := companion.ParseDecision("I think we should wait."); err == nil {
		t.Fatal("prose with no object must be an error")
	}
	if _, err := companion.ParseDecision(`{"action": "none"`); err == nil {
		t.Fatal("a truncated object must be an error")
	}
}

func TestValidateDecision(t *testing.T) {
	selfOK := "## What I'm curious about\n\n## Threads with you\n\n## How you like to be talked to\n\n## Opinions I've formed\n"
	selfMissing := "## What I'm curious about\n"
	for _, tc := range []struct {
		name string
		json string
		edit func(*companion.View)
		want string // "" means valid
	}{
		{"propose", goodPropose, nil, ""},
		{"none", `{"action":"none"}`, nil, ""},
		{"none with no budget", `{"action":"none"}`, func(v *companion.View) { v.BudgetLeft = 0 }, ""},
		{"nudge", `{"action":"nudge","nudge_of":10,"message":"Still want that NET watch? Yes or no is fine."}`, nil, ""},
		{"drop", `{"action":"none","drops":[{"outreach":10,"reason":"the earnings moment passed"}]}`, nil, ""},
		{"self rewrite", `{"action":"none","self_md":` + quote(selfOK) + `}`, nil, ""},
		{"unknown action", `{"action":"alert","interest":"stock:zs","message":"down 6%"}`, nil, "action"},
		{"unknown interest", strings.Replace(goodPropose, "stock:zs", "stock:nope", 1), nil, "no interest"},
		{"not candidate", strings.Replace(goodPropose, "stock:zs", "stock:net", 1), nil, "not candidate"},
		{"cooling down", strings.Replace(goodPropose, "stock:zs", "topic:go", 1), nil, "cooling down"},
		{"no watch", `{"action":"propose","interest":"stock:zs","message":"hi"}`, nil, "watch"},
		{"bad cron", strings.Replace(goodPropose, "0 14 * * 1-5", "*/10 * * * *", 1), nil, "more than once an hour"},
		{"empty message", `{"action":"nudge","nudge_of":10,"message":" "}`, nil, "message"},
		{"long message", `{"action":"nudge","nudge_of":10,"message":"` + strings.Repeat("x", 1201) + `"}`, nil, "message"},
		{"no budget", goodPropose, func(v *companion.View) { v.BudgetLeft = 0 }, "budget"},
		{"nudge of nothing", `{"action":"nudge","nudge_of":99,"message":"hi"}`, nil, "not an unanswered"},
		{"second nudge", `{"action":"nudge","nudge_of":11,"message":"hi again"}`, nil, "already has a nudge"},
		{"nudge without id", `{"action":"nudge","message":"hi"}`, nil, "nudge_of"},
		{"drop of nothing", `{"action":"none","drops":[{"outreach":99,"reason":"x"}]}`, nil, "not an unanswered"},
		{"drop without reason", `{"action":"none","drops":[{"outreach":10,"reason":""}]}`, nil, "reason"},
		{"nudged and dropped", `{"action":"nudge","nudge_of":10,"message":"hi","drops":[{"outreach":10,"reason":"x"}]}`, nil, "both"},
		{"self missing heading", `{"action":"none","self_md":` + quote(selfMissing) + `}`, nil, "heading"},
		{"self too large", `{"action":"none","self_md":` + quote(selfOK) + `}`, func(v *companion.View) { v.SelfMax = 20 }, "self_max_bytes"},
		{"self unreadable", `{"action":"none","self_md":` + quote(selfOK) + `}`, func(v *companion.View) { v.SelfReadable = false }, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := reflectView()
			if tc.edit != nil {
				tc.edit(&v)
			}
			d, err := companion.ParseDecision(tc.json)
			if err != nil {
				t.Fatal(err)
			}
			err = companion.Validate(d, v)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid decision refused: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func quote(s string) string {
	b := strings.ReplaceAll(s, "\n", `\n`)
	return `"` + strings.ReplaceAll(b, `"`, `\"`) + `"`
}

func TestRenderInputAndFingerprint(t *testing.T) {
	v := reflectView()
	in := companion.RenderInput(v)
	for _, want := range []string{"stock:zs", "candidate", "2026-10-09, 2026-10-10, 2026-10-11", "cooling down", "#10", "already nudged", "Messages you may still start today: 2"} {
		if !strings.Contains(in, want) {
			t.Errorf("input lacks %q:\n%s", want, in)
		}
	}
	if !companion.Eligible(v) {
		t.Fatal("a candidate and two unanswered rows are eligible")
	}
	fp := companion.Fingerprint(v)
	if fp != companion.Fingerprint(reflectView()) {
		t.Fatal("the fingerprint must be deterministic")
	}
	v2 := reflectView()
	v2.Days[1] = append(v2.Days[1], "2026-10-12")
	if companion.Fingerprint(v2) == fp {
		t.Fatal("new evidence for a candidate must change the fingerprint")
	}
	v3 := reflectView()
	v3.Now = v3.Now.Add(24 * time.Hour)
	if companion.Fingerprint(v3) == fp {
		t.Fatal("a new local day must change the fingerprint")
	}
	empty := companion.View{Now: v.Now, Loc: time.UTC, Interests: []store.Interest{{ID: 2, Key: "topic:go", State: store.InterestCandidate, CooldownUntil: v.Now.Add(time.Hour)}}}
	if companion.Eligible(empty) {
		t.Fatal("only a cooling-down candidate is not eligible")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/companion/ -run 'Governor|QuietHours|ValidateWatch|ParseDecision|ValidateDecision|RenderInput' -v`
Expected: compile errors (undefined `companion.ApplyOutcome`, `companion.View`, …).

- [ ] **Step 3: Implement**

Create `internal/companion/governor.go`:

```go
package companion

import "github.com/codered/spore/internal/store"

// BudgetFloor is the fewest unprompted messages a day the governor allows.
// At the floor spore sends little beyond what the user approved.
const BudgetFloor = 1.0

// Feedback kinds companion_feedback accepts.
const (
	FeedbackMore = "more"
	FeedbackLess = "less"
	FeedbackStop = "stop"
)

// FeedbackKinds is the closed set companion_feedback accepts.
var FeedbackKinds = []string{FeedbackMore, FeedbackLess, FeedbackStop}

// ApplyOutcome is the governor's response to one resolved outreach: an
// answer raises the budget a little, silence lowers it more, and "stop"
// halves it. Pending and unanswered change nothing: unanswered is not yet a
// verdict.
func ApplyOutcome(budget float64, outcome string, start int) float64 {
	switch outcome {
	case store.OutcomeReplied, store.OutcomeReacted:
		budget += 0.25
	case store.OutcomeIgnored:
		budget -= 0.5
	case store.OutcomeStop:
		budget /= 2
	}
	return clampBudget(budget, start)
}

// ApplyFeedback is the governor's response to the user saying more, less or
// stop in the companion conversation. It moves faster than ApplyOutcome:
// the user said it outright.
func ApplyFeedback(budget float64, kind string, start int) float64 {
	switch kind {
	case FeedbackMore:
		budget++
	case FeedbackLess:
		budget--
	case FeedbackStop:
		budget /= 2
	}
	return clampBudget(budget, start)
}

func clampBudget(b float64, start int) float64 {
	return max(BudgetFloor, min(b, float64(start)))
}
```

Create `internal/companion/quiet.go`:

```go
package companion

import (
	"time"

	"github.com/codered/spore/internal/config"
)

// InQuietHours reports whether now falls inside the "HH:MM-HH:MM" range,
// read on the clock in loc. The range may wrap midnight; its start is inside
// it and its end is not.
func InQuietHours(now time.Time, loc *time.Location, spec string) (bool, error) {
	start, end, err := config.ParseQuietHours(spec)
	if err != nil {
		return false, err
	}
	l := now.In(loc)
	m := l.Hour()*60 + l.Minute()
	if start < end {
		return m >= start && m < end, nil
	}
	return m >= start || m < end, nil
}

// LocalDayStart is midnight at the start of now's calendar day in loc: where
// the daily budget resets.
func LocalDayStart(now time.Time, loc *time.Location) time.Time {
	l := now.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}
```

Create `internal/companion/watch.go`:

```go
package companion

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codered/spore/internal/scheduler"
)

// Watch is a proposal's watch: when the job runs and what it checks.
type Watch struct {
	Cron string `json:"cron"`
	Goal string `json:"goal"`
}

// cronProbe is how many firings ValidateCron inspects. Every sub-hour gap a
// five-field cron can have shows up within its first day of firings, and a
// weekly pattern's first occurrence is reached well inside fifty.
const cronProbe = 50

// ValidateCron checks a watch schedule: a recurring five-field cron the
// scheduler can parse (it runs in UTC), firing at most once an hour.
func ValidateCron(spec string, now time.Time) error {
	sched, err := scheduler.Parse(spec)
	if err != nil {
		return fmt.Errorf("watch cron %q: %w", spec, err)
	}
	if sched.Kind() != "cron" {
		return fmt.Errorf("watch cron %q must be a recurring five-field cron, not a one-off time", spec)
	}
	prev := sched.Next(now.UTC())
	if prev.IsZero() {
		return fmt.Errorf("watch cron %q never fires", spec)
	}
	for i := 0; i < cronProbe; i++ {
		next := sched.Next(prev)
		if next.IsZero() {
			break
		}
		if next.Sub(prev) < time.Hour {
			return fmt.Errorf("watch cron %q fires more than once an hour", spec)
		}
		prev = next
	}
	return nil
}

// ValidateGoal checks what a watch is told to do: 1..500 characters.
func ValidateGoal(goal string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(goal)); n < 1 || n > 500 {
		return fmt.Errorf("watch goal must be 1..500 characters, got %d", n)
	}
	return nil
}

// ValidateWatch checks a whole watch.
func ValidateWatch(w Watch, now time.Time) error {
	if err := ValidateGoal(w.Goal); err != nil {
		return err
	}
	return ValidateCron(w.Cron, now)
}

// WatchPrompt is what a watch job runs: the goal, framed so the run knows it
// is unattended and read-only, and that "nothing to report" is an answer.
func WatchPrompt(label, goal string) string {
	return fmt.Sprintf("You are running a watch the user approved, on %s. Goal: %s\n\n"+
		"You run unattended, with read-only tools and your own notes. Report what you found in a few lines. "+
		"If nothing crosses the goal's bar, say so in one line. If you need a tool you do not have, say which and why.",
		label, strings.TrimSpace(goal))
}
```

Create `internal/companion/reflect.go`:

```go
package companion

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codered/spore/internal/store"
)

// Reflection actions. PR 2a acts on these three; alerts, findings,
// follow-ups, the digest and research arrive in PR 3.
const (
	ActionPropose = "propose"
	ActionNudge   = "nudge"
	ActionNone    = "none"
)

// Actions is every action a reflection may return.
var Actions = []string{ActionPropose, ActionNudge, ActionNone}

// Drop is an unanswered message the reflection chose not to follow up, and
// why. Dropping is a decision written down, never a timeout.
type Drop struct {
	Outreach int64  `json:"outreach"`
	Reason   string `json:"reason"`
}

// Decision is one reflection's answer. Nothing in it is trusted until
// Validate has checked it against the View it was made from.
type Decision struct {
	Action   string `json:"action"`
	Interest string `json:"interest"`
	Message  string `json:"message"`
	Watch    *Watch `json:"watch"`
	// SelfMD, when set, replaces self.md whole.
	SelfMD  *string `json:"self_md"`
	NudgeOf int64   `json:"nudge_of"`
	Drops   []Drop  `json:"drops"`
}

// View is what a reflection sees, and what its decision is checked against.
type View struct {
	Now time.Time
	Loc *time.Location
	// Soul is soul.md, the user's file; Self is self.md, spore's own.
	Soul string
	Self string
	// SelfReadable is false when self.md exists but could not be read: the
	// reflection may not rewrite a file it could not see.
	SelfReadable bool
	SelfMax      int
	// Interests are the candidate, proposed and active ones.
	Interests []store.Interest
	// Days lists each interest's sighting days; Keys names every interest
	// by id, so an outreach about a retired one still shows its key.
	Days map[int64][]string
	Keys map[int64]string
	// Recent is the last week's outreach; Unanswered is every unanswered
	// row, however old; Nudged marks the unanswered rows already nudged.
	Recent     []store.Outreach
	Unanswered []store.Outreach
	Nudged     map[int64]bool
	// BudgetLeft is how many more messages may start today.
	BudgetLeft int
}

// Interest finds a shown interest by key.
func (v View) Interest(key string) (store.Interest, bool) {
	for _, in := range v.Interests {
		if in.Key == key {
			return in, true
		}
	}
	return store.Interest{}, false
}

// UnansweredByID finds a shown unanswered outreach by id.
func (v View) UnansweredByID(id int64) (store.Outreach, bool) {
	for _, o := range v.Unanswered {
		if o.ID == id {
			return o, true
		}
	}
	return store.Outreach{}, false
}

// ParseDecision reads a reflection reply. It tolerates prose or a code fence
// around one JSON object; a reply with no complete object is an error.
func ParseDecision(text string) (Decision, error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Decision{}, fmt.Errorf("reflection reply has no JSON object: %.200q", text)
	}
	var d Decision
	if err := json.Unmarshal([]byte(text[start:end+1]), &d); err != nil {
		return Decision{}, fmt.Errorf("reflection reply is not valid JSON: %w", err)
	}
	d.Action = strings.TrimSpace(d.Action)
	d.Interest = strings.ToLower(strings.TrimSpace(d.Interest))
	d.Message = strings.TrimSpace(d.Message)
	return d, nil
}

// Validate checks a decision against the view it was made from. Any error
// skips the tick: nothing is sent and nothing is written.
func Validate(d Decision, v View) error {
	switch d.Action {
	case ActionNone:
	case ActionPropose:
		in, ok := v.Interest(d.Interest)
		if !ok {
			return fmt.Errorf("propose: no interest %q", d.Interest)
		}
		if in.State != store.InterestCandidate {
			return fmt.Errorf("propose: %s is %s, not candidate", in.Key, in.State)
		}
		if v.Now.Before(in.CooldownUntil) {
			return fmt.Errorf("propose: %s is cooling down until %s", in.Key, in.CooldownUntil.Format(time.RFC3339))
		}
		if d.Watch == nil {
			return errors.New("propose needs a watch")
		}
		if err := ValidateWatch(*d.Watch, v.Now); err != nil {
			return err
		}
	case ActionNudge:
		if d.NudgeOf == 0 {
			return errors.New("nudge needs nudge_of")
		}
		if _, ok := v.UnansweredByID(d.NudgeOf); !ok {
			return fmt.Errorf("nudge_of %d is not an unanswered outreach", d.NudgeOf)
		}
		if v.Nudged[d.NudgeOf] {
			return fmt.Errorf("outreach %d already has a nudge", d.NudgeOf)
		}
	default:
		return fmt.Errorf("action %q is not one of %s", d.Action, strings.Join(Actions, ", "))
	}
	if d.Action != ActionNone {
		if n := utf8.RuneCountInString(d.Message); n < 1 || n > 1200 {
			return fmt.Errorf("message must be 1..1200 characters, got %d", n)
		}
		if v.BudgetLeft < 1 {
			return errors.New("no budget left today")
		}
	}
	for _, dr := range d.Drops {
		if _, ok := v.UnansweredByID(dr.Outreach); !ok {
			return fmt.Errorf("drop %d is not an unanswered outreach", dr.Outreach)
		}
		if d.Action == ActionNudge && dr.Outreach == d.NudgeOf {
			return fmt.Errorf("outreach %d is both nudged and dropped", dr.Outreach)
		}
		if n := utf8.RuneCountInString(strings.TrimSpace(dr.Reason)); n < 1 || n > 200 {
			return fmt.Errorf("drop %d: reason must be 1..200 characters, got %d", dr.Outreach, n)
		}
	}
	if d.SelfMD != nil {
		if !v.SelfReadable {
			return errors.New("self.md is unavailable this tick and may not be rewritten")
		}
		if len(*d.SelfMD) > v.SelfMax {
			return fmt.Errorf("self_md is %d bytes, over companion.self_max_bytes %d", len(*d.SelfMD), v.SelfMax)
		}
		for _, h := range SelfHeadings {
			if !hasHeading(*d.SelfMD, h) {
				return fmt.Errorf("self_md is missing the heading %q", h)
			}
		}
	}
	return nil
}

func hasHeading(body, h string) bool {
	for _, l := range strings.Split(body, "\n") {
		if strings.TrimSpace(l) == "## "+h {
			return true
		}
	}
	return false
}

// Eligible reports whether a reflection could act on anything: a candidate
// out of its cooldown, or an unanswered message. Nothing eligible means no
// call is made.
func Eligible(v View) bool {
	if len(v.Unanswered) > 0 {
		return true
	}
	for _, in := range v.Interests {
		if in.State == store.InterestCandidate && !v.Now.Before(in.CooldownUntil) {
			return true
		}
	}
	return false
}

// Fingerprint is everything a reflection's answer depends on that changes
// between ticks: the local day, each candidate's evidence and cooldown, the
// unanswered rows and their nudges, and the week's outcomes. An unchanged
// fingerprint means the last answer still stands, so no call is made.
func Fingerprint(v View) string {
	lines := []string{"day " + v.Now.In(v.Loc).Format("2006-01-02")}
	for _, in := range v.Interests {
		if in.State == store.InterestCandidate {
			lines = append(lines, fmt.Sprintf("c %d %d %t", in.ID, len(v.Days[in.ID]), v.Now.Before(in.CooldownUntil)))
		}
	}
	for _, o := range v.Unanswered {
		lines = append(lines, fmt.Sprintf("u %d %t", o.ID, v.Nudged[o.ID]))
	}
	for _, o := range v.Recent {
		lines = append(lines, fmt.Sprintf("o %d %s", o.ID, o.Outcome))
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// ReflectionPrompt is the system prompt of a heartbeat reflection.
const ReflectionPrompt = `You are spore's companion reflection. Spore is a personal assistant that lives on one person's machine. Between conversations you look at what spore has noticed about the user and decide whether to start a conversation.

You may:
- propose watching a candidate interest: a habit the user has shown on several days. Only a candidate that is not cooling down may be proposed. The user answers with buttons: Yes starts the watch, Not now, or Never. Write a short, specific message that says what you noticed, and give a watch: a five-field cron in UTC that fires at most hourly (daily or on weekdays is usual), and a goal saying what to check and when it is worth telling them.
- nudge an unanswered message: one short follow-up that adds something (new information, a simpler question, or an easy way to say no). Never repeat the original. Each message gets at most one nudge.
- do nothing ("none"). This is the usual answer. Starting a conversation costs the user's attention; only do it when it is worth it.

Every unanswered message you are shown needs a decision eventually: nudge it, drop it in "drops" with a short reason (for example, the moment has passed), or leave it out to decide on a later tick.

You may also rewrite your own notes in "self_md": the full new self.md, keeping its four headings. Leave it null unless something is worth recording. "Who you are" is the user's file and wins over your notes.

Reply with one JSON object and nothing else:
{"action": "propose" | "nudge" | "none",
 "interest": "<interest key, for propose>",
 "message": "<what to send, 1..1200 characters; empty for none>",
 "watch": {"cron": "<five-field cron, UTC>", "goal": "<what to check, 1..500 characters>"} or null,
 "nudge_of": <outreach id, for nudge>,
 "drops": [{"outreach": <id>, "reason": "<1..200 characters>"}],
 "self_md": null or "<the full new self.md>"}`

// RenderInput is the user message of a reflection: the state Validate will
// check the answer against, in the order the prompt explains it.
func RenderInput(v View) string {
	var b strings.Builder
	local := v.Now.In(v.Loc)
	fmt.Fprintf(&b, "Local time: %s (%s). UTC: %s.\n", local.Format("Mon 2006-01-02 15:04"), v.Loc, v.Now.UTC().Format("15:04"))
	fmt.Fprintf(&b, "Messages you may still start today: %d.\n\n", v.BudgetLeft)

	b.WriteString("## Who you are (soul.md)\n")
	b.WriteString(orNone(v.Soul))
	b.WriteString("\n\n## Your own notes (self.md)\n")
	switch {
	case !v.SelfReadable:
		b.WriteString("(unavailable this tick: do not rewrite it)")
	default:
		b.WriteString(orNone(v.Self))
	}

	b.WriteString("\n\n## Interests\n")
	if len(v.Interests) == 0 {
		b.WriteString("(none)\n")
	}
	for _, in := range v.Interests {
		fmt.Fprintf(&b, "- %s %q: %s", in.Key, in.Label, in.State)
		if in.State == store.InterestCandidate && v.Now.Before(in.CooldownUntil) {
			fmt.Fprintf(&b, " (cooling down until %s; do not propose)", in.CooldownUntil.In(v.Loc).Format("2006-01-02"))
		}
		if days := v.Days[in.ID]; len(days) > 0 {
			fmt.Fprintf(&b, "; seen on %s", strings.Join(days, ", "))
		}
		b.WriteString("\n")
	}

	b.WriteString("\n## Messages you started in the last 7 days\n")
	if len(v.Recent) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range v.Recent {
		about := ""
		if k := v.Keys[o.InterestID]; k != "" {
			about = " about " + k
		}
		fmt.Fprintf(&b, "- #%d %s%s, %s, sent %s: %q\n", o.ID, o.Kind, about, o.Outcome,
			o.SentAt.In(v.Loc).Format("Mon 15:04"), clip(o.Text, 300))
	}

	b.WriteString("\n## Unanswered: nudge, drop, or leave for later\n")
	if len(v.Unanswered) == 0 {
		b.WriteString("(none)\n")
	}
	for _, o := range v.Unanswered {
		note := ""
		if v.Nudged[o.ID] {
			note = " (already nudged: drop it or leave it)"
		}
		fmt.Fprintf(&b, "- #%d %s, sent %dh ago%s: %q\n", o.ID, o.Kind, int(v.Now.Sub(o.SentAt).Hours()), note, clip(o.Text, 300))
	}
	return b.String()
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(empty)"
	}
	return strings.TrimSpace(s)
}

// clip shortens s to at most n runes, marking the cut.
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/companion/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/companion/
git commit -m "companion: governor, quiet hours, watch validation, reflection parse and validate"
```

---

### Task 5: The heartbeat: state helpers and `Heartbeat.Tick`

**Files:**
- Create: `internal/companion/state.go`, `internal/companion/heartbeat.go`
- Create: `internal/companion/heartbeat_test.go`

**Interfaces:**
- Consumes: Task 2 store methods; Task 3 `Recorder.Sweep`; Task 4 `View`, `ParseDecision`, `Validate`, `RenderInput`, `Fingerprint`, `Eligible`, `ReflectionPrompt`, `InQuietHours`, `LocalDayStart`, `ApplyOutcome`, `clampBudget`, `clip`.
- Produces:
  - State keys: `StateSessionID = "session_id"`, `StateBudget = "daily_budget"`, `StatePausedUntil = "paused_until"`, `StateLastReflection = "last_reflection_at"`, `StateLastResult = "last_reflection_result"`, `StateFailures = "reflection_failures"`, `StateFingerprint = "last_fingerprint"`.
  - `LoadBudget(ctx, *store.Store, start int) (float64, error)`; `SaveBudget(ctx, *store.Store, float64) error`; `PausedUntil(ctx, *store.Store) (time.Time, error)`; `SetPausedUntil(ctx, *store.Store, time.Time) error`; `StateTime(ctx, *store.Store, key string) (time.Time, error)`; `SpentToday(ctx, *store.Store, now time.Time, loc *time.Location) (int, error)`; `Resolve(ctx, *store.Store, start int, id int64, from []string, to, reason string, now time.Time) (bool, error)`; unexported `returnProposal`.
  - Durations: `UnansweredAfter` (12h), `NudgeIgnoredAfter` (24h), `NotNowCooldown` (14d), `ReturnCooldown` (7d), `RecentOutreach` (7d); `FailureWarning = 3`.
  - `ErrBusy` (the deliverer's "companion session has a turn running").
  - `Reflector` interface `{ Reflect(ctx, system, input string) (string, error) }`; `Deliverer` interface `{ EnsureSession(ctx) (string, error); Deliver(ctx, store.Outreach) (store.Outreach, error) }`; `SelfWriter func(ctx, sessionID, body string) error`.
  - `Heartbeat{Store; Cfg; Recorder *Recorder; Reflector; Deliverer; WriteSelf SelfWriter; Now func() time.Time}`; `(*Heartbeat).Tick(ctx) (string, error)`; `(*Heartbeat).Run(ctx, every time.Duration)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/heartbeat_test.go`:

```go
package companion_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type fakeReflector struct {
	replies []string
	err     error
	inputs  []string
}

func (f *fakeReflector) Reflect(_ context.Context, _, input string) (string, error) {
	f.inputs = append(f.inputs, input)
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return `{"action":"none"}`, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

type fakeDeliverer struct {
	st   *store.Store
	sid  string
	sent []store.Outreach
	err  error
}

func (f *fakeDeliverer) EnsureSession(ctx context.Context) (string, error) {
	if f.sid == "" {
		id, err := f.st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
		if err != nil {
			return "", err
		}
		f.sid = id
		if err := f.st.SetCompanionState(ctx, companion.StateSessionID, id); err != nil {
			return "", err
		}
	}
	return f.sid, nil
}

func (f *fakeDeliverer) Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error) {
	if f.err != nil {
		return store.Outreach{}, f.err
	}
	if _, err := f.EnsureSession(ctx); err != nil {
		return store.Outreach{}, err
	}
	o.Channel = store.OutreachChannelTerminal
	id, err := f.st.AddOutreach(ctx, o)
	if err != nil {
		return store.Outreach{}, err
	}
	o.ID = id
	f.sent = append(f.sent, o)
	return o, nil
}

type hbFixture struct {
	h          *companion.Heartbeat
	st         *store.Store
	cfg        *config.Config
	refl       *fakeReflector
	del        *fakeDeliverer
	now        time.Time
	selfWrites []string
}

// newHeartbeat is a heartbeat at 15:00 UTC on a Monday: outside the default
// 22:00-08:00 quiet hours, with the companion on and days counted in UTC.
func newHeartbeat(t *testing.T) *hbFixture {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	cfg.Companion.Timezone = "UTC"
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fx := &hbFixture{st: st, cfg: cfg, refl: &fakeReflector{}, del: &fakeDeliverer{st: st},
		now: time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)}
	fx.h = &companion.Heartbeat{
		Store: st, Cfg: cfg, Recorder: companion.NewRecorder(st, cfg),
		Reflector: fx.refl, Deliverer: fx.del,
		WriteSelf: func(_ context.Context, _, body string) error {
			fx.selfWrites = append(fx.selfWrites, body)
			return nil
		},
		Now: func() time.Time { return fx.now },
	}
	return fx
}

// candidate seeds an interest seen on the three days before now and sweeps
// it to candidate.
func (fx *hbFixture) candidate(t *testing.T, key string) store.Interest {
	t.Helper()
	ctx := context.Background()
	sid, _ := fx.st.CreateSessionFrom(ctx, "t", "", store.SourceChat)
	in, err := fx.st.TouchInterest(ctx, key, "label for "+key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		d := fx.now.AddDate(0, 0, -i)
		if err := fx.st.AddInterestSignal(ctx, in.ID, sid, "asked", d.Format("2006-01-02"), d); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.h.Recorder.Sweep(ctx, fx.now); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, key)
	if got.State != store.InterestCandidate {
		t.Fatalf("seeded %s is %s, want candidate", key, got.State)
	}
	return got
}

func (fx *hbFixture) outcome(t *testing.T, id int64) store.Outreach {
	t.Helper()
	o, ok, err := fx.st.OutreachByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("outreach %d: %v %v", id, ok, err)
	}
	return o
}

func (fx *hbFixture) budget(t *testing.T) float64 {
	t.Helper()
	b, err := companion.LoadBudget(context.Background(), fx.st, fx.cfg.Companion.StartBudget)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const proposeZS = `{"action":"propose","interest":"stock:zs","message":"You've checked ZS three days running. Want me to watch it?","watch":{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a daily move over 5%."}}`

func TestTickDisabledDoesNothing(t *testing.T) {
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.cfg.Companion.Enabled = false
	if res, err := fx.h.Tick(context.Background()); err != nil || res != "disabled" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatal("a disabled companion reflected")
	}
}

func TestTickQuietHoursWrapMidnight(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 12, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 13, 7, 30, 0, 0, time.UTC),
	} {
		fx := newHeartbeat(t)
		fx.candidate(t, "stock:zs")
		fx.now = at
		if res, err := fx.h.Tick(context.Background()); err != nil || res != "quiet hours" {
			t.Fatalf("tick at %s = %q %v", at.Format("15:04"), res, err)
		}
		if len(fx.refl.inputs) != 0 {
			t.Fatal("reflected during quiet hours")
		}
	}
}

func TestTickNothingEligibleMakesNoCall(t *testing.T) {
	fx := newHeartbeat(t)
	if res, err := fx.h.Tick(context.Background()); err != nil || res != "nothing to decide" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatal("reflected with nothing to decide")
	}
}

func TestTickProposes(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	res, err := fx.h.Tick(ctx)
	if err != nil || res != "propose stock:zs" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 {
		t.Fatalf("delivered %d, want 1", len(fx.del.sent))
	}
	o := fx.del.sent[0]
	if o.Kind != store.OutreachProposal || o.InterestID != in.ID || o.Payload != `{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a daily move over 5%."}` {
		t.Fatalf("outreach = %+v", o)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("interest = %s, want proposed", got.State)
	}
	// Nothing is left to decide: the proposal is pending, not unanswered.
	if res, _ := fx.h.Tick(ctx); res != "nothing to decide" {
		t.Fatalf("second tick = %q", res)
	}
	if len(fx.refl.inputs) != 1 {
		t.Fatalf("reflected %d times, want 1", len(fx.refl.inputs))
	}
}

func TestTickInvalidOutputWritesNothingAndCountsFailures(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	bad := `{"action":"propose","interest":"stock:nope","message":"hi","watch":{"cron":"0 14 * * *","goal":"g"}}`
	fx.refl.replies = []string{bad, bad, bad}
	for i := 1; i <= 3; i++ {
		if _, err := fx.h.Tick(ctx); err == nil {
			t.Fatalf("tick %d accepted invalid output", i)
		}
	}
	if len(fx.del.sent) != 0 {
		t.Fatal("invalid output was delivered")
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "3" {
		t.Fatalf("failures = %q, want 3", v)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate untouched", got.State)
	}
	fx.refl.replies = []string{`{"action":"none"}`}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "0" {
		t.Fatalf("failures after a success = %q, want 0", v)
	}
}

func TestTickCallErrorCountsAFailure(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.err = errors.New("provider down")
	if _, err := fx.h.Tick(ctx); err == nil {
		t.Fatal("a failed call must fail the tick")
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "1" {
		t.Fatalf("failures = %q, want 1", v)
	}
}

func TestTickBusyIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	fx.del.err = companion.ErrBusy
	res, err := fx.h.Tick(ctx)
	if err != nil || res != "companion session busy" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v == "1" {
		t.Fatal("a busy session counted as a failed reflection")
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate (nothing was sent)", got.State)
	}
}

func TestTickBudgetSpent(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.cfg.Companion.StartBudget = 1
	fx.candidate(t, "stock:zs")
	if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", SentAt: fx.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.Tick(ctx); err != nil || res != "budget spent" {
		t.Fatalf("tick = %q %v", res, err)
	}
}

func TestTickPaused(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	if err := companion.SetPausedUntil(ctx, fx.st, fx.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.Tick(ctx); err != nil || res != "paused" {
		t.Fatalf("tick = %q %v", res, err)
	}
}

// A pending message becomes unanswered at 12 h -- never ignored -- and stays
// unanswered while reflection leaves it alone.
func TestPendingBecomesUnansweredNotIgnored(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeUnanswered {
		t.Fatalf("outcome = %s, want unanswered", o.Outcome)
	}
	if len(fx.refl.inputs) != 1 {
		t.Fatal("an unanswered message must reach reflection")
	}
	fx.now = fx.now.Add(30 * time.Minute)
	if res, _ := fx.h.Tick(ctx); res != "nothing changed" {
		t.Fatalf("second tick = %q, want nothing changed", res)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeUnanswered {
		t.Fatalf("outcome = %s, want still unanswered", o.Outcome)
	}
	young, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "y", SentAt: fx.now.Add(-11 * time.Hour)})
	_, _ = fx.h.Tick(ctx)
	if o := fx.outcome(t, young); o.Outcome != store.OutcomePending {
		t.Fatalf("an 11 h message = %s, want pending", o.Outcome)
	}
}

func TestTickNudges(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	fx.refl.replies = []string{`{"action":"nudge","nudge_of":` + strconv.FormatInt(id, 10) + `,"message":"Still interested? A no is fine too."}`}
	if res, err := fx.h.Tick(ctx); err != nil || res != "nudge #"+strconv.FormatInt(id, 10) {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachNudge || fx.del.sent[0].NudgeOf != id {
		t.Fatalf("sent = %+v", fx.del.sent)
	}
}

// A nudge unanswered for a day ends both messages, steps the governor down
// once, and returns the proposal's interest to candidate with a cooldown.
func TestIgnoredNudgeEndsBothAndReturnsTheProposal(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	_, _ = fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed)
	orig, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-40 * time.Hour)})
	_, _ = fx.st.ResolveOutreach(ctx, orig, []string{store.OutcomePending}, store.OutcomeUnanswered, "", fx.now.Add(-28*time.Hour))
	nudge, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachNudge, Text: "y", NudgeOf: orig, SentAt: fx.now.Add(-25 * time.Hour)})
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, nudge); o.Outcome != store.OutcomeIgnored {
		t.Fatalf("nudge = %s, want ignored", o.Outcome)
	}
	if o := fx.outcome(t, orig); o.Outcome != store.OutcomeIgnored {
		t.Fatalf("original = %s, want ignored", o.Outcome)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestCandidate || !got.CooldownUntil.Equal(fx.now.Add(companion.ReturnCooldown)) {
		t.Fatalf("interest = %s until %v, want candidate cooling down 7d", got.State, got.CooldownUntil)
	}
	if b := fx.budget(t); b != 3.5 {
		t.Fatalf("budget = %v, want 3.5 (one step down)", b)
	}
}

func TestTickDropsWithAReason(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	_, _ = fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	fx.refl.replies = []string{`{"action":"none","drops":[{"outreach":` + strconv.FormatInt(id, 10) + `,"reason":"the earnings moment passed"}]}`}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	o := fx.outcome(t, id)
	if o.Outcome != store.OutcomeIgnored || o.DropReason != "the earnings moment passed" {
		t.Fatalf("dropped = %+v", o)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate (a drop is not a no)", got.State)
	}
}

func TestTickRewritesSelf(t *testing.T) {
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	body := "## What I'm curious about\n\n- ZS earnings\n\n## Threads with you\n\n## How you like to be talked to\n\n## Opinions I've formed\n"
	fx.refl.replies = []string{`{"action":"none","self_md":` + quote(body) + `}`}
	if _, err := fx.h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.selfWrites) != 1 || fx.selfWrites[0] != body {
		t.Fatalf("self writes = %q", fx.selfWrites)
	}
}
```

(`quote` is defined in `reflect_test.go`, same package.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/companion/ -run 'Tick|Pending|Ignored' -v`
Expected: compile errors (undefined `companion.Heartbeat`, `companion.ErrBusy`, `companion.LoadBudget`, …).

- [ ] **Step 3: Implement**

Create `internal/companion/state.go`:

```go
package companion

import (
	"context"
	"strconv"
	"time"

	"github.com/codered/spore/internal/store"
)

// Keys of the companion_state table.
const (
	StateSessionID      = "session_id"
	StateBudget         = "daily_budget"
	StatePausedUntil    = "paused_until"
	StateLastReflection = "last_reflection_at"
	StateLastResult     = "last_reflection_result"
	StateFailures       = "reflection_failures"
	StateFingerprint    = "last_fingerprint"
)

// LoadBudget is the governor's budget: the stored value, or start when none
// is stored yet. A value that does not parse -- a hand-edited row -- reads
// as start rather than stopping the companion.
func LoadBudget(ctx context.Context, st *store.Store, start int) (float64, error) {
	v, ok, err := st.CompanionState(ctx, StateBudget)
	if err != nil {
		return 0, err
	}
	if !ok {
		return float64(start), nil
	}
	b, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return float64(start), nil
	}
	return clampBudget(b, start), nil
}

// SaveBudget stores the governor's budget.
func SaveBudget(ctx context.Context, st *store.Store, b float64) error {
	return st.SetCompanionState(ctx, StateBudget, strconv.FormatFloat(b, 'f', -1, 64))
}

// StateTime reads a time-valued key; zero when unset or unreadable.
func StateTime(ctx context.Context, st *store.Store, key string) (time.Time, error) {
	v, ok, err := st.CompanionState(ctx, key)
	if err != nil || !ok || v == "" {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, nil
	}
	return t, nil
}

func setStateTime(ctx context.Context, st *store.Store, key string, t time.Time) error {
	v := ""
	if !t.IsZero() {
		v = t.UTC().Format(time.RFC3339)
	}
	return st.SetCompanionState(ctx, key, v)
}

// PausedUntil is when a pause ends; zero when there is none.
func PausedUntil(ctx context.Context, st *store.Store) (time.Time, error) {
	return StateTime(ctx, st, StatePausedUntil)
}

// SetPausedUntil pauses outreach until t; a zero t resumes it.
func SetPausedUntil(ctx context.Context, st *store.Store, t time.Time) error {
	return setStateTime(ctx, st, StatePausedUntil, t)
}

// SpentToday counts the messages started since local midnight that count
// against the daily budget: all but alerts, which have their own allowance.
func SpentToday(ctx context.Context, st *store.Store, now time.Time, loc *time.Location) (int, error) {
	rows, err := st.OutreachSince(ctx, LocalDayStart(now, loc))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range rows {
		if o.Kind != store.OutreachAlert {
			n++
		}
	}
	return n, nil
}

// Resolve moves one outreach to `to`, when its outcome is one of from, and
// lets the governor respond. A pending nudge of it follows it to the same
// outcome without a second governor step: one answer, one adjustment. It
// reports whether the outreach moved.
func Resolve(ctx context.Context, st *store.Store, start int, id int64, from []string, to, reason string, now time.Time) (bool, error) {
	moved, err := st.ResolveOutreach(ctx, id, from, to, reason, now)
	if err != nil || !moved {
		return moved, err
	}
	b, err := LoadBudget(ctx, st, start)
	if err != nil {
		return true, err
	}
	if nb := ApplyOutcome(b, to, start); nb != b {
		if err := SaveBudget(ctx, st, nb); err != nil {
			return true, err
		}
	}
	n, ok, err := st.NudgeFor(ctx, id)
	if err != nil {
		return true, err
	}
	if ok {
		if _, err := st.ResolveOutreach(ctx, n.ID, []string{store.OutcomePending}, to, "", now); err != nil {
			return true, err
		}
	}
	return true, nil
}

// returnProposal puts a proposed interest back to candidate with
// ReturnCooldown when its proposal was dropped or went unanswered after a
// nudge: the user never said no, so it may come up again later.
func returnProposal(ctx context.Context, st *store.Store, o store.Outreach, now time.Time) error {
	if o.Kind != store.OutreachProposal || o.InterestID == 0 {
		return nil
	}
	moved, err := st.SetInterestState(ctx, o.InterestID, store.InterestProposed, store.InterestCandidate)
	if err != nil || !moved {
		return err
	}
	return st.SetInterestCooldown(ctx, o.InterestID, now.Add(ReturnCooldown))
}
```

Create `internal/companion/heartbeat.go`:

```go
package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// Timeouts and cooldowns. See the companion spec, sections 2.3, 4.6 and 5.3.
const (
	UnansweredAfter   = 12 * time.Hour
	NudgeIgnoredAfter = 24 * time.Hour
	NotNowCooldown    = 14 * 24 * time.Hour
	ReturnCooldown    = 7 * 24 * time.Hour
	RecentOutreach    = 7 * 24 * time.Hour
	// FailureWarning is how many reflections in a row may fail before
	// status warns about it.
	FailureWarning = 3
)

// ErrBusy is the deliverer refusing because a turn is running in the
// companion session: appending to it now would land inside that turn.
var ErrBusy = errors.New("the companion session has a turn running")

// Reflector makes the reflection call.
type Reflector interface {
	Reflect(ctx context.Context, system, input string) (string, error)
}

// Deliverer is the daemon's side of an outreach: the companion session and
// the channel. The heartbeat never touches either directly.
type Deliverer interface {
	// EnsureSession returns the companion session, creating it when there
	// is none.
	EnsureSession(ctx context.Context) (string, error)
	// Deliver records o (as pending) in the store and the companion session,
	// then sends it on the channel. It returns the stored row. It returns
	// ErrBusy, having written nothing, while a turn runs in the session.
	Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error)
}

// SelfWriter replaces self.md through the refinement ledger, recorded
// against sessionID.
type SelfWriter func(ctx context.Context, sessionID, body string) error

// Heartbeat is the companion's tick: resolve stale outreach, sweep
// interests, and -- when something changed and the gate allows -- ask the
// model whether to start a conversation.
type Heartbeat struct {
	Store     *store.Store
	Cfg       *config.Config
	Recorder  *Recorder
	Reflector Reflector
	Deliverer Deliverer
	WriteSelf SelfWriter
	// Now is the clock; tests replace it.
	Now func() time.Time
}

func (h *Heartbeat) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

// Run ticks every `every` until ctx ends, starting with one tick now. The
// heartbeat reads current state, never a backlog, so a restart or a night
// of quiet hours never produces a burst.
func (h *Heartbeat) Run(ctx context.Context, every time.Duration) {
	tick := func() {
		res, err := h.Tick(ctx)
		if err != nil {
			slog.Warn("companion heartbeat failed", "err", err)
			return
		}
		slog.Debug("companion heartbeat", "result", res)
	}
	tick()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}

// Tick runs one heartbeat and says what it did in a few words: why it
// skipped, or the action taken. An error means the tick failed and nothing
// was sent.
func (h *Heartbeat) Tick(ctx context.Context) (string, error) {
	c := h.Cfg.Companion
	if !c.Enabled {
		return "disabled", nil
	}
	now := h.now()
	loc, err := c.Location()
	if err != nil {
		return "", err
	}
	if err := h.resolveStale(ctx, now); err != nil {
		return "", err
	}
	if h.Recorder != nil {
		if err := h.Recorder.Sweep(ctx, now); err != nil {
			return "", err
		}
	}
	paused, err := PausedUntil(ctx, h.Store)
	if err != nil {
		return "", err
	}
	if now.Before(paused) {
		return "paused", nil
	}
	quiet, err := InQuietHours(now, loc, c.QuietHours)
	if err != nil {
		return "", err
	}
	if quiet {
		return "quiet hours", nil
	}
	v, err := h.view(ctx, now, loc)
	if err != nil {
		return "", err
	}
	if !Eligible(v) {
		return "nothing to decide", nil
	}
	if v.BudgetLeft < 1 {
		return "budget spent", nil
	}
	fp := Fingerprint(v)
	last, _, err := h.Store.CompanionState(ctx, StateFingerprint)
	if err != nil {
		return "", err
	}
	if last == fp {
		return "nothing changed", nil
	}
	reply, err := h.Reflector.Reflect(ctx, ReflectionPrompt, RenderInput(v))
	if err != nil {
		return "", h.fail(ctx, now, fmt.Errorf("reflection call: %w", err))
	}
	d, err := ParseDecision(reply)
	if err == nil {
		err = Validate(d, v)
	}
	if err != nil {
		return "", h.fail(ctx, now, fmt.Errorf("reflection output: %w", err))
	}
	if err := h.apply(ctx, d, v, now); err != nil {
		if errors.Is(err, ErrBusy) {
			// The user is talking to spore right now; the next tick asks again.
			return "companion session busy", nil
		}
		return "", h.fail(ctx, now, err)
	}
	res := summarize(d)
	return res, h.succeed(ctx, now, fp, res)
}

// resolveStale times out what the user did not answer: a message pending
// for UnansweredAfter becomes unanswered (reflection decides what next), and
// a nudge pending for NudgeIgnoredAfter ends itself and its original as
// ignored.
func (h *Heartbeat) resolveStale(ctx context.Context, now time.Time) error {
	pending, err := h.Store.OutreachByOutcome(ctx, store.OutcomePending)
	if err != nil {
		return err
	}
	for _, o := range pending {
		age := now.Sub(o.SentAt)
		if o.Kind != store.OutreachNudge {
			if age >= UnansweredAfter {
				if _, err := h.Store.ResolveOutreach(ctx, o.ID, []string{store.OutcomePending}, store.OutcomeUnanswered, "", now); err != nil {
					return err
				}
			}
			continue
		}
		if age < NudgeIgnoredAfter {
			continue
		}
		if _, err := h.Store.ResolveOutreach(ctx, o.ID, []string{store.OutcomePending}, store.OutcomeIgnored, "", now); err != nil {
			return err
		}
		orig, ok, err := h.Store.OutreachByID(ctx, o.NudgeOf)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		moved, err := Resolve(ctx, h.Store, h.Cfg.Companion.StartBudget, orig.ID, []string{store.OutcomeUnanswered},
			store.OutcomeIgnored, "its nudge went unanswered for a day", now)
		if err != nil {
			return err
		}
		if moved {
			if err := returnProposal(ctx, h.Store, orig, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// view reads everything a reflection sees.
func (h *Heartbeat) view(ctx context.Context, now time.Time, loc *time.Location) (View, error) {
	v := View{Now: now, Loc: loc, SelfMax: h.Cfg.Companion.SelfMaxBytes,
		Days: map[int64][]string{}, Keys: map[int64]string{}, Nudged: map[int64]bool{}}
	// soul.md is the user's; missing or unreadable, it simply has no section.
	if b, err := os.ReadFile(h.Cfg.SoulPath()); err == nil {
		v.Soul = string(b)
	}
	switch b, err := os.ReadFile(h.Cfg.SelfPath()); {
	case err == nil:
		v.Self, v.SelfReadable = string(b), true
	case errors.Is(err, fs.ErrNotExist):
		v.SelfReadable = true
	default:
		slog.Warn("companion: self.md is unreadable; reflection may not rewrite it", "err", err)
	}
	all, err := h.Store.Interests(ctx)
	if err != nil {
		return View{}, err
	}
	for _, in := range all {
		v.Keys[in.ID] = in.Key
		switch in.State {
		case store.InterestCandidate, store.InterestProposed, store.InterestActive:
			v.Interests = append(v.Interests, in)
			days, err := h.Store.InterestDays(ctx, in.ID)
			if err != nil {
				return View{}, err
			}
			v.Days[in.ID] = days
		}
	}
	if v.Recent, err = h.Store.OutreachSince(ctx, now.Add(-RecentOutreach)); err != nil {
		return View{}, err
	}
	if v.Unanswered, err = h.Store.OutreachByOutcome(ctx, store.OutcomeUnanswered); err != nil {
		return View{}, err
	}
	for _, o := range v.Unanswered {
		_, ok, err := h.Store.NudgeFor(ctx, o.ID)
		if err != nil {
			return View{}, err
		}
		v.Nudged[o.ID] = ok
	}
	b, err := LoadBudget(ctx, h.Store, h.Cfg.Companion.StartBudget)
	if err != nil {
		return View{}, err
	}
	spent, err := SpentToday(ctx, h.Store, now, loc)
	if err != nil {
		return View{}, err
	}
	v.BudgetLeft = int(b) - spent
	return v, nil
}

// apply carries out a validated decision. The message goes first: if it
// cannot be delivered, nothing else in the decision is written.
func (h *Heartbeat) apply(ctx context.Context, d Decision, v View, now time.Time) error {
	switch d.Action {
	case ActionPropose:
		in, _ := v.Interest(d.Interest)
		payload, err := json.Marshal(d.Watch)
		if err != nil {
			return err
		}
		if _, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal,
			Text: d.Message, Payload: string(payload), SentAt: now}); err != nil {
			return err
		}
		if _, err := h.Store.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed); err != nil {
			return err
		}
	case ActionNudge:
		orig, _ := v.UnansweredByID(d.NudgeOf)
		if _, err := h.Deliverer.Deliver(ctx, store.Outreach{InterestID: orig.InterestID, Kind: store.OutreachNudge,
			Text: d.Message, NudgeOf: orig.ID, SentAt: now}); err != nil {
			return err
		}
	}
	for _, dr := range d.Drops {
		o, _ := v.UnansweredByID(dr.Outreach)
		moved, err := Resolve(ctx, h.Store, h.Cfg.Companion.StartBudget, o.ID, []string{store.OutcomeUnanswered},
			store.OutcomeIgnored, strings.TrimSpace(dr.Reason), now)
		if err != nil {
			return err
		}
		if moved {
			if err := returnProposal(ctx, h.Store, o, now); err != nil {
				return err
			}
		}
	}
	if d.SelfMD != nil && h.WriteSelf != nil {
		sid, err := h.Deliverer.EnsureSession(ctx)
		if err != nil {
			return err
		}
		if err := h.WriteSelf(ctx, sid, *d.SelfMD); err != nil {
			return fmt.Errorf("rewrite self.md: %w", err)
		}
	}
	return nil
}

// summarize names what a decision did, for logs and status.
func summarize(d Decision) string {
	var s string
	switch d.Action {
	case ActionPropose:
		s = "propose " + d.Interest
	case ActionNudge:
		s = "nudge #" + strconv.FormatInt(d.NudgeOf, 10)
	default:
		s = "none"
	}
	if n := len(d.Drops); n > 0 {
		s += fmt.Sprintf(" (+%d dropped)", n)
	}
	if d.SelfMD != nil {
		s += " (self.md rewritten)"
	}
	return s
}

// fail records a failed reflection and returns cause.
func (h *Heartbeat) fail(ctx context.Context, now time.Time, cause error) error {
	n := 0
	if v, ok, err := h.Store.CompanionState(ctx, StateFailures); err == nil && ok {
		n, _ = strconv.Atoi(v)
	}
	n++
	_ = h.Store.SetCompanionState(ctx, StateFailures, strconv.Itoa(n))
	_ = setStateTime(ctx, h.Store, StateLastReflection, now)
	_ = h.Store.SetCompanionState(ctx, StateLastResult, clip("failed: "+cause.Error(), 200))
	return cause
}

// succeed records a reflection's result and the fingerprint it answered.
func (h *Heartbeat) succeed(ctx context.Context, now time.Time, fp, result string) error {
	if err := h.Store.SetCompanionState(ctx, StateFailures, "0"); err != nil {
		return err
	}
	if err := setStateTime(ctx, h.Store, StateLastReflection, now); err != nil {
		return err
	}
	if err := h.Store.SetCompanionState(ctx, StateLastResult, result); err != nil {
		return err
	}
	return h.Store.SetCompanionState(ctx, StateFingerprint, fp)
}
```

Note on `TestPendingBecomesUnansweredNotIgnored`'s second tick: after the first tick stores the fingerprint, the half-hour-later tick sees the same day, the same unanswered row and the same outcomes, so it returns "nothing changed". If it does not, check that `Fingerprint` does not include anything time-varying besides the local day and cooldown flags.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/companion/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/companion/
git commit -m "companion: the heartbeat tick: gate, unanswered follow-up, reflection, governor"
```

---

### Task 6: `companion.Service`: proposals, replies, feedback, pause, interest updates, status

**Files:**
- Create: `internal/companion/service.go`
- Create: `internal/companion/service_test.go`

**Interfaces:**
- Consumes: Tasks 2, 4, 5 (`Resolve`, `LoadBudget`, `SaveBudget`, `SetPausedUntil`, `StateTime`, `SpentToday`, `ApplyFeedback`, `ValidateWatch`, `ValidateCron`, `ValidateGoal`, `WatchPrompt`, `NotNowCooldown`); `scheduler.Parse`; `config.CompanionProfile`.
- Produces:
  - `ChoiceYes = "yes"`, `ChoiceNotNow = "not-now"`, `ChoiceNever = "never"`; `ErrNotProposal`; `StopPause` (7d).
  - `Service{Store *store.Store; Cfg *config.Config; Now func() time.Time}`; `NewService(st, cfg) *Service`.
  - Methods: `AnswerProposal(ctx, outreachID int64, choice string) (string, error)`; `UserReplied(ctx, sessionID string) error`; `IsCompanionSession(ctx, sessionID string) (bool, error)`; `Feedback(ctx, kind string) (string, error)`; `Pause(ctx, d time.Duration) (time.Time, error)`; `Resume(ctx) error`; `Undecline(ctx, key string) error`; `UpdateInterest(ctx, key, cron, goal, state string) (string, error)`; `Status(ctx) (Status, error)`.
  - `Status{Enabled bool; Channel string; PausedUntil time.Time; Budget float64; SpentToday int; LastReflection time.Time; LastResult string; Failures int; SessionID string}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/service_test.go`:

```go
package companion_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type svcFixture struct {
	*hbFixture
	svc *companion.Service
	sid string
}

func newService(t *testing.T) *svcFixture {
	t.Helper()
	fx := newHeartbeat(t)
	svc := companion.NewService(fx.st, fx.cfg)
	svc.Now = func() time.Time { return fx.now }
	sid, err := fx.del.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &svcFixture{hbFixture: fx, svc: svc, sid: sid}
}

// proposal seeds a proposed interest and its pending proposal.
func (fx *svcFixture) proposal(t *testing.T, key string) (store.Interest, int64) {
	t.Helper()
	ctx := context.Background()
	in := fx.candidate(t, key)
	if ok, err := fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed); err != nil || !ok {
		t.Fatalf("propose %s: %v %v", key, ok, err)
	}
	id, err := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "watch?",
		Payload: `{"cron":"0 14 * * 1-5","goal":"Check it."}`, SentAt: fx.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return in, id
}

func TestAnswerProposalYesCreatesTheWatch(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_ = companion.SaveBudget(ctx, fx.st, 2)
	in, id := fx.proposal(t, "stock:zs")
	msg, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestActive || got.WatchJobID == 0 {
		t.Fatalf("interest = %+v", got)
	}
	job, ok, err := fx.st.Job(ctx, got.WatchJobID)
	if err != nil || !ok {
		t.Fatalf("job: %v %v", ok, err)
	}
	if job.Profile != config.CompanionProfile || job.Notify != store.NotifyEach || !job.CheckedIn ||
		job.OriginSessionID != fx.sid || job.Spec != "0 14 * * 1-5" || !job.Enabled ||
		!strings.Contains(job.Prompt, in.Label) || !strings.Contains(job.Prompt, "Check it.") {
		t.Fatalf("watch job = %+v", job)
	}
	if !strings.Contains(msg, in.Label) {
		t.Errorf("message = %q", msg)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeReacted {
		t.Fatalf("outcome = %s, want reacted", o.Outcome)
	}
	if b := fx.budget(t); b != 2.25 {
		t.Fatalf("budget = %v, want 2.25", b)
	}
}

func TestAnswerProposalTwiceCreatesOneJob(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("second press err = %v, want ErrNotProposal", err)
	}
	jobs, _ := fx.st.ListJobs(ctx)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
}

func TestAnswerProposalNotNowAndNever(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, later := fx.proposal(t, "stock:zs")
	_, never := fx.proposal(t, "topic:go")
	if _, err := fx.svc.AnswerProposal(ctx, later, companion.ChoiceNotNow); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestObserving || !got.CooldownUntil.Equal(fx.now.Add(companion.NotNowCooldown)) {
		t.Fatalf("not now = %s until %v", got.State, got.CooldownUntil)
	}
	if _, err := fx.svc.AnswerProposal(ctx, never, companion.ChoiceNever); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "topic:go"); got.State != store.InterestDeclined {
		t.Fatalf("never = %s", got.State)
	}
	if _, err := fx.svc.AnswerProposal(ctx, later, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("yes after not now = %v, want ErrNotProposal", err)
	}
	if jobs, _ := fx.st.ListJobs(ctx); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0", len(jobs))
	}
}

func TestAnswerProposalRefusesOtherInput(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, "maybe"); err == nil {
		t.Fatal("an unknown choice must be refused")
	}
	nudge, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", NudgeOf: id, SentAt: fx.now})
	if _, err := fx.svc.AnswerProposal(ctx, nudge, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("answering a nudge = %v, want ErrNotProposal", err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, 9999, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("answering nothing = %v, want ErrNotProposal", err)
	}
}

func TestUserRepliedResolvesOnlyInTheCompanionSession(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	a, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "a", SentAt: fx.now})
	b, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "b", SentAt: fx.now.Add(-13 * time.Hour)})
	_, _ = fx.st.ResolveOutreach(ctx, b, []string{store.OutcomePending}, store.OutcomeUnanswered, "", fx.now)
	other, _ := fx.st.CreateSessionFrom(ctx, "chat", "", store.SourceChat)
	if err := fx.svc.UserReplied(ctx, other); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, a); o.Outcome != store.OutcomePending {
		t.Fatalf("a reply elsewhere resolved %d", a)
	}
	if err := fx.svc.UserReplied(ctx, fx.sid); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a, b} {
		if o := fx.outcome(t, id); o.Outcome != store.OutcomeReplied {
			t.Fatalf("outreach %d = %s, want replied", id, o.Outcome)
		}
	}
}

func TestFeedbackAndPause(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	if _, err := fx.svc.Feedback(ctx, "louder"); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	if _, err := fx.svc.Feedback(ctx, companion.FeedbackStop); err != nil {
		t.Fatal(err)
	}
	if b := fx.budget(t); b != 2 {
		t.Fatalf("budget after stop = %v, want 2", b)
	}
	until, _ := companion.PausedUntil(ctx, fx.st)
	if !until.Equal(fx.now.Add(companion.StopPause)) {
		t.Fatalf("paused until %v, want 7 days", until)
	}
	if err := fx.svc.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if until, _ := companion.PausedUntil(ctx, fx.st); !until.IsZero() {
		t.Fatalf("still paused until %v", until)
	}
	got, err := fx.svc.Pause(ctx, 3*time.Hour)
	if err != nil || !got.Equal(fx.now.Add(3*time.Hour)) {
		t.Fatalf("pause = %v %v", got, err)
	}
	if _, err := fx.svc.Pause(ctx, 0); err == nil {
		t.Fatal("a zero pause must be refused")
	}
}

func TestUpdateInterestNeverActivates(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	fx.candidate(t, "stock:zs")
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "", "", store.InterestActive); err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("activating through interest_update = %v, want refused", err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("state = %s", got.State)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "0 9 * * *", "", ""); err == nil {
		t.Fatal("changing a watch that is not active must be refused")
	}
}

func TestUpdateInterestChangesAndRetiresAWatch(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "*/15 * * * *", "", ""); err == nil {
		t.Fatal("a cron firing every 15 minutes must be refused")
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "0 16 * * 1-5", "Check ZS after the close.", ""); err != nil {
		t.Fatal(err)
	}
	in, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	job, _, _ := fx.st.Job(ctx, in.WatchJobID)
	if job.Spec != "0 16 * * 1-5" || !strings.Contains(job.Prompt, "after the close") {
		t.Fatalf("job = %+v", job)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "", "", store.InterestRetired); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestRetired {
		t.Fatalf("state = %s", got.State)
	}
	if job, _, _ := fx.st.Job(ctx, in.WatchJobID); job.Enabled {
		t.Fatal("retiring an interest must stop its watch")
	}
}

func TestUndecline(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	_, _ = fx.svc.AnswerProposal(ctx, id, companion.ChoiceNever)
	if err := fx.svc.Undecline(ctx, "stock:zs"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestObserving {
		t.Fatalf("state = %s", got.State)
	}
	if err := fx.svc.Undecline(ctx, "stock:zs"); err == nil {
		t.Fatal("undeclining an observing interest must be refused")
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, _ = fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-time.Hour)})
	_ = fx.st.SetCompanionState(ctx, companion.StateFailures, "3")
	_ = fx.st.SetCompanionState(ctx, companion.StateLastResult, "failed: provider down")
	s, err := fx.svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Channel != config.CompanionChannelTerminal || s.Budget != 4 || s.SpentToday != 1 ||
		s.Failures != 3 || s.LastResult != "failed: provider down" || s.SessionID != fx.sid {
		t.Fatalf("status = %+v", s)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/companion/ -run 'Answer|UserReplied|Feedback|UpdateInterest|Undecline|TestStatus' -v`
Expected: compile errors (undefined `companion.NewService`, …).

- [ ] **Step 3: Implement**

Create `internal/companion/service.go`:

```go
package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// Proposal answers: the three buttons.
const (
	ChoiceYes    = "yes"
	ChoiceNotNow = "not-now"
	ChoiceNever  = "never"
)

// StopPause is how long "stop" holds outreach.
const StopPause = 7 * 24 * time.Hour

// ErrNotProposal is an answer to something that is not an open proposal:
// not a proposal at all, or one already answered.
var ErrNotProposal = errors.New("not an open proposal")

// Service is the companion's deterministic side: what the user's buttons,
// replies and commands do. Nothing here asks a model.
type Service struct {
	Store *store.Store
	Cfg   *config.Config
	// Now is the clock; tests replace it.
	Now func() time.Time
}

// NewService builds a Service on the wall clock.
func NewService(st *store.Store, cfg *config.Config) *Service {
	return &Service{Store: st, Cfg: cfg, Now: time.Now}
}

func (s *Service) start() int { return s.Cfg.Companion.StartBudget }

// AnswerProposal acts on a proposal's button. Yes creates the watch job --
// the only way an interest becomes active -- Not now cools the interest down
// for two weeks, and Never declines it. It returns a line to show the user.
func (s *Service) AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error) {
	switch choice {
	case ChoiceYes, ChoiceNotNow, ChoiceNever:
	default:
		return "", fmt.Errorf("choice must be yes, not-now or never, got %q", choice)
	}
	o, ok, err := s.Store.OutreachByID(ctx, outreachID)
	if err != nil {
		return "", err
	}
	if !ok || o.Kind != store.OutreachProposal || o.InterestID == 0 {
		return "", fmt.Errorf("%w: outreach %d", ErrNotProposal, outreachID)
	}
	in, ok, err := s.Store.InterestByID(ctx, o.InterestID)
	if err != nil {
		return "", err
	}
	if !ok || in.State != store.InterestProposed {
		return "", fmt.Errorf("%w: outreach %d (its interest is %s)", ErrNotProposal, outreachID, in.State)
	}
	now := s.Now()
	var msg string
	switch choice {
	case ChoiceYes:
		if msg, err = s.startWatch(ctx, in, o, now); err != nil {
			return "", err
		}
	case ChoiceNotNow:
		moved, err := s.Store.SetInterestState(ctx, in.ID, store.InterestProposed, store.InterestObserving)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
		}
		if err := s.Store.SetInterestCooldown(ctx, in.ID, now.Add(NotNowCooldown)); err != nil {
			return "", err
		}
		msg = fmt.Sprintf("OK, not now. I won't bring up %s for two weeks.", in.Label)
	case ChoiceNever:
		moved, err := s.Store.DeclineInterest(ctx, in.ID)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
		}
		msg = fmt.Sprintf("Understood. I won't offer to watch %s again.", in.Label)
	}
	if _, err := Resolve(ctx, s.Store, s.start(), o.ID, []string{store.OutcomePending, store.OutcomeUnanswered},
		store.OutcomeReacted, "", now); err != nil {
		return "", err
	}
	return msg, nil
}

// startWatch creates the proposal's watch job and activates the interest.
func (s *Service) startWatch(ctx context.Context, in store.Interest, o store.Outreach, now time.Time) (string, error) {
	var w Watch
	if err := json.Unmarshal([]byte(o.Payload), &w); err != nil {
		return "", fmt.Errorf("proposal %d has no readable watch: %w", o.ID, err)
	}
	if err := ValidateWatch(w, now); err != nil {
		return "", err
	}
	sched, err := scheduler.Parse(w.Cron)
	if err != nil {
		return "", err
	}
	next := sched.Next(now.UTC())
	if next.IsZero() {
		return "", fmt.Errorf("watch cron %q never fires", w.Cron)
	}
	origin, _, err := s.Store.CompanionState(ctx, StateSessionID)
	if err != nil {
		return "", err
	}
	jobID, err := s.Store.CreateJob(ctx, store.Job{
		Kind: sched.Kind(), Spec: strings.TrimSpace(w.Cron), Prompt: WatchPrompt(in.Label, w.Goal), Enabled: true,
		NextRun: next, OriginSessionID: origin, Notify: store.NotifyEach, CheckedIn: true,
		Profile: config.CompanionProfile,
	})
	if err != nil {
		return "", err
	}
	moved, err := s.Store.ActivateInterest(ctx, in.ID, jobID)
	if err != nil || !moved {
		// Another answer won the race: the watch it created stands, this one
		// must not run beside it.
		_ = s.Store.SetJobEnabled(ctx, jobID, false)
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: %s changed while answering", ErrNotProposal, in.Key)
	}
	return fmt.Sprintf("Watching %s (job %d, cron %s UTC). Every run will land here.", in.Label, jobID, w.Cron), nil
}

// IsCompanionSession reports whether sessionID is the companion's session.
func (s *Service) IsCompanionSession(ctx context.Context, sessionID string) (bool, error) {
	id, ok, err := s.Store.CompanionState(ctx, StateSessionID)
	if err != nil {
		return false, err
	}
	return ok && id != "" && id == sessionID, nil
}

// UserReplied records that the user wrote in the companion session: every
// pending or unanswered message there is answered.
func (s *Service) UserReplied(ctx context.Context, sessionID string) error {
	ok, err := s.IsCompanionSession(ctx, sessionID)
	if err != nil || !ok {
		return err
	}
	rows, err := s.Store.OutreachByOutcome(ctx, store.OutcomePending, store.OutcomeUnanswered)
	if err != nil {
		return err
	}
	now := s.Now()
	for _, o := range rows {
		if _, err := Resolve(ctx, s.Store, s.start(), o.ID, []string{store.OutcomePending, store.OutcomeUnanswered},
			store.OutcomeReplied, "", now); err != nil {
			return err
		}
	}
	return nil
}

// Feedback applies the user's "more", "less" or "stop" to the governor.
// "stop" also pauses outreach for StopPause.
func (s *Service) Feedback(ctx context.Context, kind string) (string, error) {
	if !slices.Contains(FeedbackKinds, kind) {
		return "", fmt.Errorf("feedback must be one of %s, got %q", strings.Join(FeedbackKinds, ", "), kind)
	}
	b, err := LoadBudget(ctx, s.Store, s.start())
	if err != nil {
		return "", err
	}
	nb := ApplyFeedback(b, kind, s.start())
	if err := SaveBudget(ctx, s.Store, nb); err != nil {
		return "", err
	}
	if kind == FeedbackStop {
		until := s.Now().Add(StopPause)
		if err := SetPausedUntil(ctx, s.Store, until); err != nil {
			return "", err
		}
		return fmt.Sprintf("Paused until %s. After that, at most %d unprompted messages a day.",
			until.Format("Mon Jan 2"), int(nb)), nil
	}
	return fmt.Sprintf("Noted: at most %d unprompted messages a day from now on.", int(nb)), nil
}

// Pause holds outreach for d.
func (s *Service) Pause(ctx context.Context, d time.Duration) (time.Time, error) {
	if d <= 0 {
		return time.Time{}, fmt.Errorf("pause must be positive, got %s", d)
	}
	until := s.Now().Add(d)
	return until, SetPausedUntil(ctx, s.Store, until)
}

// Resume ends a pause.
func (s *Service) Resume(ctx context.Context) error {
	return SetPausedUntil(ctx, s.Store, time.Time{})
}

// Undecline lets a declined interest be noticed again.
func (s *Service) Undecline(ctx context.Context, key string) error {
	key = strings.ToLower(strings.TrimSpace(key))
	in, ok, err := s.Store.InterestByKey(ctx, key)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no interest %q", key)
	}
	moved, err := s.Store.SetInterestState(ctx, in.ID, store.InterestDeclined, store.InterestObserving)
	if err != nil {
		return err
	}
	if !moved {
		return fmt.Errorf("%s is %s, not declined", key, in.State)
	}
	return nil
}

// UpdateInterest is interest_update: adjust an active watch's cron or goal,
// or retire an interest. It can never make an interest active; only the
// user's Yes on a proposal does that.
func (s *Service) UpdateInterest(ctx context.Context, key, cron, goal, state string) (string, error) {
	key = strings.ToLower(strings.TrimSpace(key))
	in, ok, err := s.Store.InterestByKey(ctx, key)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("no interest %q", key)
	}
	if state != "" {
		if state != store.InterestRetired {
			return "", fmt.Errorf("state may only be %q: only the user's Yes on a proposal starts a watch", store.InterestRetired)
		}
		if in.State == store.InterestRetired {
			return in.Label + " is already retired.", nil
		}
		if in.WatchJobID != 0 {
			if _, found, err := s.Store.Job(ctx, in.WatchJobID); err != nil {
				return "", err
			} else if found {
				if err := s.Store.SetJobEnabled(ctx, in.WatchJobID, false); err != nil {
					return "", err
				}
			}
		}
		moved, err := s.Store.SetInterestState(ctx, in.ID, in.State, store.InterestRetired)
		if err != nil {
			return "", err
		}
		if !moved {
			return "", fmt.Errorf("%s changed while retiring it; try again", key)
		}
		return fmt.Sprintf("Retired %s.", in.Label), nil
	}
	if cron == "" && goal == "" {
		return "", errors.New("nothing to change: give cron, goal or state")
	}
	if in.State != store.InterestActive || in.WatchJobID == 0 {
		return "", fmt.Errorf("%s is %s; only an active watch can be changed", key, in.State)
	}
	job, found, err := s.Store.Job(ctx, in.WatchJobID)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("the watch job for %s is gone", key)
	}
	now := s.Now()
	spec, prompt := job.Spec, job.Prompt
	if cron != "" {
		if err := ValidateCron(cron, now); err != nil {
			return "", err
		}
		spec = strings.TrimSpace(cron)
	}
	if goal != "" {
		if err := ValidateGoal(goal); err != nil {
			return "", err
		}
		prompt = WatchPrompt(in.Label, goal)
	}
	sched, err := scheduler.Parse(spec)
	if err != nil {
		return "", err
	}
	if err := s.Store.UpdateJobSchedule(ctx, job.ID, spec, prompt, sched.Next(now.UTC())); err != nil {
		return "", err
	}
	return fmt.Sprintf("Updated the %s watch (job %d, cron %s UTC).", in.Label, job.ID, spec), nil
}

// Status is what `spore companion status` shows.
type Status struct {
	Enabled        bool
	Channel        string
	PausedUntil    time.Time
	Budget         float64
	SpentToday     int
	LastReflection time.Time
	LastResult     string
	Failures       int
	SessionID      string
}

// Status reads the companion's state. It works with or without a daemon.
func (s *Service) Status(ctx context.Context) (Status, error) {
	c := s.Cfg.Companion
	st := Status{Enabled: c.Enabled, Channel: s.Cfg.CompanionChannel()}
	loc, err := c.Location()
	if err != nil {
		return Status{}, err
	}
	if st.PausedUntil, err = PausedUntil(ctx, s.Store); err != nil {
		return Status{}, err
	}
	if st.Budget, err = LoadBudget(ctx, s.Store, s.start()); err != nil {
		return Status{}, err
	}
	if st.SpentToday, err = SpentToday(ctx, s.Store, s.Now(), loc); err != nil {
		return Status{}, err
	}
	if st.LastReflection, err = StateTime(ctx, s.Store, StateLastReflection); err != nil {
		return Status{}, err
	}
	if st.LastResult, _, err = s.Store.CompanionState(ctx, StateLastResult); err != nil {
		return Status{}, err
	}
	if v, ok, err := s.Store.CompanionState(ctx, StateFailures); err != nil {
		return Status{}, err
	} else if ok {
		st.Failures, _ = strconv.Atoi(v)
	}
	if st.SessionID, _, err = s.Store.CompanionState(ctx, StateSessionID); err != nil {
		return Status{}, err
	}
	return st, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/companion/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/companion/
git commit -m "companion: Service: proposal buttons, replies, feedback, pause, interest updates, status"
```

---

### Task 7: Daemon: router site, LLM reflector, deliverer, proposal route, reply hook, job profile

**Files:**
- Modify: `internal/router/router.go`, `internal/router/router_test.go` (only if it pins the site list)
- Modify: `internal/bridge/discord/model.go` (`modelGroups`)
- Create: `internal/companion/llm.go`, `internal/companion/llm_test.go`
- Create: `internal/daemon/companion.go`, `internal/daemon/companion_test.go`
- Modify: `internal/daemon/server.go` (fields, attach methods, routes), `internal/daemon/sessions.go` (reply hook), `internal/daemon/jobs.go` (job profile)

**Interfaces:**
- Consumes: `companion.Service`, `companion.Deliverer`, `companion.ErrBusy`, `companion.StateSessionID`, `companion.ErrNotProposal`, `companion.Status`; `store.Job.Profile`; `daemon.SchedulerTag`.
- Produces:
  - `router.SiteCompanion = "companion"` (in `ValidSite`, `Sites`, `GlobalSites`).
  - `companion.Channel` interface `{ Name() string; Send(ctx context.Context, sessionID string, o store.Outreach) error }` (declared in `internal/companion/llm.go`, Step 3).
  - `companion.LLMReflector{Registry *provider.Registry; Router *router.Router; MaxTokens int}` implementing `Reflector`.
  - On `*daemon.Server`: `AttachCompanion(*companion.Service)`, `Companion() *companion.Service`, `SetCompanionChannel(companion.Channel)`, `CompanionDeliverer() companion.Deliverer`, `AnswerProposal(ctx, outreachID int64, choice string) (string, error)`, `AttachHeartbeat(*companion.Heartbeat)`, `Heartbeat() *companion.Heartbeat`.
  - Routes: `POST /api/companion/proposals/{id}` (body `{"choice":"yes"|"not-now"|"never"}`, 200 `{"message": "..."}`, 409 not an open proposal, 400 bad input, 503 companion off); `GET /api/companion/status` (`CompanionStatusJSON`).
  - `StartJob` runs a job under `policy.Profile(job.Profile)` (local when empty).

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/llm_test.go`:

```go
package companion_test

import (
	"context"
	"testing"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
)

func TestLLMReflectorUsesTheCompanionSite(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: `{"action":"none"}`})
	reg := provider.NewRegistry()
	reg.Register("script", script, provider.ProviderPrice{})
	rt, err := router.New([]config.Route{{When: "companion", Model: "script/reflect"}}, "script/chat")
	if err != nil {
		t.Fatal(err)
	}
	r := companion.LLMReflector{Registry: reg, Router: rt, MaxTokens: 4096}
	got, err := r.Reflect(context.Background(), "the system", "the input")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"action":"none"}` {
		t.Fatalf("reply = %q", got)
	}
	reqs := script.Requests()
	if len(reqs) != 1 || reqs[0].Model != "reflect" || reqs[0].System[0].Text != "the system" ||
		reqs[0].Messages[0].Blocks[0].Text != "the input" || reqs[0].MaxTokens != 4096 {
		t.Fatalf("request = %+v", reqs)
	}
}
```

(If `config.Route`'s fields are named differently, read `internal/config/config.go` and use its names; `router.New` takes `[]config.Route`.)

Create `internal/daemon/companion_test.go`:

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type failingChannel struct{}

func (failingChannel) Name() string { return store.OutreachChannelDiscord }
func (failingChannel) Send(context.Context, string, store.Outreach) error {
	return errors.New("discord is down")
}

func TestDeliverRecordsAndAppends(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	o, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "Want me to watch ZS?", SentAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	sid, ok, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	if !ok || sid == "" {
		t.Fatal("no companion session recorded")
	}
	sess, _, _ := s.store.Session(ctx, sid)
	if sess.Source != store.SourceCompanion {
		t.Fatalf("source = %q", sess.Source)
	}
	msgs, err := s.store.Messages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("messages = %+v", msgs)
	}
	if !strings.Contains(string(msgs[0].BlocksJSON), SchedulerTag) || !strings.Contains(string(msgs[0].BlocksJSON), "#"+strconv.FormatInt(o.ID, 10)) {
		t.Errorf("cue = %s", msgs[0].BlocksJSON)
	}
	if !strings.Contains(string(msgs[1].BlocksJSON), "Want me to watch ZS?") {
		t.Errorf("assistant = %s", msgs[1].BlocksJSON)
	}
	got, _, _ := s.store.OutreachByID(ctx, o.ID)
	if got.Channel != store.OutreachChannelTerminal || got.Outcome != store.OutcomePending {
		t.Fatalf("outreach = %+v", got)
	}
	if _, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "still?", NudgeOf: o.ID, SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := s.store.CompanionState(ctx, companion.StateSessionID); again != sid {
		t.Fatalf("second delivery opened session %s, want %s", again, sid)
	}
}

func TestDeliverFallsBackToTheTerminal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	s.SetCompanionChannel(failingChannel{})
	o, err := s.CompanionDeliverer().Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: time.Now()})
	if err != nil {
		t.Fatalf("a failed Discord send must not fail the delivery: %v", err)
	}
	if got, _, _ := s.store.OutreachByID(ctx, o.ID); got.Channel != store.OutreachChannelFallback {
		t.Fatalf("channel = %q, want terminal-fallback", got.Channel)
	}
}

func TestDeliverRefusesWhileATurnRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	sid, err := d.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.hub.Begin(sid) {
		t.Fatal("could not hold the slot")
	}
	defer s.hub.End(sid)
	if _, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: time.Now()}); !errors.Is(err, companion.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if rows, _ := s.store.OutreachSince(ctx, time.Time{}); len(rows) != 0 {
		t.Fatalf("a refused delivery wrote %d outreach rows", len(rows))
	}
	if msgs, _ := s.store.Messages(ctx, sid); len(msgs) != 0 {
		t.Fatalf("a refused delivery appended %d messages", len(msgs))
	}
}

func TestEnsureSessionRecreatesAndReattachesWatches(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	old, _ := d.EnsureSession(ctx)
	jobID, _ := s.store.CreateJob(ctx, store.Job{Kind: "cron", Spec: "0 14 * * *", Prompt: "w", Enabled: true,
		NextRun: time.Now().Add(time.Hour), OriginSessionID: old, Notify: store.NotifyEach, CheckedIn: true, Profile: "companion"})
	in, _ := s.store.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = s.store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	_, _ = s.store.ActivateInterest(ctx, in.ID, jobID)
	if _, err := s.store.DeleteSessions(ctx, []string{old}); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("a deleted companion session was reused")
	}
	if j, _, _ := s.store.Job(ctx, jobID); j.OriginSessionID != fresh {
		t.Fatalf("watch origin = %q, want the new session %q", j.OriginSessionID, fresh)
	}
}

// seedProposal makes a proposed interest and its pending proposal, delivered
// into the companion session.
func seedProposal(t *testing.T, s *Server) int64 {
	t.Helper()
	ctx := context.Background()
	in, _ := s.store.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)")
	_, _ = s.store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	o, err := s.CompanionDeliverer().Deliver(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal,
		Text: "watch ZS?", Payload: `{"cron":"0 14 * * 1-5","goal":"Check ZS."}`, SentAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func TestProposalRouteAnswersOnce(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	url := ts.URL + "/api/companion/proposals/" + strconv.FormatInt(id, 10)
	res := postJSON(t, url, map[string]string{"choice": "yes"})
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(body["message"], "Zscaler") {
		t.Fatalf("first press = %d %v", res.StatusCode, body)
	}
	jobs, _ := s.store.ListJobs(context.Background())
	if len(jobs) != 1 || jobs[0].Profile != "companion" {
		t.Fatalf("jobs = %+v", jobs)
	}
	res = postJSON(t, url, map[string]string{"choice": "yes"})
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second press = %d, want 409", res.StatusCode)
	}
	res = postJSON(t, url, map[string]string{"choice": "maybe"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad choice = %d, want 400", res.StatusCode)
	}
}

func TestProposalRouteWithoutCompanionIs503(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/companion/proposals/1", map[string]string{"choice": "yes"})
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}

func TestAReplyInTheCompanionSessionAnswersOutreach(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "ok"})
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	sid, _, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	res := postJSON(t, ts.URL+"/api/sessions/"+sid+"/messages", map[string]string{"text": "tell me more"})
	res.Body.Close()
	if o, _, _ := s.store.OutreachByID(ctx, id); o.Outcome != store.OutcomeReplied {
		t.Fatalf("outcome = %s, want replied", o.Outcome)
	}
}

// A watch job runs under the companion profile: the same fs_write a local
// job may make is refused.
func TestAWatchJobRunsUnderTheCompanionProfile(t *testing.T) {
	writeNote := []provider.ScriptTurn{
		{ToolCalls: []provider.Block{{Type: provider.BlockToolUse, ID: "w1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"note.txt","content":"x"}`)}}},
		{Text: "done"},
	}
	const pol = `[policy]
workspace = "%WORKSPACE%"
default = "deny"
allow = ["fs_read", "fs_list", "fs_write"]
`
	for _, tc := range []struct {
		profile string
		wrote   bool
	}{{"local", true}, {"companion", false}} {
		t.Run(tc.profile, func(t *testing.T) {
			srv, _, _ := newFullServerWithPolicy(t, pol, writeNote...)
			sid, err := srv.StartJob(t.Context(), store.Job{ID: 1, Kind: "cron", Spec: "0 9 * * *", Prompt: "write a note", Enabled: true, Profile: tc.profile})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for srv.TurnRunning(sid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			sess, _, _ := srv.Store().Session(t.Context(), sid)
			_, statErr := os.Stat(filepath.Join(sess.Workspace, "note.txt"))
			if wrote := statErr == nil; wrote != tc.wrote {
				t.Fatalf("%s job wrote the file = %v, want %v", tc.profile, wrote, tc.wrote)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/companion/ ./internal/daemon/ -run 'LLMReflector|Deliver|EnsureSession|ProposalRoute|ReplyInTheCompanion|WatchJobRuns' -v`
Expected: compile errors (undefined `companion.LLMReflector`, `s.CompanionDeliverer`, `router.SiteCompanion`, …).

- [ ] **Step 3: Implement**

**Router** (`internal/router/router.go`): add the site.

```go
	// SiteCompanion is the companion's heartbeat reflection: whether to
	// start a conversation. A site of its own so it can run on a cheaper
	// model.
	SiteCompanion = "companion"
```

Add it to `ValidSite`'s case list, to `Sites` after `SiteRefinement` (`{SiteChat, SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteCompanion, SiteSubagent}`), and to `GlobalSites` (`{SiteCompaction, SiteTitle, SiteClassify, SiteRefinement, SiteCompanion}`). Task 1 already added `"companion"` to `config.RoutingOverrideSites`.

**Discord /model** (`internal/bridge/discord/model.go`): `modelGroups` becomes `[][]string{{"chat", "subagent", "compaction", "title"}, {"classify", "refinement", "companion"}}`, and its comment says "seven operations".

**Reflector and channel** (`internal/companion/llm.go`):

```go
package companion

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

// Channel delivers an outreach somewhere the user will see it. The Discord
// bridge is one; with none, the companion session itself is the terminal
// channel.
type Channel interface {
	// Name is the outreach channel recorded for a successful send.
	Name() string
	// Send delivers o, already recorded in sessionID, to the user.
	Send(ctx context.Context, sessionID string, o store.Outreach) error
}

// LLMReflector makes the reflection call on the companion call site.
type LLMReflector struct {
	Registry *provider.Registry
	Router   *router.Router
	// MaxTokens caps the reply. A thinking model spends part of it before
	// the JSON, so it is the chat reply cap, not a small fixed number.
	MaxTokens int
}

// Reflect sends one system prompt and one user message and returns the
// reply's text.
func (r LLMReflector) Reflect(ctx context.Context, system, input string) (string, error) {
	ref := r.Router.Model(router.SiteCompanion)
	p, model, price, err := r.Registry.Resolve(ref)
	if err != nil {
		return "", err
	}
	_, span := sporetrace.StartLLM(ctx, router.SiteCompanion, ref)
	ch, err := p.Stream(ctx, provider.Request{
		Model:     model,
		System:    []provider.Block{{Type: provider.BlockText, Text: system}},
		MaxTokens: r.MaxTokens,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: input}},
		}},
	})
	if err != nil {
		span.RecordError(err)
		span.End()
		return "", fmt.Errorf("companion provider %s: %w", ref, err)
	}
	var text string
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventDone:
			if ev.Usage != nil {
				usage = *ev.Usage
			}
		case provider.EventError:
			span.RecordError(ev.Err)
			span.End()
			return "", ev.Err
		}
	}
	sporetrace.EndLLM(span, input, text, usage, price.Cost(usage))
	return text, nil
}
```

**Daemon** (`internal/daemon/companion.go`):

```go
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// AttachCompanion supplies the companion service. Nil leaves the companion
// routes answering 503 and the reply hook off.
func (s *Server) AttachCompanion(svc *companion.Service) { s.companion = svc }

// Companion is the attached companion service, or nil.
func (s *Server) Companion() *companion.Service { return s.companion }

// SetCompanionChannel sets where outreach is sent besides the companion
// session. Call it before the heartbeat starts: it is read without a lock.
func (s *Server) SetCompanionChannel(ch companion.Channel) { s.companionChannel = ch }

// AttachHeartbeat supplies the heartbeat serve runs.
func (s *Server) AttachHeartbeat(h *companion.Heartbeat) { s.heartbeat = h }

// Heartbeat is the attached heartbeat, or nil.
func (s *Server) Heartbeat() *companion.Heartbeat { return s.heartbeat }

// CompanionDeliverer is the heartbeat's way into the companion session.
func (s *Server) CompanionDeliverer() companion.Deliverer { return companionDeliverer{s} }

type companionDeliverer struct{ s *Server }

// EnsureSession returns the companion session, opening a new one when there
// is none or the user deleted it. A new session takes over every active
// watch whose origin the delete cleared, so their runs report here.
func (d companionDeliverer) EnsureSession(ctx context.Context) (string, error) {
	s := d.s
	id, ok, err := s.store.CompanionState(ctx, companion.StateSessionID)
	if err != nil {
		return "", err
	}
	if ok && id != "" {
		_, found, err := s.store.Session(ctx, id)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}
	id, err = s.CreateSession(ctx, "spore", "", store.SourceCompanion, policy.ProfileLocal)
	if err != nil {
		return "", err
	}
	if err := s.store.SetCompanionState(ctx, companion.StateSessionID, id); err != nil {
		return "", err
	}
	active, err := s.store.Interests(ctx, store.InterestActive)
	if err != nil {
		return "", err
	}
	for _, in := range active {
		if in.WatchJobID == 0 {
			continue
		}
		if _, err := s.store.AttachJobOrigin(ctx, in.WatchJobID, id); err != nil {
			slog.Warn("companion: could not point a watch at the new session", "job", in.WatchJobID, "err", err)
		}
	}
	return id, nil
}

// Deliver records o in the store and in the companion session, then sends it
// on the channel. The session's turn slot is held while the messages are
// appended, so they can never land inside a turn; while one runs, Deliver
// writes nothing and returns companion.ErrBusy. A failed channel send still
// counts: the message is in the terminal, recorded as terminal-fallback.
func (d companionDeliverer) Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error) {
	s := d.s
	sid, err := d.EnsureSession(ctx)
	if err != nil {
		return store.Outreach{}, err
	}
	if !s.hub.Begin(sid) {
		return store.Outreach{}, companion.ErrBusy
	}
	ch := s.companionChannel
	o.Channel = store.OutreachChannelTerminal
	if ch != nil {
		o.Channel = ch.Name()
	}
	o.ID, err = s.store.AddOutreach(ctx, o)
	if err == nil {
		err = s.appendOutreach(ctx, sid, o)
	}
	s.hub.End(sid)
	if err != nil {
		return store.Outreach{}, err
	}
	o.Outcome = store.OutcomePending
	s.hub.Publish(sid, WireEvent{Type: WireJobNote, Text: o.Text})
	if ch != nil {
		if err := ch.Send(ctx, sid, o); err != nil {
			slog.Warn("companion: channel delivery failed; the message is in the terminal", "outreach", o.ID, "err", err)
			o.Channel = store.OutreachChannelFallback
			if err := s.store.SetOutreachChannel(ctx, o.ID, o.Channel); err != nil {
				slog.Warn("companion: could not record the fallback", "outreach", o.ID, "err", err)
			}
		}
	}
	return o, nil
}

// appendOutreach writes the outreach into the companion session as a
// scheduler-tagged user cue followed by the message as spore's own reply.
// The pair keeps the history alternating -- a session may not open on an
// assistant message -- and tells the model, when the user answers, what it
// said and why.
func (s *Server) appendOutreach(ctx context.Context, sid string, o store.Outreach) error {
	cue := fmt.Sprintf("%s companion outreach #%d (%s), started by spore at %s UTC. The next message is what you sent; the user may answer it.",
		SchedulerTag, o.ID, o.Kind, o.SentAt.UTC().Format("2006-01-02 15:04"))
	if o.Kind == store.OutreachProposal {
		cue += " It carries Yes / Not now / Never buttons; only a button press starts a watch."
	}
	for _, m := range []struct{ role, text string }{{string(provider.RoleUser), cue}, {string(provider.RoleAssistant), o.Text}} {
		blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: m.text}})
		if err != nil {
			return err
		}
		if _, err := s.store.AppendMessage(ctx, store.Message{SessionID: sid, Role: m.role, BlocksJSON: blocks}); err != nil {
			return err
		}
	}
	return nil
}

// AnswerProposal answers a proposal's button and leaves the answer as a note
// in the companion session, which the bridge also posts to its thread. The
// HTTP route and the Discord buttons both come through here.
func (s *Server) AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error) {
	if s.companion == nil {
		return "", errCompanionOff
	}
	msg, err := s.companion.AnswerProposal(ctx, outreachID, choice)
	if err != nil {
		return "", err
	}
	if sid, ok, err := s.store.CompanionState(ctx, companion.StateSessionID); err == nil && ok && sid != "" {
		if err := s.postNote(ctx, sid, msg); err != nil {
			slog.Warn("companion: could not post the proposal answer", "outreach", outreachID, "err", err)
		}
	}
	return msg, nil
}

var errCompanionOff = errors.New("the companion is not enabled")

func (s *Server) handleAnswerProposal(w http.ResponseWriter, r *http.Request) {
	if s.companion == nil {
		writeError(w, http.StatusServiceUnavailable, "%v", errCompanionOff)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "outreach id must be a number: %v", err)
		return
	}
	var body struct {
		Choice string `json:"choice"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	msg, err := s.AnswerProposal(r.Context(), id, body.Choice)
	switch {
	case errors.Is(err, companion.ErrNotProposal):
		writeError(w, http.StatusConflict, "%v", err)
	case err != nil:
		writeError(w, http.StatusBadRequest, "%v", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"message": msg})
	}
}

// CompanionStatusJSON is GET /api/companion/status.
type CompanionStatusJSON struct {
	Enabled        bool       `json:"enabled"`
	Channel        string     `json:"channel"`
	PausedUntil    *time.Time `json:"paused_until,omitempty"`
	Budget         float64    `json:"budget"`
	SpentToday     int        `json:"spent_today"`
	LastReflection *time.Time `json:"last_reflection,omitempty"`
	LastResult     string     `json:"last_result,omitempty"`
	Failures       int        `json:"failures"`
	SessionID      string     `json:"session_id,omitempty"`
}

func (s *Server) handleCompanionStatus(w http.ResponseWriter, r *http.Request) {
	if s.companion == nil {
		writeError(w, http.StatusServiceUnavailable, "%v", errCompanionOff)
		return
	}
	st, err := s.companion.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	out := CompanionStatusJSON{Enabled: st.Enabled, Channel: st.Channel, Budget: st.Budget, SpentToday: st.SpentToday,
		LastResult: st.LastResult, Failures: st.Failures, SessionID: st.SessionID}
	if !st.PausedUntil.IsZero() {
		out.PausedUntil = &st.PausedUntil
	}
	if !st.LastReflection.IsZero() {
		out.LastReflection = &st.LastReflection
	}
	writeJSON(w, http.StatusOK, out)
}
```

**Server** (`internal/daemon/server.go`): add fields to `Server`, after `notifier`:

```go
	// companion is the companion service; nil when the companion is off.
	companion *companion.Service
	// companionChannel is where outreach goes besides the companion
	// session; nil means the terminal alone.
	companionChannel companion.Channel
	// heartbeat is the companion's tick, run by serve; nil when off.
	heartbeat *companion.Heartbeat
```

and register the routes in `buildMux`, after the jobs routes:

```go
	api("POST /api/companion/proposals/{id}", s.handleAnswerProposal)
	api("GET /api/companion/status", s.handleCompanionStatus)
```

**Reply hook** (`internal/daemon/sessions.go`, in `startTurnThen`): directly after the `if !found { … }` block that follows reading the session, add:

```go
	// A message the user sends in the companion session answers whatever
	// spore said there. The scheduler's own turns are not the user.
	if s.companion != nil && sess.Source == store.SourceCompanion && client != "scheduler" {
		if err := s.companion.UserReplied(s.base, sessionID); err != nil {
			slog.Warn("companion: could not record the reply", "session", sessionID, "err", err)
		}
	}
```

**Job profile** (`internal/daemon/jobs.go`, `StartJob`): compute the profile once and use it for both `CreateSession` calls and for `startTurnThen`:

```go
	// A watch the companion created runs under the companion profile; every
	// other job runs as its local creator would.
	profile := policy.ProfileLocal
	if job.Profile != "" {
		profile = policy.Profile(job.Profile)
	}
```

Replace the three `policy.ProfileLocal` arguments in `StartJob` with `profile`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/router/ ./internal/companion/ ./internal/daemon/ ./internal/bridge/... ./internal/models/ -race`
Expected: PASS. If a router, models or Discord `/model` test pins the number or order of sites, update it to include `companion` after `refinement`.

- [ ] **Step 5: Commit**

```bash
git add internal/router/ internal/companion/ internal/daemon/ internal/bridge/discord/model.go
git commit -m "daemon: companion delivery, proposal and status routes, reply hook; jobs run under their profile"
```

---

### Task 8: Tools: `interest_update` and `companion_feedback`

**Files:**
- Create: `internal/tool/companiontool/companiontool.go`
- Create: `internal/tool/companiontool/companiontool_test.go`

**Interfaces:**
- Consumes: `companion.Service` (Task 6): `UpdateInterest`, `Feedback`, `IsCompanionSession`; `companion.FeedbackKinds`; `policy.SessionFrom`.
- Produces: `companiontool.Service` interface (the three methods above); `companiontool.New(cfg *config.Config, svc Service) []tool.Tool`, which returns nil when the companion is off or `svc` is nil.

- [ ] **Step 1: Write the failing tests**

Create `internal/tool/companiontool/companiontool_test.go`:

```go
package companiontool_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
	"github.com/codered/spore/internal/tool/companiontool"
)

func fixture(t *testing.T) (*config.Config, *store.Store, *companion.Service, string) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	if err := st.SetCompanionState(ctx, companion.StateSessionID, sid); err != nil {
		t.Fatal(err)
	}
	return cfg, st, companion.NewService(st, cfg), sid
}

func byName(tools []tool.Tool, name string) tool.Tool {
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	return nil
}

func in(sid string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{ID: sid})
}

func TestToolsExistOnlyWhileTheCompanionIsOn(t *testing.T) {
	cfg, _, svc, _ := fixture(t)
	if got := companiontool.New(cfg, svc); len(got) != 2 {
		t.Fatalf("tools = %d, want 2", len(got))
	}
	cfg.Companion.Enabled = false
	if got := companiontool.New(cfg, svc); len(got) != 0 {
		t.Fatalf("tools while off = %d, want 0", len(got))
	}
	cfg.Companion.Enabled = true
	if got := companiontool.New(cfg, nil); len(got) != 0 {
		t.Fatalf("tools with no service = %d, want 0", len(got))
	}
}

func TestToolsActOnlyInTheCompanionSession(t *testing.T) {
	cfg, st, svc, _ := fixture(t)
	other, _ := st.CreateSessionFrom(context.Background(), "chat", "", store.SourceChat)
	tools := companiontool.New(cfg, svc)
	for _, name := range []string{"interest_update", "companion_feedback"} {
		args := `{"kind":"more"}`
		if name == "interest_update" {
			args = `{"key":"stock:zs","state":"retired"}`
		}
		_, err := byName(tools, name).Call(in(other), json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "companion conversation") {
			t.Fatalf("%s outside the companion session = %v, want refused", name, err)
		}
	}
}

func TestInterestUpdateCannotActivate(t *testing.T) {
	cfg, st, svc, sid := fixture(t)
	ctx := context.Background()
	zs, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, zs.ID, store.InterestObserving, store.InterestProposed)
	upd := byName(companiontool.New(cfg, svc), "interest_update")
	if _, err := upd.Call(in(sid), json.RawMessage(`{"key":"stock:zs","state":"active"}`)); err == nil {
		t.Fatal("interest_update made an interest active")
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("state = %s, want proposed", got.State)
	}
	out, err := upd.Call(in(sid), json.RawMessage(`{"key":"stock:zs","state":"retired"}`))
	if err != nil || !strings.Contains(out, "Retired") {
		t.Fatalf("retire = %q %v", out, err)
	}
}

func TestCompanionFeedback(t *testing.T) {
	cfg, _, svc, sid := fixture(t)
	fb := byName(companiontool.New(cfg, svc), "companion_feedback")
	if _, err := fb.Call(in(sid), json.RawMessage(`{"kind":"louder"}`)); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	out, err := fb.Call(in(sid), json.RawMessage(`{"kind":"less"}`))
	if err != nil || !strings.Contains(out, "3") {
		t.Fatalf("less = %q %v", out, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tool/companiontool/ -v`
Expected: build failure (package does not exist).

- [ ] **Step 3: Implement**

Create `internal/tool/companiontool/companiontool.go`:

```go
// Package companiontool holds the two tools of the companion conversation:
// interest_update, which adjusts or retires a watch, and companion_feedback,
// which tells the governor "more", "less" or "stop". They exist only while
// the companion is on, and act only in the companion's own session: a chat
// about something else must not retire a watch by accident.
package companiontool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

// Service is the slice of companion.Service the tools use.
type Service interface {
	UpdateInterest(ctx context.Context, key, cron, goal, state string) (string, error)
	Feedback(ctx context.Context, kind string) (string, error)
	IsCompanionSession(ctx context.Context, sessionID string) (bool, error)
}

// New builds the tools; none while the companion is off.
func New(cfg *config.Config, svc Service) []tool.Tool {
	if !cfg.Companion.Enabled || svc == nil {
		return nil
	}
	return []tool.Tool{interestUpdate{svc}, feedback{svc}}
}

var errElsewhere = errors.New("this tool only works in the companion conversation, where spore reaches out to the user")

// inCompanion refuses a call from any session but the companion's.
func inCompanion(ctx context.Context, svc Service) error {
	ok, err := svc.IsCompanionSession(ctx, policy.SessionFrom(ctx).ID)
	if err != nil {
		return err
	}
	if !ok {
		return errElsewhere
	}
	return nil
}

func decode(args json.RawMessage, v any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}

type interestUpdate struct{ svc Service }

func (interestUpdate) Name() string { return "interest_update" }

func (interestUpdate) Description() string {
	return "Change an active watch the user approved -- its cron (five fields, UTC, at most hourly) or its goal -- " +
		"or retire an interest (state \"retired\") when the user wants it stopped. It cannot start a watch: " +
		"only the user's Yes button on a proposal does that."
}

func (interestUpdate) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "key": {"type": "string", "description": "The interest key, e.g. stock:zs."},
	    "cron": {"type": "string", "description": "New five-field cron, UTC."},
	    "goal": {"type": "string", "description": "New goal: what to check and when it is worth telling the user."},
	    "state": {"type": "string", "enum": ["retired"]}
	  },
	  "required": ["key"]
	}`)
}

// ReadOnly is false: it changes a job and an interest.
func (interestUpdate) ReadOnly() bool { return false }

func (t interestUpdate) Call(ctx context.Context, args json.RawMessage) (string, error) {
	if err := inCompanion(ctx, t.svc); err != nil {
		return "", err
	}
	var in struct {
		Key   string `json:"key"`
		Cron  string `json:"cron"`
		Goal  string `json:"goal"`
		State string `json:"state"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	return t.svc.UpdateInterest(ctx, in.Key, in.Cron, in.Goal, in.State)
}

type feedback struct{ svc Service }

func (feedback) Name() string { return "companion_feedback" }

func (feedback) Description() string {
	return "Record how the user wants to hear from you unprompted: \"more\", \"less\", or \"stop\" " +
		"(which also pauses your messages for a week). Use it when the user says so, in any words."
}

func (feedback) Schema() json.RawMessage {
	enum, _ := json.Marshal(companion.FeedbackKinds)
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {"kind": {"type": "string", "enum": ` + string(enum) + `}},
	  "required": ["kind"]
	}`)
}

// ReadOnly is false: it changes the governor.
func (feedback) ReadOnly() bool { return false }

func (t feedback) Call(ctx context.Context, args json.RawMessage) (string, error) {
	if err := inCompanion(ctx, t.svc); err != nil {
		return "", err
	}
	var in struct {
		Kind string `json:"kind"`
	}
	if err := decode(args, &in); err != nil {
		return "", err
	}
	return t.svc.Feedback(ctx, in.Kind)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tool/companiontool/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tool/companiontool/
git commit -m "tools: interest_update and companion_feedback, companion conversation only"
```

---

### Task 9: Discord: companion thread or DM, admission, proposal buttons

**Files:**
- Modify: `internal/bridge/discord/client.go` (`Client.UserChannel` + `gatewayClient` impl)
- Modify: `internal/bridge/discord/fake_test.go` (`fakeClient.UserChannel`)
- Modify: `internal/bridge/discord/admit.go` (`WithCompanion`, companion rule)
- Modify: `internal/bridge/discord/bridge.go` (`Options`, `New`, `handleInteraction`)
- Create: `internal/bridge/discord/companion.go`
- Create: `internal/bridge/discord/companion_test.go`

**Interfaces:**
- Consumes: `companion.Channel` (Task 7), `config.CompanionDiscordConfig` (Task 1), `store.Outreach*` (Task 2), `companion.ChoiceYes|NotNow|Never` (Task 6).
- Produces:
  - `Client.UserChannel(ctx, userID string) (channelID string, err error)`.
  - `(Admitter).WithCompanion(channelID string, userIDs []string) Admitter`.
  - `discord.Options.Companion config.CompanionDiscordConfig`, `Options.CompanionOn bool`, `Options.Proposals ProposalAnswerer`.
  - `discord.ProposalAnswerer` interface `{ AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error) }` (`*daemon.Server` satisfies it).
  - `(*Bridge).CompanionChannel(cfg config.CompanionDiscordConfig) *CompanionChannel`; `*CompanionChannel` implements `companion.Channel`.

- [ ] **Step 1: Write the failing tests**

Create `internal/bridge/discord/companion_test.go`:

```go
package discord

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

type fakeProposals struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeProposals) AnswerProposal(_ context.Context, _ int64, choice string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, choice)
	return "Watching ZS.", nil
}

func (f *fakeProposals) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// companionBridge is a bridge whose companion thread opens in channel CC,
// for user U only; V is a bridge user who is not a companion user.
func companionBridge(t *testing.T, mode string) (*Bridge, *fakeClient, *fakeTurns, *store.Store, *fakeProposals) {
	t.Helper()
	st := openTestStore(t)
	f := newFakeClient()
	turns := newFakeTurns()
	props := &fakeProposals{}
	cfg := testDiscordConfig()
	cfg.UserIDs = []string{"U", "V"}
	b, err := New(Options{
		Cfg: cfg, Client: f, Turns: turns, Sessions: &fakeSessions{store: st}, Store: st,
		Broker: daemon.NewBroker(daemon.NewHub()), Throttle: -1,
		Companion:   config.CompanionDiscordConfig{Mode: mode, ChannelID: "CC", UserIDs: []string{"U"}},
		CompanionOn: true, Proposals: props,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, f, turns, st, props
}

func TestCompanionChannelOpensOneThreadAndSendsButtons(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if ch.Name() != store.OutreachChannelDiscord {
		t.Fatalf("name = %q", ch.Name())
	}
	if err := ch.Send(ctx, sid, store.Outreach{ID: 7, Kind: store.OutreachProposal, Text: "Watch ZS?"}); err != nil {
		t.Fatal(err)
	}
	threads := f.allThreads()
	if len(threads) != 1 || threads[0].ChannelID != "CC" {
		t.Fatalf("threads = %+v", threads)
	}
	thread := threads[0].ThreadID
	if bound, ok, _ := st.SessionForExternal(ctx, bridgeName, thread); !ok || bound != sid {
		t.Fatalf("thread bound to %q %v, want %s", bound, ok, sid)
	}
	in := f.sentTo(thread)
	if len(in) != 1 || in[0].Message.Content != "Watch ZS?" || len(in[0].Message.Buttons) != 3 {
		t.Fatalf("thread messages = %+v", in)
	}
	for i, want := range []string{companion.ChoiceYes, companion.ChoiceNotNow, companion.ChoiceNever} {
		id, choice, err := decodeCompanionCustomID(in[0].Message.Buttons[i].CustomID)
		if err != nil || id != 7 || choice != want {
			t.Fatalf("button %d = %q (%d %q %v)", i, in[0].Message.Buttons[i].CustomID, id, choice, err)
		}
	}
	if err := ch.Send(ctx, sid, store.Outreach{ID: 8, Kind: store.OutreachNudge, Text: "Still?"}); err != nil {
		t.Fatal(err)
	}
	if n := len(f.allThreads()); n != 1 {
		t.Fatalf("threads after a second send = %d, want 1", n)
	}
	if got := f.sentTo(thread); len(got) != 2 || len(got[1].Message.Buttons) != 0 {
		t.Fatalf("a nudge carries no buttons: %+v", got)
	}
}

func TestCompanionChannelOpensANewThreadForANewSession(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	first, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	_ = ch.Send(ctx, first, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "a"})
	second, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	_ = ch.Send(ctx, second, store.Outreach{ID: 2, Kind: store.OutreachNudge, Text: "b"})
	if n := len(f.allThreads()); n != 2 {
		t.Fatalf("threads = %d, want one per companion session", n)
	}
}

func TestCompanionChannelDMMode(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordDM)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordDM, UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if bound, ok, _ := st.SessionForExternal(ctx, bridgeName, "DM-U"); !ok || bound != sid {
		t.Fatalf("DM bound to %q %v", bound, ok)
	}
	if got := f.sentTo("DM-U"); len(got) != 1 {
		t.Fatalf("DM messages = %+v", got)
	}
	if n := len(f.allThreads()); n != 0 {
		t.Fatalf("DM mode opened %d threads", n)
	}
}

func TestCompanionChannelSendErrorIsReturned(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	f.setFailNext("Send", errors.New("discord is down"))
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachNudge, Text: "hi"}); err == nil {
		t.Fatal("a failed send must be an error, so the daemon falls back to the terminal")
	}
}

func TestCompanionAdmission(t *testing.T) {
	a := NewAdmitter(config.DiscordConfig{GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U", "V"}}).
		WithCompanion("CC", []string{"U"})
	for _, tc := range []struct {
		name string
		in   Inbound
		want bool
	}{
		{"companion user in a companion thread", Inbound{UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC"}, true},
		{"bridge user who is not a companion user", Inbound{UserID: "V", GuildID: "G", ChannelID: "T", ParentID: "CC"}, false},
		{"top level in the companion channel", Inbound{UserID: "U", GuildID: "G", ChannelID: "CC"}, false},
		{"a stranger", Inbound{UserID: "X", GuildID: "G", ChannelID: "T", ParentID: "CC"}, false},
		{"ordinary channel still works", Inbound{UserID: "V", GuildID: "G", ChannelID: "C1"}, true},
	} {
		if got := a.AdmitMessage(tc.in); got != tc.want {
			t.Errorf("%s: admitted = %v, want %v", tc.name, got, tc.want)
		}
	}
	off := NewAdmitter(config.DiscordConfig{GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"}})
	if off.AdmitMessage(Inbound{UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC"}) {
		t.Error("without the companion, its channel admits nobody")
	}
}

func TestProposalButtonPressReachesTheCompanion(t *testing.T) {
	_, f, _, _, props := companionBridge(t, config.CompanionDiscordThread)
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC",
		CustomID: companionCustomID(7, companion.ChoiceYes)})
	waitFor(t, func() bool { return len(f.allResponds()) == 1 })
	if got := props.all(); len(got) != 1 || got[0] != companion.ChoiceYes {
		t.Fatalf("answers = %v", got)
	}
	if r := f.allResponds()[0]; !strings.Contains(r.Content, "Watching ZS") {
		t.Fatalf("respond = %+v", r)
	}
	// V is a bridge user but not a companion user: the press is dropped.
	f.press(Interaction{ID: "i2", Token: "tok", UserID: "V", GuildID: "G", ChannelID: "T", ParentID: "CC",
		CustomID: companionCustomID(7, companion.ChoiceNever)})
	time.Sleep(50 * time.Millisecond)
	if got := props.all(); len(got) != 1 {
		t.Fatalf("a non-companion user's press was acted on: %v", got)
	}
}

func TestAReplyInTheCompanionThreadContinuesItsSession(t *testing.T) {
	ctx := context.Background()
	_, f, turns, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	if err := st.BindExternal(ctx, bridgeName, "T", sid); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "T", ParentID: "CC", Content: "yes please"})
	turns.waitForTurn(t)
	if s := turns.allStarts(); len(s) != 1 || s[0].sessionID != sid {
		t.Fatalf("starts = %+v, want one in the companion session", s)
	}
}

func TestCompanionCustomIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"companion|x|yes", "companion|7|maybe", "companion|7", "spore|s|1|allow|once"} {
		if _, _, err := decodeCompanionCustomID(s); err == nil {
			t.Errorf("decoded %q", s)
		}
	}
}
```

(`waitFor(t, cond)` already exists in `bridge_test.go`; use it.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/bridge/discord/ -run 'Companion|ProposalButton|ReplyInTheCompanion' -v`
Expected: compile errors (`Options.Companion`, `CompanionChannel`, `WithCompanion`, `companionCustomID` undefined).

- [ ] **Step 3: Implement**

**Client** (`internal/bridge/discord/client.go`): add to the `Client` interface:

```go
	// UserChannel opens (or finds) the DM channel with a user and returns
	// its id. The companion uses it in DM mode.
	UserChannel(ctx context.Context, userID string) (string, error)
```

and to `gatewayClient`:

```go
// UserChannel returns the DM channel with userID, creating it if needed.
func (c *gatewayClient) UserChannel(ctx context.Context, userID string) (string, error) {
	ch, err := c.sess.UserChannelCreate(userID, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("open discord DM with %s: %w", userID, err)
	}
	return ch.ID, nil
}
```

**Fake** (`internal/bridge/discord/fake_test.go`): add

```go
// UserChannel names a user's DM channel "DM-<user>".
func (f *fakeClient) UserChannel(ctx context.Context, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.take("UserChannel"); err != nil {
		return "", err
	}
	return "DM-" + userID, nil
}
```

If any other type in the repo implements `discord.Client` (`go vet ./...` will say), add the same method to it.

**Admitter** (`internal/bridge/discord/admit.go`): add two fields to `Admitter`:

```go
	// companionChannel and companionUsers admit the companion's thread: any
	// thread under that channel, for those users only. Empty when the
	// companion is off or not on Discord in thread mode.
	companionChannel string
	companionUsers   map[string]struct{}
```

add the method:

```go
// WithCompanion returns a copy that also admits threads under channelID for
// userIDs. The companion's channel may be a private one outside the bridge's
// channel_ids; a top-level message there is still not admitted.
func (a Admitter) WithCompanion(channelID string, userIDs []string) Admitter {
	a.companionChannel = channelID
	a.companionUsers = make(map[string]struct{}, len(userIDs))
	for _, u := range userIDs {
		if u != "" {
			a.companionUsers[u] = struct{}{}
		}
	}
	return a
}
```

and replace the end of `admit` (from the `key := channelID` line) with:

```go
	// A thread is admitted on its parent, so threads the bridge opens itself
	// need no entry in the config.
	key := channelID
	if parentID != "" {
		key = parentID
	}
	if _, ok := a.channels[key]; ok {
		return true
	}
	if a.companionChannel != "" && parentID == a.companionChannel {
		_, ok := a.companionUsers[userID]
		return ok
	}
	return false
```

**Options and New** (`internal/bridge/discord/bridge.go`): add to `Options`:

```go
	// Companion is how the companion reaches the user here; CompanionOn is
	// whether it does at all. Off, the companion's channel admits nobody.
	Companion   config.CompanionDiscordConfig
	CompanionOn bool
	// Proposals answers the companion's proposal buttons. Nil means a press
	// is answered "the companion is off".
	Proposals ProposalAnswerer
```

add `proposals ProposalAnswerer` to `Bridge`, and in `New` build the admitter as:

```go
	admit := NewAdmitter(o.Cfg)
	if o.CompanionOn && o.Companion.Mode == config.CompanionDiscordThread && o.Companion.ChannelID != "" {
		admit = admit.WithCompanion(o.Companion.ChannelID, o.Companion.UserIDs)
	}
```

using `admit: admit` and `proposals: o.Proposals` in the returned struct.

In `handleInteraction`, after the `isModelCustomID` branch, add:

```go
	if isCompanionCustomID(i.CustomID) {
		b.answerProposal(i)
		return
	}
```

**Companion surface** (`internal/bridge/discord/companion.go`):

```go
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// ProposalAnswerer acts on a companion proposal's button. The daemon is one.
type ProposalAnswerer interface {
	AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error)
}

const companionPrefix = "companion|"

// companionCustomID packs a proposal answer into a button's custom id.
func companionCustomID(outreachID int64, choice string) string {
	return companionPrefix + strconv.FormatInt(outreachID, 10) + "|" + choice
}

func isCompanionCustomID(s string) bool { return strings.HasPrefix(s, companionPrefix) }

// decodeCompanionCustomID parses a proposal button's custom id. It arrives
// from the network, so anything off-shape is an error, never a default.
func decodeCompanionCustomID(s string) (int64, string, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 || parts[0]+"|" != companionPrefix {
		return 0, "", fmt.Errorf("not a companion component id: %q", s)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("bad outreach id in %q: %w", s, err)
	}
	switch parts[2] {
	case companion.ChoiceYes, companion.ChoiceNotNow, companion.ChoiceNever:
	default:
		return 0, "", fmt.Errorf("bad choice in %q", s)
	}
	return id, parts[2], nil
}

// proposalButtons are a proposal's three answers.
func proposalButtons(outreachID int64) []Button {
	return []Button{
		{CustomID: companionCustomID(outreachID, companion.ChoiceYes), Label: "✅ Yes, watch it"},
		{CustomID: companionCustomID(outreachID, companion.ChoiceNotNow), Label: "⏸ Not now"},
		{CustomID: companionCustomID(outreachID, companion.ChoiceNever), Label: "✖ Never", Danger: true},
	}
}

// answerProposal handles a proposal button press: answer it, then say how it
// went, privately, inside Discord's three-second window.
func (b *Bridge) answerProposal(i Interaction) {
	id, choice, err := decodeCompanionCustomID(i.CustomID)
	if err != nil {
		return // not ours, or malformed: silence
	}
	msg := "the companion is not enabled"
	if b.proposals != nil {
		if msg, err = b.proposals.AnswerProposal(b.ctx, id, choice); err != nil {
			msg = "could not record that: " + err.Error()
		}
	}
	if err := b.client.Respond(b.ctx, i.ID, i.Token, msg); err != nil {
		slog.Warn("discord companion response", "err", err)
	}
}

// CompanionChannel delivers the companion's outreach on Discord.
type CompanionChannel struct {
	b   *Bridge
	cfg config.CompanionDiscordConfig
}

// CompanionChannel returns the companion's Discord channel.
func (b *Bridge) CompanionChannel(cfg config.CompanionDiscordConfig) *CompanionChannel {
	return &CompanionChannel{b: b, cfg: cfg}
}

var _ companion.Channel = (*CompanionChannel)(nil)

// Name implements companion.Channel.
func (c *CompanionChannel) Name() string { return store.OutreachChannelDiscord }

// Send implements companion.Channel: it posts o where the companion session
// is bound, opening that place first if this is the session's first
// message. A proposal carries its three buttons.
func (c *CompanionChannel) Send(ctx context.Context, sessionID string, o store.Outreach) error {
	channelID, err := c.channelFor(ctx, sessionID)
	if err != nil {
		return err
	}
	m := Message{Content: o.Text}
	if o.Kind == store.OutreachProposal {
		m.Buttons = proposalButtons(o.ID)
	}
	_, err = c.b.client.Send(ctx, channelID, m)
	return err
}

// channelFor is the session's bound channel, or a new one: a thread opened
// under the companion channel, or the first companion user's DM. It holds
// resolveMu so a reply racing the first outreach cannot bind twice.
func (c *CompanionChannel) channelFor(ctx context.Context, sessionID string) (string, error) {
	c.b.resolveMu.Lock()
	defer c.b.resolveMu.Unlock()
	if ch, ok := c.b.boundChannel(ctx, sessionID); ok {
		return ch, nil
	}
	if len(c.cfg.UserIDs) == 0 {
		return "", fmt.Errorf("companion.discord has no user to reach")
	}
	var channelID string
	switch c.cfg.Mode {
	case config.CompanionDiscordDM:
		dm, err := c.b.client.UserChannel(ctx, c.cfg.UserIDs[0])
		if err != nil {
			return "", err
		}
		channelID = dm
	default:
		anchor, err := c.b.client.Send(ctx, c.cfg.ChannelID, Message{Content: "spore: things I noticed and want to check with you"})
		if err != nil {
			return "", err
		}
		thread, err := c.b.client.CreateThread(ctx, c.cfg.ChannelID, anchor, "spore")
		if err != nil {
			return "", err
		}
		channelID = thread
	}
	if err := c.b.store.BindExternal(ctx, bridgeName, channelID, sessionID); err != nil {
		return "", err
	}
	return channelID, nil
}
```

Note: `TestCompanionChannelSendErrorIsReturned` queues the failure on the first `Send`, which is the anchor message, so the error comes from `channelFor`; either way `Send` returns it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/bridge/... -race`
Expected: PASS (new and existing bridge tests).

- [ ] **Step 5: Commit**

```bash
git add internal/bridge/discord/
git commit -m "discord: companion thread or DM, its admission rule, proposal buttons"
```

---

### Task 10: Wiring, the CLI, and the sidebar

**Files:**
- Modify: `internal/refine/refine.go` (`TriggerHeartbeat`)
- Modify: `cmd/spore/wire.go` (`buildTools`, `buildAgent`, `agentParts`, `buildServer`, `buildBridge`)
- Modify: `cmd/spore/wire_test.go`, `cmd/spore/kernel_wire_test.go` (the `buildTools` call gains a trailing `nil`)
- Modify: `cmd/spore/serve.go` (heartbeat goroutine, companion channel)
- Modify: `cmd/spore/companion.go`, `cmd/spore/companion_test.go`, `cmd/spore/main.go` (usage lines)
- Modify: `internal/tui/sidebar.go`, `internal/tui/sidebar_test.go`

**Interfaces:**
- Consumes: everything above.
- Produces: `refine.TriggerHeartbeat = "heartbeat"`; `buildTools(..., comp *companion.Service)`; `agentParts.companion`; CLI `spore companion status | interests | outreach | pause [duration] | resume | answer <id> yes|not-now|never | interests undecline <key>`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/spore/companion_test.go`:

```go
func TestCompanionStatusShowsTheHeartbeat(t *testing.T) {
	ctx := context.Background()
	cfg, st := companionFixture(t)
	_ = st.SetCompanionState(ctx, companion.StateFailures, "3")
	_ = st.SetCompanionState(ctx, companion.StateLastResult, "failed: provider down")
	_ = companion.SetPausedUntil(ctx, st, time.Now().Add(time.Hour))
	var out bytes.Buffer
	if err := companionStatus(ctx, &out, cfg, st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"channel:   terminal", "paused until", "budget:   ", "0 of 4 sent today", "failed: provider down", "warning: the last 3 reflections failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}

func TestCompanionOutreachLists(t *testing.T) {
	ctx := context.Background()
	_, st := companionFixture(t)
	_, _ = st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Channel: store.OutreachChannelDiscord,
		Text: "You've checked ZS three days running.", SentAt: time.Now().Add(-time.Hour)})
	var out bytes.Buffer
	if err := companionOutreach(ctx, &out, st, time.UTC); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "proposal") ||
		!strings.Contains(lines[1], "pending") || !strings.Contains(lines[1], "checked ZS") {
		t.Fatalf("outreach =\n%s", out.String())
	}
}

func TestCompanionPauseResumeUndecline(t *testing.T) {
	ctx := context.Background()
	cfg, st := companionFixture(t)
	var out bytes.Buffer
	if err := companionCommand(ctx, &out, cfg, st, []string{"pause", "2h"}); err != nil {
		t.Fatal(err)
	}
	if until, _ := companion.PausedUntil(ctx, st); time.Until(until) < 119*time.Minute {
		t.Fatalf("paused until %v", until)
	}
	if err := companionCommand(ctx, &out, cfg, st, []string{"resume"}); err != nil {
		t.Fatal(err)
	}
	if until, _ := companion.PausedUntil(ctx, st); !until.IsZero() {
		t.Fatalf("still paused until %v", until)
	}
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	_, _ = st.DeclineInterest(ctx, in.ID)
	if err := companionCommand(ctx, &out, cfg, st, []string{"interests", "undecline", "stock:zs"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestObserving {
		t.Fatalf("state = %s", got.State)
	}
	if err := companionCommand(ctx, &out, cfg, st, []string{"pause", "soon"}); err == nil {
		t.Fatal("a bad duration must be refused")
	}
}
```

Add `"github.com/codered/spore/internal/companion"` to that file's imports.

Append to `internal/tui/sidebar_test.go`:

```go
// The companion session is where spore reaches out; it must not hide behind
// "show all" like a job or a bridge session.
func TestTheDefaultFilterShowsTheCompanionSession(t *testing.T) {
	c := seed(
		daemon.SessionJSON{ID: "c1", Title: "chat", Workspace: "/a", Source: "chat"},
		daemon.SessionJSON{ID: "sp", Title: "spore", Workspace: "/s", Source: "companion"},
	)
	if got := strings.Join(ids(c.rows(false, "", "")), " "); !strings.Contains(got, "sp@0") {
		t.Fatalf("default rows = %q, want the idle companion session", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/spore/ -run 'Companion' -v; go test ./internal/tui/ -run 'Companion' -v`
Expected: compile errors in `cmd/spore` (`companionOutreach`, `companionCommand` undefined) and a FAIL in `internal/tui`.

- [ ] **Step 3: Implement**

**Trigger** (`internal/refine/refine.go`): add to the `Trigger` constants:

```go
	// TriggerHeartbeat marks a self.md rewrite the companion's reflection made.
	TriggerHeartbeat Trigger = "heartbeat"
```

**Sidebar** (`internal/tui/sidebar.go`): in `visible`, change `case "chat", "unknown":` to `case "chat", "unknown", "companion":` and extend the comment above it: "The companion session is where spore reaches out unprompted, so it shows too."

**Wiring** (`cmd/spore/wire.go`):

1. `buildTools` gains a last parameter `comp *companion.Service`. After `tools = append(tools, refinetool.New(ref))`, add:

```go
	// A nil *companion.Service must not reach New as a non-nil interface.
	if comp != nil {
		tools = append(tools, companiontool.New(cfg, comp)...)
	}
```

Import `"github.com/codered/spore/internal/tool/companiontool"`. Update the two test callers (`cmd/spore/wire_test.go:113`, `cmd/spore/kernel_wire_test.go:69`) to pass a trailing `nil`.

2. `agentParts` gains `companion *companion.Service`. In `buildAgent`, after `ref.Signals = companion.NewRecorder(st, cfg)`:

```go
	// The companion's service backs its tools, the proposal route and the
	// reply hook. It exists only while the companion is on.
	var comp *companion.Service
	if cfg.Companion.Enabled {
		comp = companion.NewService(st, cfg)
	}
```

pass `comp` to `buildTools`, and set `companion: comp` in the returned `agentParts`.

3. In `buildServer`, after `srv.AttachRefiner(ref)`:

```go
	if parts.companion != nil {
		srv.AttachCompanion(parts.companion)
		srv.AttachHeartbeat(&companion.Heartbeat{
			Store: st, Cfg: cfg, Recorder: ref.Signals,
			Reflector: companion.LLMReflector{Registry: ref.Registry, Router: ref.Router,
				MaxTokens: max(cfg.Context.MaxOutputTokens, 4096)},
			Deliverer: srv.CompanionDeliverer(),
			WriteSelf: func(ctx context.Context, sid, body string) error {
				_, err := ref.UpdateSelf(ctx, sid, refine.TriggerHeartbeat, "heartbeat rewrite of self.md",
					func(string) (string, error) { return body, nil })
				return err
			},
			Now: time.Now,
		})
	}
```

(Add `"context"` to the imports if it is not there.)

4. In `buildBridge`, extend `opts`:

```go
	opts := discord.Options{
		Cfg: d, Client: client, Turns: srv, Sessions: srv,
		Store: srv.Store(), Broker: srv.Broker(), Guard: srv.Guard(),
		ShowCost:    cfg.ShowCost,
		Companion:   cfg.Companion.Discord,
		CompanionOn: cfg.Companion.Enabled,
		Proposals:   srv,
	}
```

**Serve** (`cmd/spore/serve.go`): directly after `go func() { _ = sched.Run(...) }()`, add:

```go
	if hb := srv.Heartbeat(); hb != nil {
		if bridge != nil && cfg.CompanionChannel() == config.CompanionChannelDiscord {
			srv.SetCompanionChannel(bridge.CompanionChannel(cfg.Companion.Discord))
		}
		every, err := time.ParseDuration(cfg.Companion.Heartbeat)
		if err != nil {
			return fmt.Errorf("companion.heartbeat: %w", err) // Load already rejected this; belt and braces
		}
		go func() {
			// The bridge connects in the background; a first beat at startup
			// would find it not yet open and fall back to the terminal.
			select {
			case <-time.After(time.Minute):
			case <-ctx.Done():
				return
			}
			hb.Run(ctx, every)
		}()
		if !detach {
			fmt.Printf("companion on (channel %s, heartbeat %s)\n", cfg.CompanionChannel(), every)
		}
	}
```

**CLI** (`cmd/spore/companion.go`): replace `cmdCompanion` and `companionStatus`, and add the new commands:

```go
// cmdCompanion is the operator's view of the companion. Everything but
// "answer" reads and writes the database directly, like spore recall, so it
// works with or without a running daemon; "answer" goes through the daemon
// so the answer is posted where the proposal was.
func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore companion status | interests | outreach | pause [duration] | resume | answer <id> yes|not-now|never | interests undecline <key>")
	}
	if args[0] == "answer" {
		return companionAnswer(ctx, cfg, args[1:])
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return companionCommand(ctx, os.Stdout, cfg, st, args)
}

// companionCommand runs every subcommand that needs only the store.
func companionCommand(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store, args []string) error {
	loc, err := cfg.Companion.Location()
	if err != nil {
		return err
	}
	svc := companion.NewService(st, cfg)
	switch args[0] {
	case "status":
		return companionStatus(ctx, w, cfg, st)
	case "interests":
		if len(args) == 3 && args[1] == "undecline" {
			if err := svc.Undecline(ctx, args[2]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "%s is observed again\n", args[2])
			return nil
		}
		if len(args) > 1 {
			return fmt.Errorf("usage: spore companion interests [undecline <key>]")
		}
		return companionInterests(ctx, w, st, loc)
	case "outreach":
		return companionOutreach(ctx, w, st, loc)
	case "pause":
		d := 24 * time.Hour
		if len(args) > 1 {
			if d, err = time.ParseDuration(args[1]); err != nil {
				return fmt.Errorf("pause duration %q: %w (try 2h or 72h)", args[1], err)
			}
		}
		until, err := svc.Pause(ctx, d)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(w, "paused until %s\n", until.In(loc).Format("Mon 2006-01-02 15:04"))
		return nil
	case "resume":
		if err := svc.Resume(ctx); err != nil {
			return err
		}
		_, _ = fmt.Fprintln(w, "resumed")
		return nil
	default:
		return fmt.Errorf("unknown companion command %q: want status, interests, outreach, pause, resume or answer", args[0])
	}
}

func companionStatus(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store) error {
	c := cfg.Companion
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	loc, err := c.Location()
	if err != nil {
		return err
	}
	s, err := companion.NewService(st, cfg).Status(ctx)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(w, "companion: %s\n", state)
	if !s.PausedUntil.IsZero() && time.Now().Before(s.PausedUntil) {
		_, _ = fmt.Fprintf(w, "           paused until %s\n", s.PausedUntil.In(loc).Format("Mon 2006-01-02 15:04"))
	}
	_, _ = fmt.Fprintf(w, "channel:   %s\n", s.Channel)
	_, _ = fmt.Fprintf(w, "timezone:  %s (habit_days %d, quiet %s)\n", loc, c.HabitDays, c.QuietHours)
	_, _ = fmt.Fprintf(w, "budget:    %.2f a day, %d of %d sent today\n", s.Budget, s.SpentToday, int(s.Budget))
	last := "never"
	switch {
	case !s.LastReflection.IsZero():
		last = s.LastReflection.In(loc).Format("Mon 15:04") + ": " + s.LastResult
	case s.LastResult != "":
		last = s.LastResult
	}
	_, _ = fmt.Fprintf(w, "reflected: %s\n", last)
	if s.Failures >= companion.FailureWarning {
		_, _ = fmt.Fprintf(w, "warning: the last %d reflections failed; see the daemon log\n", s.Failures)
	}
	size := 0
	if fi, err := os.Stat(cfg.SelfPath()); err == nil { //nolint:gosec // G703: path is the daemon's own self.md under DataDir, not user input
		size = int(fi.Size())
	}
	_, _ = fmt.Fprintf(w, "self.md:   %s, %d of %d bytes\n", cfg.SelfPath(), size, c.SelfMaxBytes)
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, in := range all {
		counts[in.State]++
	}
	_, _ = fmt.Fprint(w, "interests:")
	for _, s := range []string{store.InterestObserving, store.InterestCandidate, store.InterestProposed,
		store.InterestActive, store.InterestDeclined, store.InterestRetired} {
		_, _ = fmt.Fprintf(w, " %s %d", s, counts[s])
	}
	_, _ = fmt.Fprintln(w)
	return nil
}

// companionOutreach lists the last week's outreach, oldest first.
func companionOutreach(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	rows, err := st.OutreachSince(ctx, time.Now().Add(-companion.RecentOutreach))
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "no outreach in the last 7 days")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tKIND\tOUTCOME\tCHANNEL\tSENT\tTEXT")
	for _, o := range rows {
		text := strings.Join(strings.Fields(o.Text), " ")
		if r := []rune(text); len(r) > 60 {
			text = string(r[:60]) + "…"
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", o.ID, o.Kind, o.Outcome, o.Channel,
			o.SentAt.In(loc).Format("Mon 15:04"), text)
	}
	return tw.Flush()
}

// companionAnswer presses a proposal's button from the terminal.
func companionAnswer(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: spore companion answer <outreach id> yes|not-now|never")
	}
	if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
		return fmt.Errorf("outreach id %q must be a number", args[0])
	}
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	var out struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/companion/proposals/"+args[0], map[string]string{"choice": args[1]}, &out); err != nil {
		return err
	}
	fmt.Println(out.Message)
	return nil
}
```

Imports for `cmd/spore/companion.go`: `context`, `fmt`, `io`, `net/http`, `os`, `strconv`, `strings`, `text/tabwriter`, `time`, and the repo's `companion`, `config`, `store`.

**Usage** (`cmd/spore/main.go`): replace the two companion lines with:

```
  spore companion status       report whether the companion is on, its budget and last reflection
  spore companion interests    list the interests it has noticed, with their evidence
  spore companion outreach     list the messages it started in the last 7 days
  spore companion pause [dur]  hold its messages (default 24h); resume ends a pause
  spore companion answer <id> yes|not-now|never
                               answer a proposal from the terminal
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go build ./... && go test ./cmd/spore/ ./internal/tui/ ./internal/refine/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/spore/ internal/refine/refine.go internal/tui/
git commit -m "companion: wire the heartbeat, tools, Discord channel and CLI; show the companion session"
```

---

### Task 11: Docs, the full gate, and the manual check

**Files:**
- Modify: `README.md` (the companion section)
- Modify: `docs/superpowers/specs/2026-10-10-companion-design.md` (append "As built — PR 2a")
- Modify: `docs/backlog.md` (PR 2b and PR 3 items, if the backlog tracks them)

- [ ] **Step 1: README**

In the README's companion section (added in PR 1; find it with `grep -n -i companion README.md`), add after the existing text:

````markdown
With `enabled = true` the companion also reaches out. Every `heartbeat`
(30m by default) it looks at habits it has seen on `habit_days` days in
the last month and at messages you have not answered. Now and then it
offers to watch one, with Yes / Not now / Never buttons. It nudges an
unanswered message at most once, and sends fewer messages when you do not
answer. Only Yes starts a watch. A watch is a scheduled job that runs with
read-only tools and posts every run where the proposal was.

On Discord it uses a thread of its own in a channel you name, which can be
a private channel just for you:

```toml
[companion]
enabled = true

[companion.discord]
mode       = "thread"            # or "dm"
channel_id = "123456789012345678"
user_ids   = ["your user id"]    # who may talk there; defaults to bridge.discord.user_ids
```

Without a channel id, the companion's messages appear in its own `spore`
session in the TUI and web UI, and `spore companion answer <id> yes`
answers a proposal. `spore companion status`, `outreach`, `pause` and
`resume` show and steer it.

If your `[policy.profile.remote]` lists its own `allow` rules, add
`"interest_update"` and `"companion_feedback"` there so the companion
thread can use them without asking.
````

- [ ] **Step 2: Spec "As built — PR 2a"**

Append to `docs/superpowers/specs/2026-10-10-companion-design.md`:

```markdown
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
```

- [ ] **Step 3: Run the full gate**

Run each and read the output:

```bash
make vet
make fmtcheck
make lint
make vulncheck
make tidycheck
go test ./... -race
TMPDIR=$(mktemp -d)/link && ln -s "$(mktemp -d)" "$TMPDIR" && TMPDIR=$TMPDIR go test ./internal/companion/ ./internal/daemon/ ./cmd/spore/
```

Expected: every command exits 0. (If `make vulncheck` reports a new advisory in a dependency this branch did not touch, say so rather than bumping it here.)

- [ ] **Step 4: Manual check (an isolated daemon, a real Discord thread)**

Follow `.spore/agent.md` and the "spore once uses the running daemon" rule: run a branch build with its own `HOME`, port 7788, and `policy.workspace`, never the user's running daemon. In its config set `[companion] enabled = true`, `heartbeat = "5m"`, `quiet_hours` to a window that excludes now, and `[companion.discord]` with a private test channel id.

1. Plant three days of signals for `stock:zs` (via the store, or by `/refine` on three chat sessions with backdated signals), then `spore companion interests` shows it as `candidate`.
2. Wait for a heartbeat (the first one comes one minute after start), or restart to trigger one. A `spore` thread opens in the channel with a proposal and three buttons.
3. Press Yes. An ephemeral confirmation appears, then a note in the thread. `spore companion interests` shows `active`, and the Jobs folder shows the watch with notify `each`.
4. Trigger the job (or set its cron to the next minute with `interest_update` by replying "check it at :MM past"). The run's reply lands in the thread.
5. Reply in the thread. `spore companion outreach` shows the proposal `reacted`, and anything pending becomes `replied`.

Record what you saw in the PR description. If a step does not behave as written, stop and report it; do not patch around it.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/
git commit -m "docs: the companion reaches out (PR 2a)"
```
