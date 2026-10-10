# Companion PR 1: observe + self — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** With `[companion] enabled = true`, spore records the user's recurring interests from refinement rounds, promotes one seen on `habit_days` distinct local days to a candidate, and keeps its own `self.md` (written with `self_note`, ledgered, shown in the prompt). It says nothing unprompted yet.

**Architecture:** A new `internal/companion` package owns signal validation, the trust rule, day counting and the interest state sweep, plus the `self.md` text helpers. The store gains two tables. The refiner asks its planner for `signals` beside `edits` and hands them to a `*companion.Recorder`; it also gains `UpdateSelf`, a ledgered writer for `self.md` that the existing rollback understands. A `self_note` tool and a prompt section complete the self half. Everything is gated on `cfg.Companion.Enabled`; off means byte-for-byte today's behaviour.

**Tech Stack:** Go, SQLite (mattn/go-sqlite3, `-tags sqlite_fts5`), BurntSushi/toml.

**Spec:** `docs/superpowers/specs/2026-10-10-companion-design.md` (sections 2, 3, 5.4's `self_note` paragraph, 7, 8 item 1). Read it before starting.

## Global Constraints

- Every `go test` uses `-tags sqlite_fts5`. The full gate before the PR: `make vet fmtcheck lint test` plus `make vulncheck tidycheck`.
- `[companion]` defaults: `enabled = false`, `channel = "auto"`, `quiet_hours = "22:00-08:00"`, `timezone = ""`, `habit_days = 3`, `heartbeat = "30m"`, `self_max_bytes = 10240`, `start_budget = 4`, `alert_budget = 3`, `alerts_in_quiet_hours = false`.
- Validation ranges: `channel` in `auto|discord|terminal`; `quiet_hours` `HH:MM-HH:MM`, start != end; `timezone` must load with `time.LoadLocation` (empty = `time.Local`); `habit_days` 1..30; `heartbeat` >= 5m; `self_max_bytes` 1024..65536; `start_budget`, `alert_budget` >= 1.
- Signal key regex (after lowercasing and trimming): `^(stock|crypto|topic|person|place|project):[a-z0-9._-]{1,48}$`. Kinds: `asked`, `mentioned`, `acted`. Label 1..80 characters. At most 10 signals per round.
- Signals count only from sessions with `Source` `chat` or `discord` and no `ParentID`. Never from `job`, `subagent`, `companion`, `unknown`.
- Interest states: `observing`, `candidate`, `proposed`, `active`, `declined`, `retired`. Fade: `observing`/`candidate` with no signal for 30 days → `retired`.
- `self.md` lives at `<DataDir>/self.md`. Headings, in order: `## What I'm curious about`, `## Threads with you`, `## How you like to be talked to`, `## Opinions I've formed`. Prompt heading: `## Your own notes`, placed directly after `## Who you are`.
- Ledger kind for `self.md` writes: `self.update`.
- `self_note`: default policy allow (local), deny in the `remote` profile, non-learnable. Registered only when `companion.enabled`.
- Tests that assert a policy decision build the config with `config.Load` from a TOML file, never `config.Default()` (Default skips the baseline deny).
- Temp dirs in tests that compare paths are passed through `filepath.EvalSymlinks` (macOS symlinks `/var` → `/private/var`).
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr
  ```
- Stage files by name. Never `git add -A`.

## Review Focus

1. **A local midnight splits days the UTC date does not.** Signals at 23:30 and 00:30 Pacific are two days, though they share a UTC date. Pinned in Task 3 (`TestRecordCountsLocalDaysNotUTCDays`).
2. **A malformed signal must never cost the round its edits.** A planner reply whose `signals` holds a number, a bad key or a missing kind still applies its valid edits. Pinned in Task 4 (`TestRoundKeepsEditsWhenSignalsAreMalformed`).
3. **Spore's own sessions must not feed its interests.** A job or companion session's round neither asks for signals nor records them. Pinned in Task 3 (`TestRecordRefusesUntrustedSources`) and Task 4 (`TestRoundOnJobSessionDoesNotAskForSignals`).
4. **Companion off is no change at all.** No signals section in the planner prompt, no `self_note` tool, no `## Your own notes` even if `self.md` exists. Pinned in Task 4, Task 6 and Task 7.
5. **A hand-edited `self.md` without the standard headings still takes a note.** The user owns the file too; `self_note` adds the missing heading instead of refusing. Pinned in Task 5 (`TestAppendSelfNoteAddsAMissingHeading`).

---

### Task 1: `[companion]` config and `SelfPath`

**Files:**
- Modify: `internal/config/config.go` (struct field at line 45, `Default()` at 556-594, `Load()` at 710-853, `Validate()` at 855-946, accessors near 967)
- Test: `internal/config/config_test.go`, `internal/config/companion_policy_test.go` (create)

**Interfaces:**
- Produces:
  - `type CompanionConfig struct { Enabled bool; Channel, QuietHours, Timezone string; HabitDays int; Heartbeat string; SelfMaxBytes, StartBudget, AlertBudget int; AlertsInQuietHours bool }`, field `Config.Companion`
  - `func (c CompanionConfig) Location() (*time.Location, error)`
  - `func ParseQuietHours(s string) (startMin, endMin int, err error)`
  - `func (c *Config) SelfPath() string`
  - constants `CompanionChannelAuto = "auto"`, `CompanionChannelDiscord = "discord"`, `CompanionChannelTerminal = "terminal"`

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestCompanionDefaults(t *testing.T) {
	cfg := loadTestConfig(t, "")
	c := cfg.Companion
	if c.Enabled {
		t.Error("companion must be off by default so an upgrade sends nobody anything")
	}
	if c.Channel != "auto" || c.QuietHours != "22:00-08:00" || c.Timezone != "" || c.HabitDays != 3 ||
		c.Heartbeat != "30m" || c.SelfMaxBytes != 10240 || c.StartBudget != 4 || c.AlertBudget != 3 || c.AlertsInQuietHours {
		t.Fatalf("defaults = %+v", c)
	}
	if got := cfg.SelfPath(); got != filepath.Join(cfg.DataDir, "self.md") {
		t.Fatalf("SelfPath = %q", got)
	}
}

func TestCompanionPartialBlockKeepsOtherDefaults(t *testing.T) {
	cfg := loadTestConfig(t, "[companion]\nenabled = true\nhabit_days = 5\ntimezone = \"America/Los_Angeles\"\n")
	c := cfg.Companion
	if !c.Enabled || c.HabitDays != 5 || c.SelfMaxBytes != 10240 || c.QuietHours != "22:00-08:00" {
		t.Fatalf("companion = %+v", c)
	}
	loc, err := c.Location()
	if err != nil || loc.String() != "America/Los_Angeles" {
		t.Fatalf("Location = %v, %v", loc, err)
	}
}

func TestCompanionEmptyTimezoneIsLocal(t *testing.T) {
	loc, err := CompanionConfig{}.Location()
	if err != nil || loc != time.Local {
		t.Fatalf("Location = %v, %v; want time.Local", loc, err)
	}
}

func TestCompanionValidation(t *testing.T) {
	for _, body := range []string{
		"channel = \"email\"",
		"quiet_hours = \"22-08\"",
		"quiet_hours = \"25:00-08:00\"",
		"quiet_hours = \"08:00-08:00\"",
		"timezone = \"Mars/Olympus\"",
		"habit_days = 31",
		"habit_days = -1",
		"heartbeat = \"1m\"",
		"heartbeat = \"soon\"",
		"self_max_bytes = 512",
		"self_max_bytes = 70000",
		"start_budget = -1",
		"alert_budget = -2",
	} {
		p := writeConfig(t, "[companion]\n"+body+"\n")
		if _, err := Load(p); err == nil {
			t.Errorf("Load accepted [companion] %s", body)
		}
	}
}

func TestParseQuietHours(t *testing.T) {
	s, e, err := ParseQuietHours("22:00-08:30")
	if err != nil || s != 22*60 || e != 8*60+30 {
		t.Fatalf("ParseQuietHours = %d, %d, %v", s, e, err)
	}
}
```

Create `internal/config/companion_policy_test.go`:

```go
package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

// self_note writes text that rides in every later prompt, so the remote
// profile denies it for the same reason it denies memory. Checked through
// config.Load and the engine, never config.Default(): Default skips the
// baseline deny and the profile merge Load performs.
func TestSelfNotePolicyByProfile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(p, []byte("default_model = \"anthropic/claude-opus-5\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	e, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatal(err)
	}
	call := policy.Call{Tool: "self_note", Args: json.RawMessage(`{"heading":"Threads with you","text":"x"}`)}
	ws := filepath.Dir(p)
	if got := e.Evaluate(policy.Session{ID: "s", Profile: policy.ProfileLocal, Workspace: ws}, call); got.Decision != policy.DecisionAllow {
		t.Errorf("local self_note = %q, want allow", got.Decision)
	}
	if got := e.Evaluate(policy.Session{ID: "s", Profile: policy.ProfileRemote, Workspace: ws}, call); got.Decision != policy.DecisionDeny {
		t.Errorf("remote self_note = %q, want deny", got.Decision)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/config/ -run 'Companion|QuietHours|SelfNote' -v`
Expected: build failure — `cfg.Companion undefined`, `ParseQuietHours undefined`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add to the `Config` struct after `Refine    RefineConfig   \`toml:"refine"\``:

```go
	Companion CompanionConfig `toml:"companion"`
```

Add after `func (r RefineConfig) On() bool { ... }`:

```go
// CompanionConfig drives the companion: spore noticing the user's recurring
// interests, keeping its own self.md, and (later) reaching out unprompted.
// Enabled defaults to false so an upgrade starts messaging nobody.
type CompanionConfig struct {
	Enabled bool `toml:"enabled"`
	// Channel is where unprompted messages go: auto, discord or terminal.
	Channel string `toml:"channel"`
	// QuietHours is "HH:MM-HH:MM" local time; it may wrap midnight.
	QuietHours string `toml:"quiet_hours"`
	// Timezone is an IANA name. Empty means the daemon host's zone. It
	// decides what a "day" is when counting how often an interest came up.
	Timezone string `toml:"timezone"`
	// HabitDays is how many distinct local days an interest must come up on
	// before it is a candidate habit.
	HabitDays int `toml:"habit_days"`
	// Heartbeat is a Go duration: how often the companion reflects.
	Heartbeat string `toml:"heartbeat"`
	// SelfMaxBytes caps self.md.
	SelfMaxBytes int `toml:"self_max_bytes"`
	// StartBudget is unprompted messages per day before engagement adjusts
	// it; AlertBudget is a separate daily allowance for watch alerts.
	StartBudget        int  `toml:"start_budget"`
	AlertBudget        int  `toml:"alert_budget"`
	AlertsInQuietHours bool `toml:"alerts_in_quiet_hours"`
}

// Companion channels.
const (
	CompanionChannelAuto     = "auto"
	CompanionChannelDiscord  = "discord"
	CompanionChannelTerminal = "terminal"
)

// Location is the zone days are counted in. Empty means the host's zone.
func (c CompanionConfig) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.Local, nil
	}
	return time.LoadLocation(c.Timezone)
}

// ParseQuietHours reads "HH:MM-HH:MM" into minutes after midnight. Start and
// end may wrap midnight but may not be equal: that would mean always or
// never, and neither is what anyone writing a range means.
func ParseQuietHours(s string) (startMin, endMin int, err error) {
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("quiet_hours %q must be HH:MM-HH:MM", s)
	}
	clock := func(v string) (int, error) {
		t, err := time.Parse("15:04", strings.TrimSpace(v))
		if err != nil {
			return 0, fmt.Errorf("quiet_hours %q must be HH:MM-HH:MM", s)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	if startMin, err = clock(from); err != nil {
		return 0, 0, err
	}
	if endMin, err = clock(to); err != nil {
		return 0, 0, err
	}
	if startMin == endMin {
		return 0, 0, fmt.Errorf("quiet_hours %q starts and ends at the same time", s)
	}
	return startMin, endMin, nil
}
```

In `Default()`:
- add `"self_note"` to the end of `Allow`;
- change the remote profile to `"remote": {Deny: []string{"mcp__*", "memory", "skill_install", "agent_note", "self_note"}},`;
- add after `Refine: RefineConfig{IdleMinutes: 10, MaxEdits: 5},`:

```go
		Companion: CompanionConfig{Channel: CompanionChannelAuto, QuietHours: "22:00-08:00", HabitDays: 3,
			Heartbeat: "30m", SelfMaxBytes: 10240, StartBudget: 4, AlertBudget: 3},
```

In `Load()`, immediately after the `if cfg.Refine.MaxEdits == 0 { ... }` block:

```go
	// Zero means "not set in the file", as for [refine]: a [companion] block
	// that only sets enabled keeps every other default.
	fillCompanionDefaults(&cfg.Companion, d.Companion)
```

Add next to `fillKernelDefaults`:

```go
// fillCompanionDefaults replaces each zero field with its default. A
// negative value is left for Validate to reject.
func fillCompanionDefaults(c *CompanionConfig, d CompanionConfig) {
	if c.Channel == "" {
		c.Channel = d.Channel
	}
	if c.QuietHours == "" {
		c.QuietHours = d.QuietHours
	}
	if c.HabitDays == 0 {
		c.HabitDays = d.HabitDays
	}
	if c.Heartbeat == "" {
		c.Heartbeat = d.Heartbeat
	}
	if c.SelfMaxBytes == 0 {
		c.SelfMaxBytes = d.SelfMaxBytes
	}
	if c.StartBudget == 0 {
		c.StartBudget = d.StartBudget
	}
	if c.AlertBudget == 0 {
		c.AlertBudget = d.AlertBudget
	}
}
```

In `Validate()`, before the final `return nil`:

```go
	if err := c.Companion.validate(); err != nil {
		return err
	}
```

And add:

```go
func (c CompanionConfig) validate() error {
	switch c.Channel {
	case CompanionChannelAuto, CompanionChannelDiscord, CompanionChannelTerminal:
	default:
		return fmt.Errorf("companion.channel must be auto, discord or terminal, got %q", c.Channel)
	}
	if _, _, err := ParseQuietHours(c.QuietHours); err != nil {
		return fmt.Errorf("companion.%w", err)
	}
	if _, err := c.Location(); err != nil {
		return fmt.Errorf("companion.timezone %q: %w", c.Timezone, err)
	}
	if c.HabitDays < 1 || c.HabitDays > 30 {
		return fmt.Errorf("companion.habit_days must be 1..30, got %d", c.HabitDays)
	}
	hb, err := time.ParseDuration(c.Heartbeat)
	if err != nil {
		return fmt.Errorf("companion.heartbeat %q: %w", c.Heartbeat, err)
	}
	if hb < 5*time.Minute {
		return fmt.Errorf("companion.heartbeat must be at least 5m, got %s", c.Heartbeat)
	}
	if c.SelfMaxBytes < 1024 || c.SelfMaxBytes > 65536 {
		return fmt.Errorf("companion.self_max_bytes must be 1024..65536, got %d", c.SelfMaxBytes)
	}
	if c.StartBudget < 1 || c.AlertBudget < 1 {
		return fmt.Errorf("companion.start_budget and alert_budget must be at least 1")
	}
	return nil
}
```

After `func (c *Config) SoulPath() string { ... }`:

```go
// SelfPath is spore's own journal. Unlike soul.md it is spore's to write,
// through self_note; the user may still edit it by hand.
func (c *Config) SelfPath() string { return filepath.Join(c.DataDir, "self.md") }
```

`time`, `strings` and `fmt` are already imported in this file; confirm with `head -20 internal/config/config.go`.

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/config/ -v 2>&1 | tail -30`
Expected: PASS, including every pre-existing config test. If a pre-existing test pins the exact default `Allow` slice or the remote deny list, update its expected value to include `self_note` and say so in the commit message.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/config/companion_policy_test.go
git commit -m "config: [companion] block, SelfPath, and self_note in the default policy

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 2: Store — `interests` and `interest_signals`

**Files:**
- Modify: `internal/store/schema.go` (append to `schemaSQL`), `internal/store/store.go:25-30` (source constants)
- Create: `internal/store/interests.go`
- Test: `internal/store/interests_test.go`

**Interfaces:**
- Consumes: `openTest(t)` from `internal/store/refine_test.go`; `timeFormat`, `nowString()`, `rowScanner` from `store.go`.
- Produces:
  - `const SourceCompanion = "companion"`
  - state constants `InterestObserving`, `InterestCandidate`, `InterestProposed`, `InterestActive`, `InterestDeclined`, `InterestRetired`
  - `type Interest struct { ID int64; Key, Label, State string; WatchJobID int64; CooldownUntil time.Time; DaysSeen int; FirstSeen, LastSeen time.Time }`
  - `func (s *Store) TouchInterest(ctx context.Context, key, label string) (Interest, error)`
  - `func (s *Store) AddInterestSignal(ctx context.Context, interestID int64, sessionID, kind, day string, at time.Time) error`
  - `func (s *Store) Interests(ctx context.Context, states ...string) ([]Interest, error)`
  - `func (s *Store) InterestByKey(ctx context.Context, key string) (Interest, bool, error)`
  - `func (s *Store) SetInterestState(ctx context.Context, id int64, from, to string) (bool, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/store/interests_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"
)

func TestInterestSignalsDeriveDaysAndDates(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	in, err := st.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)")
	if err != nil {
		t.Fatal(err)
	}
	if in.State != InterestObserving || in.DaysSeen != 0 {
		t.Fatalf("new interest = %+v", in)
	}
	t1 := time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	t3 := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC)
	for _, s := range []struct {
		day string
		at  time.Time
	}{{"2026-10-03", t1}, {"2026-10-03", t2}, {"2026-10-06", t3}} {
		if err := st.AddInterestSignal(ctx, in.ID, sid, "asked", s.day, s.at); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := st.InterestByKey(ctx, "stock:zs")
	if err != nil || !ok {
		t.Fatalf("InterestByKey: %v %v", ok, err)
	}
	if got.DaysSeen != 2 || !got.FirstSeen.Equal(t1) || !got.LastSeen.Equal(t3) {
		t.Fatalf("derived = days %d first %v last %v", got.DaysSeen, got.FirstSeen, got.LastSeen)
	}
}

func TestTouchInterestKeepsLabelAndRevivesRetired(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	a, _ := st.TouchInterest(ctx, "topic:go", "Go")
	if ok, err := st.SetInterestState(ctx, a.ID, InterestObserving, InterestRetired); err != nil || !ok {
		t.Fatalf("retire: %v %v", ok, err)
	}
	b, err := st.TouchInterest(ctx, "topic:go", "Golang")
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID || b.Label != "Go" || b.State != InterestObserving {
		t.Fatalf("touch = %+v, want same id, first label, observing", b)
	}
}

func TestTouchInterestLeavesOtherStatesAlone(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	a, _ := st.TouchInterest(ctx, "topic:go", "Go")
	_, _ = st.SetInterestState(ctx, a.ID, InterestObserving, InterestDeclined)
	b, _ := st.TouchInterest(ctx, "topic:go", "Go")
	if b.State != InterestDeclined {
		t.Fatalf("a new signal moved a declined interest to %s", b.State)
	}
}

func TestDeletingASessionRemovesItsSignals(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	keep, _ := st.CreateSessionFrom(ctx, "keep", "", SourceChat)
	drop, _ := st.CreateSessionFrom(ctx, "drop", "", SourceChat)
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	now := time.Now().UTC()
	_ = st.AddInterestSignal(ctx, in.ID, keep, "asked", "2026-10-01", now)
	_ = st.AddInterestSignal(ctx, in.ID, drop, "asked", "2026-10-02", now)
	if _, err := st.DeleteSessions(ctx, []string{drop}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.InterestByKey(ctx, "stock:zs")
	if got.DaysSeen != 1 {
		t.Fatalf("DaysSeen = %d after deleting a session, want 1", got.DaysSeen)
	}
}

func TestInterestsFiltersByState(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	a, _ := st.TouchInterest(ctx, "topic:a", "A")
	_, _ = st.TouchInterest(ctx, "topic:b", "B")
	_, _ = st.SetInterestState(ctx, a.ID, InterestObserving, InterestCandidate)
	got, err := st.Interests(ctx, InterestCandidate)
	if err != nil || len(got) != 1 || got[0].Key != "topic:a" {
		t.Fatalf("Interests(candidate) = %+v, %v", got, err)
	}
	all, _ := st.Interests(ctx)
	if len(all) != 2 {
		t.Fatalf("Interests() = %d rows, want 2", len(all))
	}
}

func TestSetInterestStateChecksFrom(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	a, _ := st.TouchInterest(ctx, "topic:a", "A")
	ok, err := st.SetInterestState(ctx, a.ID, InterestCandidate, InterestProposed)
	if err != nil || ok {
		t.Fatalf("moved from the wrong state: ok=%v err=%v", ok, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/store/ -run 'Interest|DeletingASession' -v`
Expected: build failure — `st.TouchInterest undefined`.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, add to the source constant block:

```go
	SourceCompanion = "companion"
```

Append to `schemaSQL` in `internal/store/schema.go`, before the closing backtick:

```sql

-- interests is what the companion believes the user cares about. Day counts
-- and first/last sightings are not stored: they are derived from
-- interest_signals on every read, so deleting a session corrects them.
CREATE TABLE IF NOT EXISTS interests (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  key            TEXT NOT NULL UNIQUE,
  label          TEXT NOT NULL,
  state          TEXT NOT NULL DEFAULT 'observing',
  watch_job_id   INTEGER,
  cooldown_until TEXT NOT NULL DEFAULT '',
  declined_at    TEXT NOT NULL DEFAULT '',
  created_at     TEXT NOT NULL,
  updated_at     TEXT NOT NULL
);

-- interest_signals is the evidence: one row per sighting in a session. day is
-- the local calendar date in companion.timezone, fixed when the row is
-- written, because "seen on three days" means the user's days.
CREATE TABLE IF NOT EXISTS interest_signals (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  interest_id INTEGER NOT NULL REFERENCES interests(id) ON DELETE CASCADE,
  session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  kind        TEXT NOT NULL,
  day         TEXT NOT NULL,
  seen_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_interest_signals_interest ON interest_signals(interest_id, day);
```

Create `internal/store/interests.go`:

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

// Interest states. See the companion spec, section 2.1, for the transitions.
const (
	InterestObserving = "observing"
	InterestCandidate = "candidate"
	InterestProposed  = "proposed"
	InterestActive    = "active"
	InterestDeclined  = "declined"
	InterestRetired   = "retired"
)

// Interest is one row of interests with its evidence summarised.
type Interest struct {
	ID            int64
	Key           string
	Label         string
	State         string
	WatchJobID    int64
	CooldownUntil time.Time
	// DaysSeen, FirstSeen and LastSeen are derived from interest_signals.
	// FirstSeen and LastSeen are zero when there are no signals.
	DaysSeen  int
	FirstSeen time.Time
	LastSeen  time.Time
}

const interestSelect = `
SELECT i.id, i.key, i.label, i.state, COALESCE(i.watch_job_id, 0), i.cooldown_until,
       COUNT(DISTINCT sg.day), COALESCE(MIN(sg.seen_at), ''), COALESCE(MAX(sg.seen_at), '')
  FROM interests i LEFT JOIN interest_signals sg ON sg.interest_id = i.id`

func scanInterest(r rowScanner) (Interest, error) {
	var x Interest
	var cooldown, first, last string
	if err := r.Scan(&x.ID, &x.Key, &x.Label, &x.State, &x.WatchJobID, &cooldown,
		&x.DaysSeen, &first, &last); err != nil {
		return Interest{}, err
	}
	x.CooldownUntil, _ = time.Parse(timeFormat, cooldown)
	x.FirstSeen, _ = time.Parse(timeFormat, first)
	x.LastSeen, _ = time.Parse(timeFormat, last)
	return x, nil
}

// TouchInterest returns the interest for key, creating it as observing when
// it is new. A retired interest seen again goes back to observing; every
// other state is left alone -- a declined interest stays declined however
// often it comes up. The label is the first one recorded.
func (s *Store) TouchInterest(ctx context.Context, key, label string) (Interest, error) {
	now := nowString()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO interests (key, label, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`, key, label, InterestObserving, now, now); err != nil {
		return Interest{}, fmt.Errorf("touch interest: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, updated_at = ? WHERE key = ? AND state = ?`,
		InterestObserving, now, key, InterestRetired); err != nil {
		return Interest{}, fmt.Errorf("touch interest: %w", err)
	}
	in, ok, err := s.InterestByKey(ctx, key)
	if err != nil {
		return Interest{}, err
	}
	if !ok {
		return Interest{}, fmt.Errorf("touch interest: %s vanished", key)
	}
	return in, nil
}

// AddInterestSignal records one sighting. day is the local date
// ("2006-01-02") the caller computed in the companion's timezone.
func (s *Store) AddInterestSignal(ctx context.Context, interestID int64, sessionID, kind, day string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO interest_signals (interest_id, session_id, kind, day, seen_at) VALUES (?, ?, ?, ?, ?)`,
		interestID, sessionID, kind, day, at.UTC().Format(timeFormat))
	if err != nil {
		return fmt.Errorf("add interest signal: %w", err)
	}
	return nil
}

// Interests lists interests ordered by key, limited to the given states
// when any are given.
func (s *Store) Interests(ctx context.Context, states ...string) ([]Interest, error) {
	q := interestSelect
	var args []any
	if len(states) > 0 {
		q += ` WHERE i.state IN (?` + strings.Repeat(`, ?`, len(states)-1) + `)`
		for _, st := range states {
			args = append(args, st)
		}
	}
	q += ` GROUP BY i.id ORDER BY i.key`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("read interests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Interest
	for rows.Next() {
		x, err := scanInterest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) InterestByKey(ctx context.Context, key string) (Interest, bool, error) {
	x, err := scanInterest(s.db.QueryRowContext(ctx, interestSelect+` WHERE i.key = ? GROUP BY i.id`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return Interest{}, false, nil
	}
	if err != nil {
		return Interest{}, false, err
	}
	return x, true, nil
}

// SetInterestState moves an interest from one state to another and reports
// whether it did; the from check makes a racing second move a no-op.
func (s *Store) SetInterestState(ctx context.Context, id int64, from, to string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE interests SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, nowString(), id, from)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
```

Note: `MIN`/`MAX` over `seen_at` are correct because every value is written as UTC in the fixed-width `timeFormat`, so string order is time order.

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/store/ -v 2>&1 | tail -20`
Expected: PASS for the new tests and every existing store test.

- [ ] **Step 5: Commit**

```bash
git add internal/store/schema.go internal/store/store.go internal/store/interests.go internal/store/interests_test.go
git commit -m "store: interests and interest_signals, with day counts derived from the evidence

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 3: `internal/companion` — signals, trust, day counting, sweep

**Files:**
- Create: `internal/companion/signal.go`, `internal/companion/recorder.go`
- Test: `internal/companion/recorder_test.go`

**Interfaces:**
- Consumes: Task 1 `config.CompanionConfig.Location()`, `cfg.Companion.HabitDays`, `cfg.Companion.Enabled`; Task 2 store API.
- Produces:
  - `type Signal struct { Key string \`json:"key"\`; Label string \`json:"label"\`; Kind string \`json:"kind"\` }`
  - `const MaxSignalsPerRound = 10`; `const FadeAfter = 30 * 24 * time.Hour`
  - `func Normalize(s Signal) (Signal, error)`
  - `func Trusted(sess store.Session) bool`
  - `type Recorder struct { Store *store.Store; Cfg *config.Config; Now func() time.Time }`
  - `func NewRecorder(st *store.Store, cfg *config.Config) *Recorder`
  - `func (r *Recorder) Enabled() bool`
  - `func (r *Recorder) Known(ctx context.Context) ([]store.Interest, error)`
  - `type RecordResult struct { Recorded int; Dropped []string }`
  - `func (r *Recorder) Record(ctx context.Context, sess store.Session, signals []Signal) (RecordResult, error)`
  - `func (r *Recorder) Sweep(ctx context.Context, now time.Time) error`

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/recorder_test.go`:

```go
package companion

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type rfix struct {
	r   *Recorder
	st  *store.Store
	now time.Time
}

func newRFix(t *testing.T) *rfix {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Companion.Enabled = true
	cfg.Companion.Timezone = "America/Los_Angeles"
	f := &rfix{st: st}
	f.r = NewRecorder(st, cfg)
	f.r.Now = func() time.Time { return f.now }
	return f
}

func (f *rfix) session(t *testing.T, source string) store.Session {
	t.Helper()
	ctx := context.Background()
	id, err := f.st.CreateSessionFrom(ctx, "t", "", source)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err := f.st.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func pacific(t *testing.T, s string) time.Time {
	t.Helper()
	loc, _ := time.LoadLocation("America/Los_Angeles")
	v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var zs = Signal{Key: "Stock:ZS ", Label: "Zscaler  (ZS) share price", Kind: "asked"}

func TestNormalize(t *testing.T) {
	got, err := Normalize(zs)
	if err != nil || got.Key != "stock:zs" || got.Label != "Zscaler (ZS) share price" {
		t.Fatalf("Normalize = %+v, %v", got, err)
	}
	for _, bad := range []Signal{
		{Key: "zs", Label: "x", Kind: "asked"},
		{Key: "weather:fremont", Label: "x", Kind: "asked"},
		{Key: "stock:", Label: "x", Kind: "asked"},
		{Key: "stock:z s", Label: "x", Kind: "asked"},
		{Key: "stock:" + strings.Repeat("a", 49), Label: "x", Kind: "asked"},
		{Key: "stock:zs", Label: "", Kind: "asked"},
		{Key: "stock:zs", Label: strings.Repeat("x", 81), Kind: "asked"},
		{Key: "stock:zs", Label: "x", Kind: "loved"},
	} {
		if _, err := Normalize(bad); err == nil {
			t.Errorf("Normalize accepted %+v", bad)
		}
	}
}

func TestRecordPromotesAfterHabitDays(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	sess := f.session(t, store.SourceChat)
	for i, day := range []string{"2026-10-03 09:00", "2026-10-03 17:00", "2026-10-06 09:00", "2026-10-08 09:00"} {
		f.now = pacific(t, day)
		res, err := f.r.Record(ctx, sess, []Signal{zs})
		if err != nil || res.Recorded != 1 {
			t.Fatalf("Record %d = %+v, %v", i, res, err)
		}
		in, _, _ := f.st.InterestByKey(ctx, "stock:zs")
		want := store.InterestObserving
		if i == 3 {
			want = store.InterestCandidate
		}
		if in.State != want {
			t.Fatalf("after signal %d (%s): state %s days %d, want %s", i, day, in.State, in.DaysSeen, want)
		}
	}
}

func TestRecordCountsLocalDaysNotUTCDays(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	sess := f.session(t, store.SourceChat)
	// 23:30 and 00:30 Pacific on consecutive local days are both on
	// 2026-10-04 in UTC (06:30 and 07:30). They are two of the user's days.
	for _, at := range []string{"2026-10-03 23:30", "2026-10-04 00:30"} {
		f.now = pacific(t, at)
		if _, err := f.r.Record(ctx, sess, []Signal{zs}); err != nil {
			t.Fatal(err)
		}
	}
	in, _, _ := f.st.InterestByKey(ctx, "stock:zs")
	if in.DaysSeen != 2 {
		t.Fatalf("DaysSeen = %d, want 2 local days", in.DaysSeen)
	}
}

func TestRecordRefusesUntrustedSources(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	f.now = pacific(t, "2026-10-03 09:00")
	for _, src := range []string{store.SourceJob, store.SourceSubagent, store.SourceCompanion, store.SourceUnknown} {
		res, err := f.r.Record(ctx, f.session(t, src), []Signal{zs})
		if err != nil {
			t.Fatal(err)
		}
		if res.Recorded != 0 || len(res.Dropped) != 1 {
			t.Errorf("source %s: %+v, want nothing recorded and one drop", src, res)
		}
	}
	if all, _ := f.st.Interests(ctx); len(all) != 0 {
		t.Fatalf("untrusted sources created interests: %+v", all)
	}
	if res, _ := f.r.Record(ctx, f.session(t, store.SourceDiscord), []Signal{zs}); res.Recorded != 1 {
		t.Fatalf("discord signal not recorded: %+v", res)
	}
}

func TestRecordRefusesAChildSession(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	f.now = pacific(t, "2026-10-03 09:00")
	sess := f.session(t, store.SourceChat)
	sess.ParentID = "parent"
	if res, _ := f.r.Record(ctx, sess, []Signal{zs}); res.Recorded != 0 {
		t.Fatalf("a child session's signal was recorded: %+v", res)
	}
}

func TestRecordDisabledIsANoOp(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	f.r.Cfg.Companion.Enabled = false
	f.now = pacific(t, "2026-10-03 09:00")
	res, err := f.r.Record(ctx, f.session(t, store.SourceChat), []Signal{zs})
	if err != nil || res.Recorded != 0 || len(res.Dropped) != 0 {
		t.Fatalf("disabled Record = %+v, %v", res, err)
	}
}

func TestRecordCapsAndMerges(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	f.now = pacific(t, "2026-10-03 09:00")
	var sigs []Signal
	sigs = append(sigs, zs, zs) // a duplicate key is merged, not dropped
	for i := 0; i < 11; i++ {
		sigs = append(sigs, Signal{Key: "topic:t" + string(rune('a'+i)), Label: "T", Kind: "mentioned"})
	}
	sigs = append(sigs, Signal{Key: "nonsense", Label: "x", Kind: "asked"})
	res, err := f.r.Record(ctx, f.session(t, store.SourceChat), sigs)
	if err != nil {
		t.Fatal(err)
	}
	if res.Recorded != MaxSignalsPerRound {
		t.Fatalf("Recorded = %d, want %d", res.Recorded, MaxSignalsPerRound)
	}
	// 2 over the cap, 1 invalid.
	if len(res.Dropped) != 3 {
		t.Fatalf("Dropped = %q, want 3", res.Dropped)
	}
}

func TestSweepDemotesRetiresAndHonoursCooldown(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	sess := f.session(t, store.SourceChat)
	for _, d := range []string{"2026-10-01 09:00", "2026-10-02 09:00", "2026-10-03 09:00"} {
		f.now = pacific(t, d)
		_, _ = f.r.Record(ctx, sess, []Signal{zs})
	}
	in, _, _ := f.st.InterestByKey(ctx, "stock:zs")
	if in.State != store.InterestCandidate {
		t.Fatalf("state = %s, want candidate", in.State)
	}
	// Raise the bar: the candidate no longer qualifies and goes back.
	f.r.Cfg.Companion.HabitDays = 5
	if err := f.r.Sweep(ctx, pacific(t, "2026-10-03 10:00")); err != nil {
		t.Fatal(err)
	}
	in, _, _ = f.st.InterestByKey(ctx, "stock:zs")
	if in.State != store.InterestObserving {
		t.Fatalf("state = %s, want observing after the bar rose", in.State)
	}
	// 31 days without a signal retires it.
	if err := f.r.Sweep(ctx, pacific(t, "2026-11-04 10:00")); err != nil {
		t.Fatal(err)
	}
	in, _, _ = f.st.InterestByKey(ctx, "stock:zs")
	if in.State != store.InterestRetired {
		t.Fatalf("state = %s, want retired after 31 quiet days", in.State)
	}
}

func TestKnownOmitsRetired(t *testing.T) {
	ctx := context.Background()
	f := newRFix(t)
	a, _ := f.st.TouchInterest(ctx, "topic:a", "A")
	_, _ = f.st.TouchInterest(ctx, "topic:b", "B")
	_, _ = f.st.SetInterestState(ctx, a.ID, store.InterestObserving, store.InterestRetired)
	got, err := f.r.Known(ctx)
	if err != nil || len(got) != 1 || got[0].Key != "topic:b" {
		t.Fatalf("Known = %+v, %v", got, err)
	}
}
```

The cooldown branch of `Sweep` (`now.Before(in.CooldownUntil)` keeps an interest observing) has no writer until PR 2 sets `cooldown_until`; PR 2's plan adds its test.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/companion/ -v`
Expected: build failure — package has no non-test files / `NewRecorder undefined`.

- [ ] **Step 3: Implement**

Create `internal/companion/signal.go`:

```go
// Package companion is spore noticing what the user keeps coming back to,
// and keeping its own notes. This file is the signal vocabulary: what a
// refinement round may report, validated here because the planner is a
// model and nothing it says is trusted until it is checked.
package companion

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/codered/spore/internal/store"
)

// Signal is one sighting of a recurring interest, as the planner reports it.
type Signal struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Kind  string `json:"kind"`
}

// MaxSignalsPerRound bounds what one round can add.
const MaxSignalsPerRound = 10

var (
	keyRE = regexp.MustCompile(`^(stock|crypto|topic|person|place|project):[a-z0-9._-]{1,48}$`)
	// Kinds is the closed set a signal may declare.
	Kinds = []string{"asked", "mentioned", "acted"}
)

// Normalize lowercases and trims the key, collapses whitespace in the label,
// and rejects anything outside the vocabulary.
func Normalize(s Signal) (Signal, error) {
	s.Key = strings.ToLower(strings.TrimSpace(s.Key))
	s.Label = strings.Join(strings.Fields(s.Label), " ")
	s.Kind = strings.TrimSpace(s.Kind)
	if !keyRE.MatchString(s.Key) {
		return Signal{}, fmt.Errorf("signal key %q must look like stock:zs or topic:daughter-reading", s.Key)
	}
	if n := len([]rune(s.Label)); n < 1 || n > 80 {
		return Signal{}, fmt.Errorf("signal %s: label must be 1..80 characters, got %d", s.Key, n)
	}
	if !slices.Contains(Kinds, s.Kind) {
		return Signal{}, fmt.Errorf("signal %s: kind %q must be one of %s", s.Key, s.Kind, strings.Join(Kinds, ", "))
	}
	return s, nil
}

// Trusted reports whether a session's signals count. A signal only raises a
// count -- every action still needs the user's yes -- so Discord counts as
// well as chat. Spore's own sessions (jobs, sub-agents, the companion) never
// do: a ZS watch must not count as the user checking ZS.
func Trusted(sess store.Session) bool {
	if sess.ParentID != "" {
		return false
	}
	return sess.Source == store.SourceChat || sess.Source == store.SourceDiscord
}
```

Create `internal/companion/recorder.go`:

```go
package companion

import (
	"context"
	"fmt"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// FadeAfter is how long an observing or candidate interest may go unseen
// before it is retired.
const FadeAfter = 30 * 24 * time.Hour

// Recorder turns validated signals into interest rows and keeps interest
// states in step with the evidence.
type Recorder struct {
	Store *store.Store
	Cfg   *config.Config
	// Now is the clock; tests replace it.
	Now func() time.Time
}

func NewRecorder(st *store.Store, cfg *config.Config) *Recorder {
	return &Recorder{Store: st, Cfg: cfg, Now: time.Now}
}

// Enabled reports whether the companion is on. Off, Record does nothing and
// the refiner does not ask for signals.
func (r *Recorder) Enabled() bool { return r.Cfg.Companion.Enabled }

// Known is every interest not retired, so the planner reuses existing keys
// instead of inventing stock:zscaler next to stock:zs.
func (r *Recorder) Known(ctx context.Context) ([]store.Interest, error) {
	return r.Store.Interests(ctx, store.InterestObserving, store.InterestCandidate,
		store.InterestProposed, store.InterestActive, store.InterestDeclined)
}

// RecordResult is what Record did. Dropped explains each signal it refused.
type RecordResult struct {
	Recorded int
	Dropped  []string
}

// Record validates and stores one round's signals for a session, then sweeps
// interest states. Validation failures are reported in Dropped, never as an
// error: a bad signal must not cost the round its edits.
func (r *Recorder) Record(ctx context.Context, sess store.Session, signals []Signal) (RecordResult, error) {
	var res RecordResult
	if !r.Enabled() || len(signals) == 0 {
		return res, nil
	}
	if !Trusted(sess) {
		res.Dropped = append(res.Dropped, fmt.Sprintf("signals: a %s session's signals do not count", sess.Source))
		return res, nil
	}
	loc, err := r.Cfg.Companion.Location()
	if err != nil {
		return res, err
	}
	now := r.Now()
	day := now.In(loc).Format("2006-01-02")
	seen := map[string]bool{}
	for _, raw := range signals {
		s, err := Normalize(raw)
		if err != nil {
			res.Dropped = append(res.Dropped, err.Error())
			continue
		}
		if seen[s.Key] {
			continue // one round, one sighting per key
		}
		if res.Recorded >= MaxSignalsPerRound {
			res.Dropped = append(res.Dropped, fmt.Sprintf("signal %s: over the %d-signal cap", s.Key, MaxSignalsPerRound))
			continue
		}
		seen[s.Key] = true
		in, err := r.Store.TouchInterest(ctx, s.Key, s.Label)
		if err != nil {
			return res, err
		}
		if err := r.Store.AddInterestSignal(ctx, in.ID, sess.ID, s.Kind, day, now); err != nil {
			return res, err
		}
		res.Recorded++
	}
	return res, r.Sweep(ctx, now)
}

// Sweep moves observing and candidate interests to match their evidence:
// retired after FadeAfter without a signal, candidate once seen on
// habit_days distinct days (unless cooling down), and back to observing if
// a candidate's evidence shrank below the bar.
func (r *Recorder) Sweep(ctx context.Context, now time.Time) error {
	rows, err := r.Store.Interests(ctx, store.InterestObserving, store.InterestCandidate)
	if err != nil {
		return err
	}
	habit := r.Cfg.Companion.HabitDays
	for _, in := range rows {
		to := in.State
		switch {
		case !in.LastSeen.IsZero() && now.Sub(in.LastSeen) > FadeAfter:
			to = store.InterestRetired
		case in.State == store.InterestObserving && in.DaysSeen >= habit && !now.Before(in.CooldownUntil):
			to = store.InterestCandidate
		case in.State == store.InterestCandidate && in.DaysSeen < habit:
			to = store.InterestObserving
		}
		if to == in.State {
			continue
		}
		if _, err := r.Store.SetInterestState(ctx, in.ID, in.State, to); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/companion/ -v`
Expected: PASS (9 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/companion/signal.go internal/companion/recorder.go internal/companion/recorder_test.go
git commit -m "companion: validate signals, count the user's local days, sweep interest states

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 4: Refinement rounds report signals

**Files:**
- Modify: `internal/refine/refine.go` (Refiner struct), `internal/refine/plan.go`, `internal/refine/round.go`
- Test: `internal/refine/signals_test.go` (create)

**Interfaces:**
- Consumes: Task 3 `companion.Signal`, `companion.Recorder` (`Enabled`, `Known`, `Record`), `companion.Trusted`.
- Produces:
  - `Refiner.Signals *companion.Recorder` (nil = no signals, as in every existing test)
  - `Input.AskSignals bool`, `Input.Interests []store.Interest`
  - `Plan.Signals []companion.Signal`, `Plan.BadSignals int`
  - `Result.Signals int`
  - `func ParseReply(text string) (edits []Edit, signals []companion.Signal, bad int, err error)`; `ParseEdits` keeps its signature and delegates.

- [ ] **Step 1: Write the failing tests**

Create `internal/refine/signals_test.go`:

```go
package refine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/store"
)

// withSignals attaches a Recorder to the fixture, companion on.
func withSignals(f *fix) *companion.Recorder {
	f.cfg.Companion.Enabled = true
	rec := companion.NewRecorder(f.st, f.cfg)
	rec.Now = func() time.Time { return time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC) }
	f.r.Signals = rec
	return rec
}

func TestParseReplyReadsSignalsAndCountsBadOnes(t *testing.T) {
	edits, sigs, bad, err := ParseReply(`{"edits":[],"signals":[{"key":"stock:zs","label":"ZS","kind":"asked"}, 42, {"key":"x"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(edits) != 0 || len(sigs) != 2 || sigs[0].Key != "stock:zs" || bad != 1 {
		t.Fatalf("edits=%v sigs=%+v bad=%d", edits, sigs, bad)
	}
	// {"key":"x"} decodes as a Signal; Normalize rejects it later.
}

func TestParseReplyWithoutSignalsIsFine(t *testing.T) {
	_, sigs, bad, err := ParseReply(`{"edits":[]}`)
	if err != nil || sigs != nil || bad != 0 {
		t.Fatalf("sigs=%v bad=%d err=%v", sigs, bad, err)
	}
}

func TestRoundRecordsSignals(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[],"signals":[{"key":"stock:zs","label":"Zscaler (ZS)","kind":"asked"}]}`)
	withSignals(f)
	f.say(t, "user", text("what's ZS at?"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Signals != 1 || !strings.Contains(res.Note, "1 interest signal") {
		t.Fatalf("res = %+v", res)
	}
	in, ok, _ := f.st.InterestByKey(context.Background(), "stock:zs")
	if !ok || in.DaysSeen != 1 {
		t.Fatalf("interest = %+v ok=%v", in, ok)
	}
	sys := f.script.Requests()[0].System[0].Text
	if !strings.Contains(sys, `"signals"`) {
		t.Fatal("the planner was not asked for signals")
	}
}

func TestRoundKeepsEditsWhenSignalsAreMalformed(t *testing.T) {
	f := newFix(t, store.SourceChat,
		`{"edits":[{"kind":"fact.create","name":"likes-tea","type":"user","description":"tea","body":"Likes tea.","rationale":"user said so"}],
		  "signals":[7, {"key":"weather:x","label":"x","kind":"asked"}, {"key":"topic:tea","label":"Tea","kind":"loved"}]}`)
	withSignals(f)
	f.say(t, "user", text("I like tea"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatalf("malformed signals failed the round: %v", err)
	}
	if len(res.Applied) != 1 || res.Signals != 0 || len(res.Dropped) != 3 {
		t.Fatalf("applied=%d signals=%d dropped=%q", len(res.Applied), res.Signals, res.Dropped)
	}
}

func TestRoundShowsKnownInterestsToThePlanner(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	withSignals(f)
	_, _ = f.st.TouchInterest(context.Background(), "stock:zs", "Zscaler (ZS)")
	f.say(t, "user", text("hi"))
	if _, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0); err != nil {
		t.Fatal(err)
	}
	user := f.script.Requests()[0].Messages[0].Blocks[0].Text
	if !strings.Contains(user, "stock:zs: Zscaler (ZS)") {
		t.Fatalf("known interests missing from planner input:\n%s", user)
	}
}

func TestRoundOnJobSessionDoesNotAskForSignals(t *testing.T) {
	f := newFix(t, store.SourceJob, `{"edits":[],"signals":[{"key":"stock:zs","label":"ZS","kind":"asked"}]}`)
	withSignals(f)
	f.say(t, "user", text("check ZS"))
	res, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.script.Requests()[0].System[0].Text, `"signals"`) {
		t.Error("a job session's planner was asked for signals")
	}
	if res.Signals != 0 {
		t.Errorf("a job session recorded %d signals", res.Signals)
	}
	if all, _ := f.st.Interests(context.Background()); len(all) != 0 {
		t.Errorf("a job session created interests: %+v", all)
	}
}

func TestRoundWithCompanionOffDoesNotAskForSignals(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	rec := withSignals(f)
	rec.Cfg.Companion.Enabled = false
	f.say(t, "user", text("hi"))
	if _, err := f.r.Round(context.Background(), f.sid, TriggerManual, "", 0); err != nil {
		t.Fatal(err)
	}
	req := f.script.Requests()[0]
	if strings.Contains(req.System[0].Text, `"signals"`) || strings.Contains(req.Messages[0].Blocks[0].Text, "Interests already") {
		t.Fatal("companion off, but the planner prompt changed")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/refine/ -run 'Signal|ParseReply|KnownInterests|JobSession|CompanionOff' -v`
Expected: build failure — `f.r.Signals undefined`, `ParseReply undefined`.

- [ ] **Step 3: Implement**

`internal/refine/refine.go`: add the import `"github.com/codered/spore/internal/companion"` and, in the `Refiner` struct after `RevokePolicy`:

```go
	// Signals records the recurring interests a round reports. Nil, or a
	// recorder whose companion is off, means the planner is not asked for
	// any. Set before any round runs; never changed after.
	Signals *companion.Recorder
```

`internal/refine/plan.go`:

1. Add import `"github.com/codered/spore/internal/companion"` and `"github.com/codered/spore/internal/store"`.
2. Extend `Input`:

```go
	// AskSignals adds the signals section to the prompt; Interests are the
	// keys already tracked, shown so the planner reuses them.
	AskSignals bool
	Interests  []store.Interest
```

3. Extend `Plan`:

```go
	Signals    []companion.Signal
	BadSignals int // entries in "signals" that were not even objects
```

4. Change `plannerPrompt(maxEdits int)` to `plannerPrompt(maxEdits int, signals bool)`. Keep the existing text exactly, and when `signals` is true append this (a separate `if` that concatenates onto the returned string):

```go
const signalsPrompt = `

Also report the user's recurring interests this conversation shows, as "signals" beside "edits":
{"edits": [ ... ], "signals": [ ... ]}

A signal is something the user keeps coming back to -- a stock or coin they check, a topic, a person, a place, a project -- not a one-off fact. Record one per interest the user themselves raised in this conversation. What spore brought up on its own does not count.

Signal shape:
{"key":"stock:zs","label":"Zscaler (ZS) share price","kind":"asked|mentioned|acted"}

- key is kind:name, kind one of stock, crypto, topic, person, place, project; name lowercase letters, digits, dot, underscore or hyphen, at most 48.
- Reuse a key from "Interests already being tracked" when it is the same interest.
- asked: the user asked about it; mentioned: they brought it up; acted: they did something about it.
- At most 10. An empty list is a good answer.`
```

Implementation shape:

```go
func plannerPrompt(maxEdits int, signals bool) string {
	p := fmt.Sprintf(`...existing text unchanged...`, maxEdits)
	if signals {
		p += signalsPrompt
	}
	return p
}
```

5. In `renderInput`, after the project-notes block and before the `Instructions` block:

```go
	if in.AskSignals {
		b.WriteString("\n## Interests already being tracked\n\n")
		if len(in.Interests) == 0 {
			b.WriteString("(none)\n")
		}
		for _, it := range in.Interests {
			fmt.Fprintf(&b, "- %s: %s\n", it.Key, it.Label)
		}
	}
```

6. Replace `ParseEdits` with `ParseReply` plus a thin `ParseEdits`:

```go
// ParseReply reads the planner's reply. It tolerates prose or a code fence
// around the object, but a reply with no complete object is an error: a
// truncated answer must fail the round, not read as "nothing to change".
// Signals are decoded one by one so a malformed entry costs only itself;
// bad counts the entries that were not signal objects at all.
func ParseReply(text string) (edits []Edit, signals []companion.Signal, bad int, err error) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return nil, nil, 0, fmt.Errorf("refiner reply has no JSON object (truncated or off-format): %.200q", text)
	}
	var out struct {
		Edits   *[]Edit           `json:"edits"`
		Signals []json.RawMessage `json:"signals"`
	}
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return nil, nil, 0, fmt.Errorf("refiner reply is not valid JSON (truncated or off-format): %w", err)
	}
	if out.Edits == nil {
		return nil, nil, 0, fmt.Errorf("refiner reply has no \"edits\" field: %.200q", text)
	}
	for _, raw := range out.Signals {
		var s companion.Signal
		if err := json.Unmarshal(raw, &s); err != nil {
			bad++
			continue
		}
		signals = append(signals, s)
	}
	return *out.Edits, signals, bad, nil
}

// ParseEdits is ParseReply without the signals.
func ParseEdits(text string) ([]Edit, error) {
	edits, _, _, err := ParseReply(text)
	return edits, err
}
```

7. In `(r *Refiner) plan`: change `plannerPrompt(r.Cfg.Refine.MaxEdits)` to `plannerPrompt(r.Cfg.Refine.MaxEdits, in.AskSignals)`; replace `edits, err := ParseEdits(text)` with `edits, signals, bad, err := ParseReply(text)`; and return `Plan{Edits: edits, Signals: signals, BadSignals: bad, Model: ref, Usage: usage, Cost: cost}`.

`internal/refine/round.go`:

1. Imports: add `"log/slog"` and `"github.com/codered/spore/internal/companion"`.
2. Add `Signals int` to `Result` after `Dropped`.
3. Replace the `p, err := r.plan(ctx, Input{...})` call with:

```go
	in := Input{
		Transcript: transcript, Facts: snap.facts, Notes: snap.notes,
		NotesPath: snap.notesPath, Instructions: instructions,
	}
	if r.Signals != nil && r.Signals.Enabled() && companion.Trusted(sess) {
		known, err := r.Signals.Known(ctx)
		if err != nil {
			return Result{}, err
		}
		in.AskSignals, in.Interests = true, known
	}
	p, err := r.plan(ctx, in)
```

4. After `r.apply(ctx, sess, trig, snap, p.Edits, &res)` and before `SetRefinedThrough`:

```go
	r.recordSignals(ctx, sess, p, &res)
```

5. Add:

```go
// recordSignals hands the planner's signals to the companion. Failures are
// logged and dropped: the edits already applied, and a lost sighting costs
// one count, not the round.
func (r *Refiner) recordSignals(ctx context.Context, sess store.Session, p Plan, res *Result) {
	for i := 0; i < p.BadSignals; i++ {
		res.Dropped = append(res.Dropped, "signal: not an object")
	}
	if r.Signals == nil || len(p.Signals) == 0 {
		return
	}
	rec, err := r.Signals.Record(ctx, sess, p.Signals)
	if err != nil {
		slog.Warn("refinement could not record interest signals", "session", sess.ID, "error", err)
	}
	res.Signals = rec.Recorded
	res.Dropped = append(res.Dropped, rec.Dropped...)
}
```

6. In `summary()`, after the `Applied` part:

```go
	if n := res.Signals; n > 0 {
		word := "signals"
		if n == 1 {
			word = "signal"
		}
		parts = append(parts, fmt.Sprintf("%d interest %s", n, word))
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/refine/ -v 2>&1 | tail -40`
Expected: PASS for the new tests and every existing refine test (`TestPlanUsesTheRefinementSiteAndShowsFactsAndNotes` still sees "at most 5").

- [ ] **Step 5: Commit**

```bash
git add internal/refine/refine.go internal/refine/plan.go internal/refine/round.go internal/refine/signals_test.go
git commit -m "refine: ask the planner for interest signals and record them through the companion

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 5: `self.md` — text helpers and a ledgered writer

**Files:**
- Create: `internal/companion/selfmd.go`, `internal/refine/self.go`
- Modify: `internal/store/refine.go` (kind constant), `internal/refine/refine.go` (kind + trigger constants), `internal/refine/apply.go` (`pathFor`)
- Test: `internal/companion/selfmd_test.go`, `internal/refine/self_test.go`

**Interfaces:**
- Consumes: Task 1 `cfg.SelfPath()`, `cfg.Companion.SelfMaxBytes`; refine's `readTarget`, `writeTarget`, `sameContent`, `newRoundID`, `fileMu`, `write`.
- Produces:
  - `companion.SelfHeadings []string` (the four bare names, no `## `)
  - `func companion.SelfTemplate() string`
  - `func companion.AppendSelfNote(body, heading, text string) (string, error)`
  - `store.KindSelfUpdate = "self.update"`; `refine.KindSelfUpdate = store.KindSelfUpdate`; `refine.TriggerTool Trigger = "tool"`
  - `var refine.ErrSelfTooLarge`
  - `func (r *Refiner) UpdateSelf(ctx context.Context, sessionID string, trig Trigger, rationale string, edit func(cur string) (string, error)) (store.Refinement, error)`

- [ ] **Step 1: Write the failing tests**

Create `internal/companion/selfmd_test.go`:

```go
package companion

import (
	"strings"
	"testing"
)

func TestAppendSelfNoteStartsFromTheTemplate(t *testing.T) {
	got, err := AppendSelfNote("", "Threads with you", "deciding whether to sell ZS before earnings")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range SelfHeadings {
		if !strings.Contains(got, "## "+h+"\n") {
			t.Errorf("missing heading %q in:\n%s", h, got)
		}
	}
	threads := strings.Index(got, "## Threads with you")
	note := strings.Index(got, "- deciding whether to sell ZS before earnings\n")
	next := strings.Index(got, "## How you like to be talked to")
	if !(threads < note && note < next) {
		t.Fatalf("note not under its heading:\n%s", got)
	}
}

func TestAppendSelfNoteAppendsAtTheEndOfItsSection(t *testing.T) {
	body, _ := AppendSelfNote("", "What I'm curious about", "first")
	body, _ = AppendSelfNote(body, "What I'm curious about", "second")
	first := strings.Index(body, "- first")
	second := strings.Index(body, "- second")
	next := strings.Index(body, "## Threads with you")
	if !(first < second && second < next) {
		t.Fatalf("order wrong:\n%s", body)
	}
}

func TestAppendSelfNoteAddsAMissingHeading(t *testing.T) {
	hand := "Some notes the user wrote by hand.\n"
	got, err := AppendSelfNote(hand, "Opinions I've formed", "tabs are fine")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, hand) || !strings.HasSuffix(got, "## Opinions I've formed\n\n- tabs are fine\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestAppendSelfNoteRejectsUnknownHeadingAndEmptyText(t *testing.T) {
	if _, err := AppendSelfNote("", "Secrets", "x"); err == nil {
		t.Error("accepted an unknown heading")
	}
	if _, err := AppendSelfNote("", "Threads with you", "  \n "); err == nil {
		t.Error("accepted empty text")
	}
}

func TestAppendSelfNoteCollapsesNewlines(t *testing.T) {
	got, _ := AppendSelfNote("", "Threads with you", "line one\n## Fake heading")
	if strings.Contains(got, "\n## Fake heading") {
		t.Fatalf("a note injected a heading:\n%s", got)
	}
}
```

Create `internal/refine/self_test.go`:

```go
package refine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

func TestUpdateSelfWritesAndLedgers(t *testing.T) {
	f := newFix(t, store.SourceChat)
	row, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "noted", func(cur string) (string, error) {
		if cur != "" {
			t.Errorf("cur = %q, want empty for an absent file", cur)
		}
		return "hello\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.cfg.SelfPath())
	if string(b) != "hello\n" {
		t.Fatalf("self.md = %q", b)
	}
	if row.Kind != KindSelfUpdate || row.Status != store.RefineApplied || row.Before != nil || row.Target != f.cfg.SelfPath() {
		t.Fatalf("row = %+v", row)
	}
}

func TestUpdateSelfRefusesOverTheCap(t *testing.T) {
	f := newFix(t, store.SourceChat)
	f.cfg.Companion.SelfMaxBytes = 1024
	_, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "big", func(string) (string, error) {
		return strings.Repeat("x", 1025), nil
	})
	if !errors.Is(err, ErrSelfTooLarge) {
		t.Fatalf("err = %v, want ErrSelfTooLarge", err)
	}
	if _, err := os.Stat(f.cfg.SelfPath()); !os.IsNotExist(err) {
		t.Fatal("an oversized self.md was written")
	}
	if rows, _ := f.st.Refinements(context.Background(), "", 10); len(rows) != 0 {
		t.Fatalf("a refused write was ledgered: %+v", rows)
	}
}

func TestSelfUpdateRollsBack(t *testing.T) {
	f := newFix(t, store.SourceChat)
	if err := os.WriteFile(f.cfg.SelfPath(), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "noted", func(cur string) (string, error) {
		return cur + "after\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := f.r.Rollback(context.Background(), f.sid, "")
	if err != nil || len(out.RolledBack) != 1 {
		t.Fatalf("Rollback = %+v, %v", out, err)
	}
	b, _ := os.ReadFile(f.cfg.SelfPath())
	if string(b) != "before\n" {
		t.Fatalf("self.md after rollback = %q", b)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/companion/ ./internal/refine/ -run 'Self' -v`
Expected: build failure — `AppendSelfNote undefined`, `UpdateSelf undefined`.

- [ ] **Step 3: Implement**

Create `internal/companion/selfmd.go`:

```go
package companion

import (
	"fmt"
	"slices"
	"strings"
)

// SelfHeadings are self.md's sections, in order, without the "## ".
var SelfHeadings = []string{
	"What I'm curious about",
	"Threads with you",
	"How you like to be talked to",
	"Opinions I've formed",
}

// SelfTemplate is an empty self.md: every heading, no notes.
func SelfTemplate() string {
	var b strings.Builder
	for i, h := range SelfHeadings {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## " + h + "\n")
	}
	return b.String()
}

// AppendSelfNote adds "- text" at the end of heading's section. An empty
// body starts from the template; a body without that heading -- the user
// edited the file by hand -- gets the heading appended rather than the note
// refused, because the file is theirs too. Newlines in text are collapsed so
// a note can never forge a heading.
func AppendSelfNote(body, heading, text string) (string, error) {
	if !slices.Contains(SelfHeadings, heading) {
		return "", fmt.Errorf("heading %q must be one of: %s", heading, strings.Join(SelfHeadings, "; "))
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", fmt.Errorf("an empty note would add a bare bullet")
	}
	if strings.TrimSpace(body) == "" {
		body = SelfTemplate()
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## "+heading {
			at = i
			break
		}
	}
	if at < 0 {
		return body + "\n## " + heading + "\n\n- " + text + "\n", nil
	}
	// The section ends at the next "## " heading or the end of the file.
	end := len(lines)
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	// Insert after the section's last non-blank line, keeping the blank
	// line that separates it from the next heading.
	ins := end
	for ins > at+1 && strings.TrimSpace(lines[ins-1]) == "" {
		ins--
	}
	bullet := "- " + text
	if ins == at+1 {
		// Empty section: a blank line between heading and first bullet.
		bullet = "\n" + bullet
	}
	out := append([]string{}, lines[:ins]...)
	out = append(out, bullet)
	out = append(out, lines[ins:]...)
	return strings.Join(out, "\n") + "\n", nil
}
```

Check the first test by hand: the template is `## What I'm curious about\n\n## Threads with you\n\n## How...\n\n## Opinions I've formed\n`. Appending under "Threads with you" finds `at` = 2, `end` = 4 (the next heading), backs `ins` from 4 to 3 (line 3 is blank), and `ins == at+1` so the bullet is `"\n- deciding..."`. The result is `## Threads with you\n\n- deciding...\n\n## How...`. For the missing-heading test the output ends `"## Opinions I've formed\n\n- tabs are fine\n"`, as asserted.

In `internal/store/refine.go`, add to the `KindPolicyAllow` const block:

```go
	// KindSelfUpdate is a write to self.md: spore's own notes. Target is the
	// file's absolute path; Before and After are whole-file contents.
	KindSelfUpdate = "self.update"
```

In `internal/refine/refine.go`: add `TriggerTool Trigger = "tool"` to the trigger block (comment: `// TriggerTool marks a write a tool made directly, such as self_note.`), and add to the edit-kind block:

```go
	// KindSelfUpdate is written by UpdateSelf, never by a planner edit.
	KindSelfUpdate = store.KindSelfUpdate
```

In `internal/refine/apply.go`, `pathFor`, add before the notes check:

```go
	if row.Kind == KindSelfUpdate {
		if row.Target != r.Cfg.SelfPath() {
			return "", fmt.Errorf("refinement %d has an unexpected self.md target %q", row.ID, row.Target)
		}
		return row.Target, nil
	}
```

Create `internal/refine/self.go`:

```go
package refine

import (
	"context"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/store"
)

// ErrSelfTooLarge refuses a self.md write over companion.self_max_bytes.
var ErrSelfTooLarge = errors.New("self.md would exceed companion.self_max_bytes")

// UpdateSelf rewrites self.md under the same lock and ledger as every other
// refinement write: edit receives the current content ("" when the file is
// absent) and returns the new content, the row is written before the file,
// and the round can be rolled back like any other. The size cap is checked
// here so no writer can skip it.
func (r *Refiner) UpdateSelf(ctx context.Context, sessionID string, trig Trigger, rationale string, edit func(cur string) (string, error)) (store.Refinement, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	path := r.Cfg.SelfPath()
	before, err := readTarget(path)
	if err != nil {
		return store.Refinement{}, err
	}
	cur := ""
	if before != nil {
		cur = *before
	}
	after, err := edit(cur)
	if err != nil {
		return store.Refinement{}, err
	}
	if max := r.Cfg.Companion.SelfMaxBytes; len(after) > max {
		return store.Refinement{}, fmt.Errorf("%w: %d bytes, cap %d", ErrSelfTooLarge, len(after), max)
	}
	row := store.Refinement{
		RoundID: newRoundID(), SessionID: sessionID, Trigger: string(trig), Kind: KindSelfUpdate,
		Target: path, Before: before, After: &after, Rationale: rationale, Status: store.RefineApplied,
	}
	if row.ID, err = r.Store.AddRefinement(ctx, row); err != nil { // the row first: no write goes unrecorded
		return store.Refinement{}, err
	}
	if err := r.write(ctx, row.Kind, row.Target, path, row.After); err != nil {
		_, _ = r.Store.SetRefinementStatus(ctx, row.ID, store.RefineApplied, store.RefineFailed)
		row.Status = store.RefineFailed
		return row, err
	}
	return row, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/companion/ ./internal/refine/ ./internal/store/ -v 2>&1 | tail -40`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/companion/selfmd.go internal/companion/selfmd_test.go internal/refine/self.go internal/refine/self_test.go internal/refine/refine.go internal/refine/apply.go internal/store/refine.go
git commit -m "refine: ledgered self.md writer with a size cap; companion: self.md note helpers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 6: the `self_note` tool

**Files:**
- Modify: `internal/tool/persona/persona.go`, `internal/policy/guard.go:346-351` (`nonLearnable`), `cmd/spore/wire.go:61`
- Test: `internal/tool/persona/persona_test.go`, `internal/policy/guard_test.go`

**Interfaces:**
- Consumes: Task 5 `(*refine.Refiner).UpdateSelf`, `refine.TriggerTool`, `refine.ErrSelfTooLarge`, `companion.AppendSelfNote`, `companion.SelfHeadings`; `policy.SessionFrom`.
- Produces:
  - `type persona.SelfUpdater interface { UpdateSelf(ctx context.Context, sessionID string, trig refine.Trigger, rationale string, edit func(string) (string, error)) (store.Refinement, error) }`
  - `func persona.New(cfg *config.Config, self SelfUpdater) []tool.Tool` — returns `agent_note`, plus `self_note` when `cfg.Companion.Enabled`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tool/persona/persona_test.go` (add imports `errors`, `github.com/codered/spore/internal/refine`, `github.com/codered/spore/internal/store`):

```go
type fakeSelf struct {
	body    string
	session string
	err     error
}

func (f *fakeSelf) UpdateSelf(_ context.Context, sessionID string, _ refine.Trigger, _ string, edit func(string) (string, error)) (store.Refinement, error) {
	if f.err != nil {
		return store.Refinement{}, f.err
	}
	after, err := edit(f.body)
	if err != nil {
		return store.Refinement{}, err
	}
	f.body, f.session = after, sessionID
	return store.Refinement{}, nil
}

func names(ts []tool.Tool) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func TestSelfNoteRegisteredOnlyWhenCompanionIsOn(t *testing.T) {
	cfg := config.Default()
	if got := names(New(cfg, &fakeSelf{})); len(got) != 1 || got[0] != "agent_note" {
		t.Fatalf("companion off: tools = %v", got)
	}
	cfg.Companion.Enabled = true
	if got := names(New(cfg, &fakeSelf{})); len(got) != 2 || got[1] != "self_note" {
		t.Fatalf("companion on: tools = %v", got)
	}
}

func TestSelfNoteAppendsUnderItsHeading(t *testing.T) {
	f := &fakeSelf{}
	n := newSelfNote(config.Default(), f)
	out, err := n.Call(ctxAt(t.TempDir()), json.RawMessage(`{"heading":"Threads with you","text":"ZS earnings on the 20th"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.body, "## Threads with you\n\n- ZS earnings on the 20th\n") || f.session != "s1" {
		t.Fatalf("body = %q session = %q", f.body, f.session)
	}
	if !strings.Contains(out, "Threads with you") {
		t.Fatalf("result %q does not say where it went", out)
	}
}

func TestSelfNoteNeedsASession(t *testing.T) {
	n := newSelfNote(config.Default(), &fakeSelf{})
	if _, err := n.Call(context.Background(), json.RawMessage(`{"heading":"Threads with you","text":"x"}`)); err == nil {
		t.Fatal("self_note ran with no session on the context")
	}
}

func TestSelfNoteExplainsAFullFile(t *testing.T) {
	n := newSelfNote(config.Default(), &fakeSelf{err: refine.ErrSelfTooLarge})
	_, err := n.Call(ctxAt(t.TempDir()), json.RawMessage(`{"heading":"Threads with you","text":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "self_max_bytes") {
		t.Fatalf("err = %v, want it to name self_max_bytes", err)
	}
	if !errors.Is(err, refine.ErrSelfTooLarge) {
		t.Fatal("error does not wrap ErrSelfTooLarge")
	}
}
```

Also add `"github.com/codered/spore/internal/tool"` to that test file's imports.

Append to `internal/policy/guard_test.go`:

```go
func TestSelfNoteIsNotLearnable(t *testing.T) {
	if _, ok := PatternFor(Call{Tool: "self_note", Args: json.RawMessage(`{"heading":"Threads with you","text":"/tmp/x"}`)}, "/tmp"); ok {
		t.Fatal("self_note produced a learnable pattern")
	}
}
```

(Confirm `encoding/json` is already imported in `guard_test.go`; add it if not.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/tool/persona/ ./internal/policy/ -run 'SelfNote' -v`
Expected: build failure — `New` takes one argument, `newSelfNote undefined`. (The policy test may already pass, because the text argument is not path-shaped. Keep the test: it pins the property in `nonLearnable` instead of leaving it to that accident, as the comment there explains.)

- [ ] **Step 3: Implement**

`internal/tool/persona/persona.go`. Update the package comment's second sentence to: `soul.md has no tool: it is the user's own file. self.md is spore's own, written with self_note when the companion is on.` Add the imports `errors`, `github.com/codered/spore/internal/companion`, `github.com/codered/spore/internal/refine` and `github.com/codered/spore/internal/store`. Replace `New`:

```go
// SelfUpdater is the ledgered self.md writer; *refine.Refiner implements it.
type SelfUpdater interface {
	UpdateSelf(ctx context.Context, sessionID string, trig refine.Trigger, rationale string, edit func(string) (string, error)) (store.Refinement, error)
}

// New builds the persona tools. self_note exists only while the companion is
// on, so turning it off leaves the tool list exactly as it was.
func New(cfg *config.Config, self SelfUpdater) []tool.Tool {
	out := []tool.Tool{newAgentNote(cfg)}
	if cfg.Companion.Enabled {
		out = append(out, newSelfNote(cfg, self))
	}
	return out
}
```

Add:

```go
type selfNote struct {
	cfg  *config.Config
	self SelfUpdater
}

func newSelfNote(cfg *config.Config, self SelfUpdater) selfNote { return selfNote{cfg: cfg, self: self} }

func (selfNote) Name() string { return "self_note" }

func (selfNote) Description() string {
	return "Add one line to your own notes (self.md), which are in front of you in every conversation. " +
		"Use it for what you are curious about, an open thread with the user worth following up, " +
		"how they like to be talked to, or an opinion you have formed. Facts about the user go in memory instead."
}

func (selfNote) Schema() json.RawMessage {
	return json.RawMessage(`{
	  "type": "object",
	  "properties": {
	    "heading": {"type": "string", "enum": ["What I'm curious about", "Threads with you", "How you like to be talked to", "Opinions I've formed"]},
	    "text": {"type": "string", "description": "One line, in your own voice."}
	  },
	  "required": ["heading", "text"]
	}`)
}

// ReadOnly is false: it writes a file.
func (selfNote) ReadOnly() bool { return false }

func (s selfNote) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Heading string `json:"heading"`
		Text    string `json:"text"`
	}
	if len(args) == 0 {
		return "", fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	sid := policy.SessionFrom(ctx).ID
	if sid == "" {
		return "", fmt.Errorf("self_note needs a session to record the change against")
	}
	if len([]rune(strings.TrimSpace(in.Text))) > 300 {
		return "", fmt.Errorf("a note is one line of at most 300 characters")
	}
	_, err := s.self.UpdateSelf(ctx, sid, refine.TriggerTool, "self_note: "+in.Heading, func(cur string) (string, error) {
		return companion.AppendSelfNote(cur, in.Heading, in.Text)
	})
	if errors.Is(err, refine.ErrSelfTooLarge) {
		return "", fmt.Errorf("%w. Tell the user: they can trim %s or raise companion.self_max_bytes", err, s.cfg.SelfPath())
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("noted under %q in %s", in.Heading, s.cfg.SelfPath()), nil
}
```

`internal/policy/guard.go`, in `nonLearnable`, after the `agent_note` entry:

```go
	// self_note: spore's own notes ride in every later prompt, so each write
	// is judged on its own.
	"self_note": true,
```

`cmd/spore/wire.go:61`: change `personatool.New(cfg)` to `personatool.New(cfg, ref)`.

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/tool/persona/ ./internal/policy/ ./cmd/spore/ -v 2>&1 | tail -30`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tool/persona/persona.go internal/tool/persona/persona_test.go internal/policy/guard.go internal/policy/guard_test.go cmd/spore/wire.go
git commit -m "persona: self_note appends to spore's own notes through the ledgered writer

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 7: `self.md` in the prompt

**Files:**
- Modify: `internal/agent/context.go` (Snapshot struct ~line 34, `SnapshotBreakdown` ~line 80, `selfSection` ~line 199, new `selfNotesSection`, `Assemble` ~line 271), `internal/agent/agent.go:169-174`
- Test: `internal/agent/context_test.go`, `cmd/spore/persona_wire_test.go`

**Interfaces:**
- Consumes: Task 1 `cfg.SelfPath()`, `cfg.Companion.Enabled`; `persona.Load`.
- Produces: `Snapshot.SelfNotes string`; `func selfNotesSection(body string) string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/context_test.go` (it is `package agent`; reuse its `systemText` helper and its existing imports, adding `github.com/codered/spore/internal/config` if missing):

```go
func TestSelfNotesSitUnderTheSoulWithDemotedHeadings(t *testing.T) {
	snap := Snapshot{
		System:    "you are spore",
		Soul:      "warm and dry",
		SelfNotes: "## Threads with you\n\n- ZS earnings on the 20th\n",
		Facts:     nil,
	}
	sys := systemText(Assemble(snap, config.Default().Context).System)
	soul := strings.Index(sys, "## Who you are")
	own := strings.Index(sys, "## Your own notes")
	thread := strings.Index(sys, "### Threads with you")
	if soul < 0 || own < soul || thread < own {
		t.Fatalf("order wrong (soul %d, own %d, thread %d):\n%s", soul, own, thread, sys)
	}
	if !strings.Contains(sys, `"Who you are" wins`) {
		t.Fatal("the precedence line is missing")
	}
}

func TestNoSelfNotesNoSection(t *testing.T) {
	sys := systemText(Assemble(Snapshot{System: "you are spore"}, config.Default().Context).System)
	if strings.Contains(sys, "Your own notes") {
		t.Fatal("an empty self.md rendered a section")
	}
}

func TestSelfSectionNamesSelfMdOnlyWhenCompanionIsOn(t *testing.T) {
	cfg := config.Default()
	if strings.Contains(selfSection(cfg, ""), "self.md") {
		t.Fatal("companion off, but the file list names self.md")
	}
	cfg.Companion.Enabled = true
	if !strings.Contains(selfSection(cfg, ""), cfg.SelfPath()) {
		t.Fatal("companion on, but the file list does not name self.md")
	}
}
```

Append to `cmd/spore/persona_wire_test.go`:

```go
// self.md reaches the prompt only while the companion is on: turning it off
// must leave the prompt exactly as it was, even with the file on disk.
func TestBuildAgentWiresSelfMdOnlyWhenCompanionIsOn(t *testing.T) {
	for _, on := range []bool{false, true} {
		dir := t.TempDir()
		ws := t.TempDir()
		cfg := config.Default()
		cfg.DataDir = dir
		cfg.DefaultModel = "anthropic/claude-opus-5"
		cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}
		cfg.Companion.Enabled = on
		if err := os.WriteFile(cfg.SelfPath(), []byte("SELF-MARKER"), 0o600); err != nil {
			t.Fatal(err)
		}
		u := buildAgentAt(t, cfg)
		sid, err := u.st.CreateSession(context.Background(), "", ws)
		if err != nil {
			t.Fatal(err)
		}
		ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Workspace: ws})
		snap, err := u.a.Snapshot(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(snap.SelfNotes, "SELF-MARKER"); got != on {
			t.Errorf("companion %v: SelfNotes has the marker = %v", on, got)
		}
		hasTool := false
		for _, s := range u.a.Tools.Specs() {
			hasTool = hasTool || s.Name == "self_note"
		}
		if hasTool != on {
			t.Errorf("companion %v: self_note registered = %v", on, hasTool)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/agent/ ./cmd/spore/ -run 'SelfNotes|SelfSection|SelfMd' -v`
Expected: build failure — `unknown field SelfNotes`.

- [ ] **Step 3: Implement**

`internal/agent/context.go`:

1. In `Snapshot`, after `Agent   string`:

```go
	// SelfNotes is self.md: spore's own notes, written with self_note. Empty
	// when the companion is off or the file is absent.
	SelfNotes string
```

2. In `SnapshotBreakdown`, change the `System:` line to:

```go
		System:      EstimateTokens(snap.System) + EstimateTokens(snap.Kernel) + EstimateTokens(selfNotesSection(snap.SelfNotes)),
```

3. Add after `soulSection`:

```go
// selfNotesSection renders self.md directly under soul.md. The file's own
// "## " headings are demoted one level so they read as parts of this
// section, and the opening line settles who wins a conflict: the user's
// soul.md, always.
func selfNotesSection(body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	lines := strings.Split(strings.Trim(body, "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") {
			lines[i] = "#" + l
		}
	}
	return "\n\n## Your own notes\n\nThese are your own notes, kept across every conversation. " +
		"Where they conflict with \"Who you are\", \"Who you are\" wins.\n\n" +
		strings.Join(lines, "\n") + "\n"
}
```

4. In `Assemble`, directly after `add(soulSection(snap.Soul))`:

```go
	add(selfNotesSection(snap.SelfNotes))
```

5. In `selfSection`, directly after the soul.md line (`fmt.Fprintf(&b, "- Your personality: ...`):

```go
	if cfg.Companion.Enabled {
		fmt.Fprintf(&b, "- Your own notes: %s -- yours, not the user's: what you are curious about, open threads with them, how they like to be talked to, opinions you have formed. Add a line with self_note.\n", cfg.SelfPath())
	}
```

`internal/agent/agent.go`, in `Snapshot`, directly after the soul.md block (after `snap.Soul = body` and its closing `}`):

```go
	if a.Cfg.Companion.Enabled {
		if body, err := persona.Load(a.Cfg.SelfPath()); err != nil {
			slog.Warn("read self.md", "path", a.Cfg.SelfPath(), "err", err)
		} else {
			snap.SelfNotes = body
		}
	}
```

- [ ] **Step 4: Run the tests**

Run: `go test -tags sqlite_fts5 ./internal/agent/ ./cmd/spore/ -v 2>&1 | tail -30`
Expected: PASS, including the prompt-caching tests in `agent_test.go` (companion is off in all of them, so their prefixes are unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/agent/context.go internal/agent/agent.go internal/agent/context_test.go cmd/spore/persona_wire_test.go
git commit -m "agent: render self.md under soul.md when the companion is on

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 8: wire the recorder and add `spore companion`

**Files:**
- Modify: `cmd/spore/wire.go:208` (after `refine.New`), `cmd/spore/main.go` (usage text, `dispatch`)
- Create: `cmd/spore/companion.go`
- Test: `cmd/spore/refine_wire_test.go`, `cmd/spore/companion_test.go` (create)

**Interfaces:**
- Consumes: Task 3 `companion.NewRecorder`; Task 2 `store.Interests`; Task 1 `cfg.Companion`, `cfg.SelfPath()`.
- Produces: `func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error`; `func companionStatus(ctx context.Context, w io.Writer, cfg *config.Config, st *store.Store) error`; `func companionInterests(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/spore/refine_wire_test.go`:

```go
// Without this line the planner is never asked for signals and the whole
// observe half is inert, with every unit test still green.
func TestBuildAgentWiresTheSignalRecorder(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}
	u := buildAgentAt(t, cfg)
	ref, ok := u.a.Refine.(*refine.Refiner)
	if !ok || ref.Signals == nil {
		t.Fatalf("Refiner.Signals is not wired: %+v", u.a.Refine)
	}
}
```

Create `cmd/spore/companion_test.go`:

```go
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func companionFixture(t *testing.T) (*config.Config, *store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	cfg.Companion.Timezone = "America/Los_Angeles"
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return cfg, st
}

func TestCompanionStatus(t *testing.T) {
	ctx := context.Background()
	cfg, st := companionFixture(t)
	_ = os.WriteFile(cfg.SelfPath(), []byte(strings.Repeat("x", 300)), 0o600)
	a, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, a.ID, store.InterestObserving, store.InterestCandidate)
	_, _ = st.TouchInterest(ctx, "topic:go", "Go")
	var out bytes.Buffer
	if err := companionStatus(ctx, &out, cfg, st); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"enabled", "America/Los_Angeles", "habit_days 3", "300 of 10240 bytes", "candidate 1", "observing 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status missing %q:\n%s", want, out.String())
		}
	}
}

func TestCompanionInterestsListsEvidence(t *testing.T) {
	ctx := context.Background()
	_, st := companionFixture(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", store.SourceChat)
	in, _ := st.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)")
	at := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	_ = st.AddInterestSignal(ctx, in.ID, sid, "asked", "2026-10-08", at)
	loc, _ := time.LoadLocation("America/Los_Angeles")
	var out bytes.Buffer
	if err := companionInterests(ctx, &out, st, loc); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"KEY", "stock:zs", "observing", "1", "2026-10-08", "Zscaler (ZS)"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("interests missing %q:\n%s", want, out.String())
		}
	}
}

func TestCompanionInterestsEmpty(t *testing.T) {
	_, st := companionFixture(t)
	var out bytes.Buffer
	if err := companionInterests(context.Background(), &out, st, time.UTC); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no interests recorded yet") {
		t.Fatalf("out = %q", out.String())
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags sqlite_fts5 ./cmd/spore/ -run 'SignalRecorder|Companion' -v`
Expected: build failure — `companionStatus undefined`; once those exist, `TestBuildAgentWiresTheSignalRecorder` fails with "Refiner.Signals is not wired".

- [ ] **Step 3: Implement**

`cmd/spore/wire.go`: add import `"github.com/codered/spore/internal/companion"` and, directly after `ref := refine.New(st, reg, rt, cfg, facts)`:

```go
	// The recorder is always attached; it does nothing while the companion
	// is off, so turning it on in config.toml needs no other wiring.
	ref.Signals = companion.NewRecorder(st, cfg)
```

Create `cmd/spore/companion.go`:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// cmdCompanion is the operator's view of what the companion has noticed.
// It reads the database directly, like spore recall, so it works with or
// without a running daemon.
func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore companion status | interests")
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	switch args[0] {
	case "status":
		return companionStatus(ctx, os.Stdout, cfg, st)
	case "interests":
		loc, err := cfg.Companion.Location()
		if err != nil {
			return err
		}
		return companionInterests(ctx, os.Stdout, st, loc)
	default:
		return fmt.Errorf("unknown companion command %q: want status or interests", args[0])
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
	fmt.Fprintf(w, "companion: %s\n", state)
	fmt.Fprintf(w, "timezone:  %s (habit_days %d)\n", loc, c.HabitDays)
	size := 0
	if fi, err := os.Stat(cfg.SelfPath()); err == nil {
		size = int(fi.Size())
	}
	fmt.Fprintf(w, "self.md:   %s, %d of %d bytes\n", cfg.SelfPath(), size, c.SelfMaxBytes)
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	counts := map[string]int{}
	for _, in := range all {
		counts[in.State]++
	}
	fmt.Fprint(w, "interests:")
	for _, s := range []string{store.InterestObserving, store.InterestCandidate, store.InterestProposed,
		store.InterestActive, store.InterestDeclined, store.InterestRetired} {
		fmt.Fprintf(w, " %s %d", s, counts[s])
	}
	fmt.Fprintln(w)
	return nil
}

func companionInterests(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(w, "no interests recorded yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tSTATE\tDAYS\tLAST SEEN\tLABEL")
	for _, in := range all {
		last := "-"
		if !in.LastSeen.IsZero() {
			last = in.LastSeen.In(loc).Format("2006-01-02")
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", in.Key, in.State, in.DaysSeen, last, in.Label)
	}
	return tw.Flush()
}
```

`cmd/spore/main.go`: in `usage`, after the `spore recall teardown` line, add:

```
  spore companion status       report whether the companion is on and what it tracks
  spore companion interests    list the interests it has noticed, with their evidence
```

In `dispatch`, after `case "recall":`:

```go
	case "companion":
		return cmdCompanion(ctx, cfg, args[1:])
```

- [ ] **Step 4: Run the full gate**

Run: `make vet fmtcheck lint test 2>&1 | tail -30`
Expected: every step clean, all packages `ok`.

Then: `make vulncheck tidycheck`
Expected: clean. (A vulncheck finding in a dependency this PR did not touch is reported to the controller, not fixed here.)

- [ ] **Step 5: Commit**

```bash
git add cmd/spore/wire.go cmd/spore/companion.go cmd/spore/companion_test.go cmd/spore/refine_wire_test.go cmd/spore/main.go
git commit -m "spore companion status|interests, and the signal recorder wired into the refiner

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```

---

### Task 9: docs and the manual check

**Files:**
- Modify: `README.md` (configuration section), `docs/superpowers/specs/2026-10-10-companion-design.md` (append "As built — PR 1")

- [ ] **Step 1: README**

Find the configuration section with `grep -n "^## \|\[refine\]" README.md`. After the `[refine]` documentation (or at the end of the configuration section if there is none), add:

````markdown
### Companion (preview)

Off by default. When on, spore notices what you keep coming back to (a stock you
check, a topic you return to) and keeps its own notes in `~/.spore/self.md`.
In this release it only observes; it does not message you yet.

```toml
[companion]
enabled = true
timezone = "America/Los_Angeles"   # days are counted in this zone; empty = this machine's
habit_days = 3                     # distinct days before something counts as a habit
self_max_bytes = 10240
```

`spore companion status` and `spore companion interests` show what it has
noticed. If your config has its own `[policy] allow` list, add `self_note` to it
or spore will ask before each note.
````

- [ ] **Step 2: Spec "As built"**

Append to the spec:

```markdown
## As built — PR 1

- Day counts are derived from `interest_signals.day` on every read; there is
  no recompute step after a session delete.
- `Sweep` runs after each `Record`. Until PR 2's heartbeat exists, a candidate
  whose evidence shrank (a deleted session) is demoted only on the next
  recorded signal; `spore companion interests` always shows live counts.
- `self.md` counts toward the `System` figure in `/context`.
```

- [ ] **Step 3: Manual check (controller runs this, not a worker)**

Isolate from the running daemon (see memory: `spore once` uses a running daemon): build the branch binary, run it with `HOME=$(mktemp -d)`, a copied config with `[companion] enabled = true`, `timezone = "America/Los_Angeles"`, `daemon.addr = "127.0.0.1:7788"`, and no `[bridge.discord]`. Then:

1. `spore once "what's ZS trading at?"`, then `/refine` in that session via `spore chat <id>`. Expect a note `refined: ... 1 interest signal`.
2. `spore companion interests`. Expect `stock:zs observing 1`.
3. `spore once "remember in your own notes that I'm watching ZS earnings"`. Expect a `self_note` call and `~/.spore/self.md` (under the temp HOME) to contain the note under `## Threads with you`.
4. `/refine rollback` in that session. Expect `self.md` restored.

- [ ] **Step 4: Commit**

```bash
git add README.md docs/superpowers/specs/2026-10-10-companion-design.md
git commit -m "docs: companion preview in the README; spec as-built notes for PR 1

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_019fWdDMCHJZDH1D15PRUvNr"
```
