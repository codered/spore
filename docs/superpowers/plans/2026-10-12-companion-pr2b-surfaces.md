# Companion PR 2b: pinned row, proposal bar, /companion — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the companion usable from every surface: its session pinned in the TUI and web lists with an unread badge, open proposals answerable from a bar (TUI `alt+y/n/x`, web buttons), and `/companion` on the TUI, web, Discord and plain chat; companion Discord sends ping nobody.

**Architecture:** One formatter, `companion.Run`, moves out of `cmd/spore` so the CLI and the daemon print identical text. The daemon gains `GET /api/companion/proposals` and `POST /api/companion/command`; every chat surface turns `/companion …` into one call and prints the text. Clients refetch open proposals when the companion session is opened and on each `job_note` event in it. The Discord `Message` gains a `Quiet` flag that sends `allowed_mentions: {parse: []}`.

**Tech Stack:** Go 1.x, SQLite (`-tags sqlite_fts5`), Bubble Tea / lipgloss (TUI), vanilla JS (web), discordgo.

**Spec:** `docs/superpowers/specs/2026-10-10-companion-design.md`, sections "PR 2b decisions" and "Design — PR 2b" (at the end of the file). Read both before starting.

**Worktree:** `/home/code/development/spore-pr2b`, branch `companion-pr2b`. Run every command from there.

## Global Constraints

- Build/test tag: always `-tags sqlite_fts5` (`make test` adds it). A failure without the tag is not "pre-existing".
- Gate before every commit: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./...` plus the task's package tests. Final gate (Task 7) runs every CI step: `make vet fmtcheck lint vulncheck tidycheck test`.
- `check` is CLI-only. `/companion` runs `status`, `interests`, `interests undecline <key>`, `outreach`, `pause [dur]`, `resume`, `answer <id> yes|not-now|never`. No args = `status`.
- Choices are exactly `yes`, `not-now`, `never` (`companion.ChoiceYes/ChoiceNotNow/ChoiceNever`).
- An open proposal = outreach kind `proposal`, outcome `pending` or `unanswered`, interest state `proposed`.
- Web: the CSP forbids `style=` attributes; set styles through classes or the `h()` helper's `style` key only. Text from the model goes in as text nodes (`text:`), never markup.
- TUI: model text shown on screen goes through `visible()` first.
- Match surrounding comment density and idiom. Comments say why, in plain sentences.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4
  ```
- Never `git add -A`; add the files the task names.

## Review Focus

1. **Approval and proposal open together in the companion session** — `alt+y`/`alt+n` must answer the approval, never the proposal; `alt+x` does nothing; the bar hides. Pinned by Task 5's `TestAnApprovalAnswersBeforeAProposal`.
2. **`alt+y/n/x` outside the companion session, or with no open proposal** — must keep today's behaviour (Esc then the key: `n` new session, `x` cancel agent). Pinned by Task 5's `TestProposalKeysFallThroughWithNothingOpen`.
3. **A proposal answered elsewhere (Discord, CLI, another tab)** — the bar must clear on the `job_note` the answer posts, not show a stale proposal whose press returns 409. Pinned by Task 5's `TestAJobNoteRefetchesProposals`; the web is checked in the manual gate.
4. **Model text with `@everyone` in a companion Discord send** — must not ping. Pinned by Task 3's `TestCompanionSendsAreQuiet`.
5. **`/companion` with bad input** (`check`, unknown, `answer x yes`, `pause soon`) — must be a 400 with a usage line on the API and a readable line on every surface, never a 500 or a model turn. Pinned by Task 2's `TestCompanionCommandRoute` and Task 3's `TestSlashCompanionRepliesQuietlyWithoutATurn`.

---

### Task 1: `store.OpenProposals` and `companion.Run`

**Files:**
- Modify: `internal/store/outreach.go` (add `OpenProposals` after `OutreachByOutcome`)
- Test: `internal/store/outreach_test.go`
- Create: `internal/companion/command.go`
- Create: `internal/companion/command_test.go`
- Modify: `cmd/spore/companion.go` (delete `companionCommand`, `companionStatus`, `companionOutreach`, `companionInterests`; call `companion.Run`)
- Modify: `cmd/spore/companion_test.go` (delete the six tests that move; keep `TestCompanionCheckRejectsBadFlags`)

**Interfaces:**
- Produces: `func (s *store.Store) OpenProposals(ctx context.Context) ([]store.Outreach, error)` — newest first.
- Produces: `var companion.ErrUsage error`; `const companion.Usage string`; `func companion.Run(ctx context.Context, w io.Writer, svc *companion.Service, args []string) error`.

- [ ] **Step 1: Write the failing store test** — append to `internal/store/outreach_test.go` (add `"slices"` to its imports):

```go
// OpenProposals lists what a button press can still answer, newest first:
// the test Service.AnswerProposal applies.
func TestOpenProposals(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	at := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	interest := func(key, state string) Interest {
		in, err := st.TouchInterest(ctx, key, key)
		if err != nil {
			t.Fatal(err)
		}
		if state != InterestObserving {
			if _, err := st.SetInterestState(ctx, in.ID, InterestObserving, state); err != nil {
				t.Fatal(err)
			}
		}
		return in
	}
	add := func(in Interest, kind string, sent time.Time) int64 {
		id, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: kind, Channel: OutreachChannelTerminal, Text: in.Key, SentAt: sent})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	zs := interest("stock:zs", InterestProposed)
	goTopic := interest("topic:go", InterestProposed)
	declined := interest("topic:old", InterestDeclined)

	older := add(zs, OutreachProposal, at)
	newer := add(goTopic, OutreachProposal, at.Add(time.Hour))
	add(zs, OutreachNudge, at.Add(2*time.Hour))             // not a proposal
	add(declined, OutreachProposal, at.Add(3*time.Hour))    // its interest is no longer proposed
	answered := add(zs, OutreachProposal, at.Add(4*time.Hour)) // answered below
	if _, err := st.ResolveOutreach(ctx, answered, []string{OutcomePending}, OutcomeReplied, "", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// unanswered is still open: the user may press the button late.
	if _, err := st.ResolveOutreach(ctx, older, []string{OutcomePending}, OutcomeUnanswered, "", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.OpenProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, o := range got {
		ids = append(ids, o.ID)
	}
	if !slices.Equal(ids, []int64{newer, older}) {
		t.Fatalf("open proposals = %v, want [%d %d]", ids, newer, older)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/store -run TestOpenProposals`
Expected: FAIL to compile — `st.OpenProposals undefined`.

- [ ] **Step 3: Implement** — add to `internal/store/outreach.go`, after `OutreachByOutcome`:

```go
// OpenProposals lists the proposals a button press can still answer, newest
// first: kind proposal, outcome pending or unanswered, and an interest that
// is still proposed. Service.AnswerProposal refuses anything else, so a
// client that lists these never offers a press that fails.
func (s *Store) OpenProposals(ctx context.Context) ([]Outreach, error) {
	return s.queryOutreach(ctx, `SELECT `+outreachColumns+` FROM outreach
		WHERE kind = ? AND outcome IN (?, ?)
		  AND interest_id IN (SELECT id FROM interests WHERE state = ?)
		ORDER BY id DESC`,
		OutreachProposal, OutcomePending, OutcomeUnanswered, InterestProposed)
}
```

- [ ] **Step 4: Run it to verify it passes**

Run: `go test -tags sqlite_fts5 ./internal/store -run TestOpenProposals`
Expected: PASS

- [ ] **Step 5: Write the failing `Run` tests** — create `internal/companion/command_test.go`. These are the CLI tests from `cmd/spore/companion_test.go`, moved onto `Run`. If a helper name collides with one already in `internal/companion` tests, rename it with a `cmd` prefix.

```go
package companion_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

func cmdFixture(t *testing.T) (*companion.Service, *config.Config, *store.Store) {
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
	return companion.NewService(st, cfg), cfg, st
}

func runCmd(t *testing.T, svc *companion.Service, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := companion.Run(context.Background(), &out, svc, args)
	return out.String(), err
}

func TestRunStatus(t *testing.T) {
	ctx := context.Background()
	svc, cfg, st := cmdFixture(t)
	_ = os.WriteFile(cfg.SelfPath(), []byte(strings.Repeat("x", 300)), 0o600)
	a, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, a.ID, store.InterestObserving, store.InterestCandidate)
	_, _ = st.TouchInterest(ctx, "topic:go", "Go")
	// No arguments means status, as /companion alone does.
	for _, args := range [][]string{{"status"}, nil} {
		out, err := runCmd(t, svc, args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"enabled", "America/Los_Angeles", "habit_days 3", "300 of 10240 bytes", "candidate 1", "observing 1"} {
			if !strings.Contains(out, want) {
				t.Errorf("status %v missing %q:\n%s", args, want, out)
			}
		}
	}
}

func TestRunStatusShowsTheHeartbeat(t *testing.T) {
	ctx := context.Background()
	svc, _, st := cmdFixture(t)
	_ = st.SetCompanionState(ctx, companion.StateFailures, "3")
	_ = st.SetCompanionState(ctx, companion.StateLastResult, "failed: provider down")
	_ = companion.SetPausedUntil(ctx, st, time.Now().Add(time.Hour))
	out, err := runCmd(t, svc, "status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"channel:   terminal", "paused until", "budget:   ", "0 of 4 sent today", "failed: provider down", "warning: the last 3 reflections failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("status missing %q:\n%s", want, out)
		}
	}
}

func TestRunInterestsListsEvidence(t *testing.T) {
	ctx := context.Background()
	svc, _, st := cmdFixture(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", store.SourceChat)
	in, _ := st.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)")
	at := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	_ = st.AddInterestSignal(ctx, in.ID, sid, "asked", "2026-10-08", at)
	out, err := runCmd(t, svc, "interests")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "KEY") {
		t.Fatalf("want a header and one row, got:\n%s", out)
	}
	// Pin the days-seen column: field 2 must be exactly "1", not any "1".
	f := strings.Fields(lines[1])
	if len(f) < 5 || f[0] != "stock:zs" || f[1] != "observing" || f[2] != "1" || f[3] != "2026-10-08" {
		t.Errorf("row fields = %q, want key stock:zs, state observing, days 1, last seen 2026-10-08", f)
	}
	if !strings.Contains(lines[1], "Zscaler (ZS)") {
		t.Errorf("row missing label: %q", lines[1])
	}
}

func TestRunInterestsEmpty(t *testing.T) {
	svc, _, _ := cmdFixture(t)
	out, err := runCmd(t, svc, "interests")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no interests recorded yet") {
		t.Fatalf("out = %q", out)
	}
}

func TestRunOutreachLists(t *testing.T) {
	ctx := context.Background()
	svc, _, st := cmdFixture(t)
	_, _ = st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Channel: store.OutreachChannelDiscord,
		Text: "You've checked ZS three days running.", SentAt: time.Now().Add(-time.Hour)})
	out, err := runCmd(t, svc, "outreach")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "proposal") ||
		!strings.Contains(lines[1], "pending") || !strings.Contains(lines[1], "checked ZS") {
		t.Fatalf("outreach =\n%s", out)
	}
}

func TestRunPauseResumeUndecline(t *testing.T) {
	ctx := context.Background()
	svc, _, st := cmdFixture(t)
	if out, err := runCmd(t, svc, "pause", "2h"); err != nil || !strings.Contains(out, "paused until") {
		t.Fatalf("pause = %q %v", out, err)
	}
	if until, _ := companion.PausedUntil(ctx, st); time.Until(until) < 119*time.Minute {
		t.Fatalf("paused until %v", until)
	}
	if out, err := runCmd(t, svc, "resume"); err != nil || !strings.Contains(out, "resumed") {
		t.Fatalf("resume = %q %v", out, err)
	}
	if until, _ := companion.PausedUntil(ctx, st); !until.IsZero() {
		t.Fatalf("still paused until %v", until)
	}
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = st.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	_, _ = st.DeclineInterest(ctx, in.ID)
	if out, err := runCmd(t, svc, "interests", "undecline", "stock:zs"); err != nil || !strings.Contains(out, "observed again") {
		t.Fatalf("undecline = %q %v", out, err)
	}
	if got, _, _ := st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestObserving {
		t.Fatalf("state = %s", got.State)
	}
}

// Every malformed line is ErrUsage, so the daemon can answer 400 rather
// than 500. answer and check are not Run's: they need a daemon.
func TestRunRejectsMalformedLines(t *testing.T) {
	svc, _, _ := cmdFixture(t)
	for _, args := range [][]string{{"pause", "soon"}, {"interests", "extra"}, {"bogus"}, {"answer", "1", "yes"}, {"check"}} {
		if _, err := runCmd(t, svc, args...); !errors.Is(err, companion.ErrUsage) {
			t.Errorf("%v: err = %v, want ErrUsage", args, err)
		}
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/companion -run 'TestRun'`
Expected: FAIL to compile — `companion.Run undefined`.

- [ ] **Step 7: Implement `companion.Run`** — create `internal/companion/command.go`. The four formatters are moved from `cmd/spore/companion.go` with their output unchanged; only their signatures and error values change.

```go
package companion

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/codered/spore/internal/store"
)

// ErrUsage marks a companion command line Run cannot run: an unknown
// subcommand or a malformed argument. The daemon answers it with 400.
var ErrUsage = errors.New("usage")

// Usage lists what a companion command line may say, in the CLI and in chat.
const Usage = "status | interests | interests undecline <key> | outreach | pause [duration] | resume | answer <id> yes|not-now|never"

func usageErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUsage, fmt.Sprintf(format, a...))
}

// Run runs one companion subcommand that needs only the store and writes
// its text to w. `spore companion` and the daemon's /companion both call it,
// so the terminal and every chat surface read the same. No arguments means
// status. answer and check need the daemon and are handled by the callers.
func Run(ctx context.Context, w io.Writer, svc *Service, args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	st := svc.Store
	loc, err := svc.Cfg.Companion.Location()
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		return writeStatus(ctx, w, svc)
	case "interests":
		if len(args) == 3 && args[1] == "undecline" {
			if err := svc.Undecline(ctx, args[2]); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(w, "%s is observed again\n", args[2])
			return nil
		}
		if len(args) > 1 {
			return usageErr("interests [undecline <key>]")
		}
		return writeInterests(ctx, w, st, loc)
	case "outreach":
		return writeOutreach(ctx, w, st, loc)
	case "pause":
		d := 24 * time.Hour
		if len(args) > 1 {
			if d, err = time.ParseDuration(args[1]); err != nil {
				return usageErr("pause duration %q: %v (try 2h or 72h)", args[1], err)
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
		return usageErr("unknown companion command %q: want %s", args[0], Usage)
	}
}

func writeStatus(ctx context.Context, w io.Writer, svc *Service) error {
	cfg, st := svc.Cfg, svc.Store
	c := cfg.Companion
	state := "disabled"
	if c.Enabled {
		state = "enabled"
	}
	loc, err := c.Location()
	if err != nil {
		return err
	}
	s, err := svc.Status(ctx)
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
	if s.Failures >= FailureWarning {
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

// writeOutreach lists the last week's outreach, oldest first.
func writeOutreach(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	rows, err := st.OutreachSince(ctx, time.Now().Add(-RecentOutreach))
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

func writeInterests(ctx context.Context, w io.Writer, st *store.Store, loc *time.Location) error {
	all, err := st.Interests(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		_, _ = fmt.Fprintln(w, "no interests recorded yet")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "KEY\tSTATE\tDAYS\tLAST SEEN\tLABEL")
	for _, in := range all {
		last := "-"
		if !in.LastSeen.IsZero() {
			last = in.LastSeen.In(loc).Format("2006-01-02")
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", in.Key, in.State, in.DaysSeen, last, in.Label)
	}
	return tw.Flush()
}
```

Note `{"answer", ...}` and `{"check"}` reach `default` and are `ErrUsage`, which is what the test wants: Run never handles them.

- [ ] **Step 8: Point the CLI at `Run`** — in `cmd/spore/companion.go`:
  1. Replace the last line of `cmdCompanion` (`return companionCommand(ctx, os.Stdout, cfg, st, args)`) with:
     ```go
     	return companion.Run(ctx, os.Stdout, companion.NewService(st, cfg), args)
     ```
  2. Delete the functions `companionCommand`, `companionStatus`, `companionOutreach` and `companionInterests` entirely.
  3. Update the comment on `cmdCompanion` to add: "The text comes from companion.Run, which the daemon's /companion also uses."
  4. Remove the imports the deletions orphan (`io`, `strings`, `text/tabwriter`, and `store` if unused — let `go build` tell you; `store.Open` is still used).
- [ ] **Step 9: Remove the moved CLI tests** — in `cmd/spore/companion_test.go`, delete `companionFixture`, `TestCompanionStatus`, `TestCompanionInterestsListsEvidence`, `TestCompanionInterestsEmpty`, `TestCompanionStatusShowsTheHeartbeat`, `TestCompanionOutreachLists`, `TestCompanionPauseResumeUndecline`. Keep `TestCompanionCheckRejectsBadFlags`. Fix its imports.

- [ ] **Step 10: Run the gates**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./internal/store ./internal/companion ./cmd/spore`
Expected: all PASS. `grep -rn "companionStatus\|companionCommand\|companionInterests\|companionOutreach" cmd internal` prints nothing.

- [ ] **Step 11: Commit**

```bash
git add internal/store/outreach.go internal/store/outreach_test.go internal/companion/command.go internal/companion/command_test.go cmd/spore/companion.go cmd/spore/companion_test.go
git commit -m "companion: Run formats every store-only subcommand; store lists open proposals

The CLI's status/interests/outreach/pause/resume/undecline text moves
into companion.Run so /companion can print the same thing from the
daemon. OpenProposals applies the test AnswerProposal does.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 2: Daemon routes, client methods, plain-chat `/companion`

**Files:**
- Modify: `internal/daemon/companion.go` (add `ProposalJSON`, `OpenProposals`, `handleProposals`, `CompanionCommand`, `handleCompanionCommand`)
- Modify: `internal/daemon/server.go:204-206` (register two routes)
- Test: `internal/daemon/companion_test.go`
- Modify: `cmd/spore/client.go` (add `companionCommand`, `companionProposals`, `answerProposal`)
- Modify: `cmd/spore/chat.go` (`runPlainSlash` handles `/companion`)
- Test: `cmd/spore/chat_test.go`

**Interfaces:**
- Consumes: `store.OpenProposals`, `companion.Run`, `companion.ErrUsage` (Task 1); existing `Server.AnswerProposal(ctx, outreachID int64, choice string) (string, error)`.
- Produces:
  - `type daemon.ProposalJSON struct { ID int64 "id"; Text string "text"; SentAt time.Time "sent_at"; InterestKey string "interest_key" }`
  - `func (s *daemon.Server) OpenProposals(ctx) ([]daemon.ProposalJSON, error)` — never nil on success.
  - `func (s *daemon.Server) CompanionCommand(ctx context.Context, args []string) (string, error)` — Task 3's Discord bridge calls this.
  - `GET /api/companion/proposals` → `[]ProposalJSON`; `POST /api/companion/command {"args":[...]}` → `{"text": "..."}`.
  - `func (c *client) companionCommand(ctx, args []string) (string, error)`, `companionProposals(ctx) ([]daemon.ProposalJSON, error)`, `answerProposal(ctx, id int64, choice string) (string, error)` — Task 4 uses these.

- [ ] **Step 1: Write the failing daemon tests** — append to `internal/daemon/companion_test.go`:

```go
func commandJSON(t *testing.T, url string, args []string) (int, map[string]string) {
	t.Helper()
	res := postJSON(t, url, map[string]any{"args": args})
	defer res.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestCompanionCommandRoute(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	url := ts.URL + "/api/companion/command"
	if code, body := commandJSON(t, url, nil); code != http.StatusOK || !strings.Contains(body["text"], "companion:") {
		t.Fatalf("no args = %d %v, want status", code, body)
	}
	if code, body := commandJSON(t, url, []string{"pause", "2h"}); code != http.StatusOK || !strings.Contains(body["text"], "paused until") {
		t.Fatalf("pause = %d %v", code, body)
	}
	for _, args := range [][]string{{"check"}, {"bogus"}, {"answer", "x", "yes"}, {"answer", "1"}, {"pause", "soon"}} {
		if code, body := commandJSON(t, url, args); code != http.StatusBadRequest || body["error"] == "" {
			t.Errorf("%v = %d %v, want 400 with a message", args, code, body)
		}
	}
}

// answer goes through AnswerProposal, so its note lands in the companion
// session (and, live, in its Discord thread) exactly as a button press's.
func TestCompanionCommandAnswerPostsTheNote(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	url := ts.URL + "/api/companion/command"
	args := []string{"answer", strconv.FormatInt(id, 10), companion.ChoiceNever}
	code, body := commandJSON(t, url, args)
	if code != http.StatusOK || body["text"] == "" {
		t.Fatalf("answer = %d %v", code, body)
	}
	sid, _, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	msgs, _ := s.store.Messages(ctx, sid)
	if last := msgs[len(msgs)-1]; last.Role != store.RoleNote {
		t.Fatalf("last message = %+v, want the answer's note", last)
	}
	if code, _ := commandJSON(t, url, args); code != http.StatusConflict {
		t.Fatalf("second answer = %d, want 409", code)
	}
}

func TestOpenProposalsRoute(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	list := func() []ProposalJSON {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/companion/proposals")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		var out []ProposalJSON
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if out == nil {
			t.Fatal("an empty list must be [], not null")
		}
		return out
	}
	got := list()
	if len(got) != 1 || got[0].ID != id || got[0].Text != "watch ZS?" || got[0].InterestKey != "stock:zs" || got[0].SentAt.IsZero() {
		t.Fatalf("proposals = %+v", got)
	}
	res := postJSON(t, ts.URL+"/api/companion/proposals/"+strconv.FormatInt(id, 10), map[string]string{"choice": companion.ChoiceNotNow})
	res.Body.Close()
	if got := list(); len(got) != 0 {
		t.Fatalf("after an answer = %+v, want none", got)
	}
}

func TestCompanionRoutesWithoutCompanionAre503(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/companion/proposals")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("proposals = %d, want 503", res.StatusCode)
	}
	if code, _ := commandJSON(t, ts.URL+"/api/companion/command", []string{"status"}); code != http.StatusServiceUnavailable {
		t.Fatalf("command = %d, want 503", code)
	}
}
```

Before writing, confirm `writeError` puts its message under the JSON key `"error"` (read `writeError` in `internal/daemon`). If it uses another key, change `body["error"]` to match.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/daemon -run 'TestCompanionCommand|TestOpenProposalsRoute|TestCompanionRoutesWithout'`
Expected: FAIL to compile — `ProposalJSON undefined`.

- [ ] **Step 3: Implement the daemon side** — add to `internal/daemon/companion.go` (add `"strings"` to its imports):

```go
// ProposalJSON is one open proposal in GET /api/companion/proposals.
type ProposalJSON struct {
	ID          int64     `json:"id"`
	Text        string    `json:"text"`
	SentAt      time.Time `json:"sent_at"`
	InterestKey string    `json:"interest_key"`
}

// OpenProposals lists the proposals a press can still answer, newest first.
// The TUI and web draw their proposal bar from it.
func (s *Server) OpenProposals(ctx context.Context) ([]ProposalJSON, error) {
	if s.companion == nil {
		return nil, errCompanionOff
	}
	rows, err := s.store.OpenProposals(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ProposalJSON, 0, len(rows))
	for _, o := range rows {
		p := ProposalJSON{ID: o.ID, Text: o.Text, SentAt: o.SentAt}
		in, ok, err := s.store.InterestByID(ctx, o.InterestID)
		if err != nil {
			return nil, err
		}
		if ok {
			p.InterestKey = in.Key
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) handleProposals(w http.ResponseWriter, r *http.Request) {
	list, err := s.OpenProposals(r.Context())
	switch {
	case errors.Is(err, errCompanionOff):
		writeError(w, http.StatusServiceUnavailable, "%v", err)
	case err != nil:
		writeError(w, http.StatusInternalServerError, "%v", err)
	default:
		writeJSON(w, http.StatusOK, list)
	}
}

// CompanionCommand runs one /companion line and returns its text, for every
// chat surface. answer goes through AnswerProposal so its note lands in the
// companion session as a button press's does; check runs only from the
// terminal, since it can take a model call's time and spends budget.
func (s *Server) CompanionCommand(ctx context.Context, args []string) (string, error) {
	if s.companion == nil {
		return "", errCompanionOff
	}
	if len(args) > 0 {
		switch args[0] {
		case "check":
			return "", fmt.Errorf("%w: check runs from the terminal: spore companion check", companion.ErrUsage)
		case "answer":
			if len(args) != 3 {
				return "", fmt.Errorf("%w: answer <id> yes|not-now|never", companion.ErrUsage)
			}
			id, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				return "", fmt.Errorf("%w: outreach id %q must be a number", companion.ErrUsage, args[1])
			}
			return s.AnswerProposal(ctx, id, args[2])
		}
	}
	var b strings.Builder
	if err := companion.Run(ctx, &b, s.companion, args); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (s *Server) handleCompanionCommand(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Args []string `json:"args"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "decode body: %v", err)
		return
	}
	text, err := s.CompanionCommand(r.Context(), body.Args)
	switch {
	case errors.Is(err, errCompanionOff):
		writeError(w, http.StatusServiceUnavailable, "%v", err)
	case errors.Is(err, companion.ErrNotProposal):
		writeError(w, http.StatusConflict, "%v", err)
	case err != nil:
		// Usage errors and a bad choice, as the proposal route answers them.
		writeError(w, http.StatusBadRequest, "%v", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"text": text})
	}
}
```

In `internal/daemon/server.go`, beside the three existing companion routes (around line 204), add:

```go
	api("GET /api/companion/proposals", s.handleProposals)
	api("POST /api/companion/command", s.handleCompanionCommand)
```

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -tags sqlite_fts5 ./internal/daemon -run 'TestCompanion|TestOpenProposalsRoute|TestProposalRoute'`
Expected: PASS

- [ ] **Step 5: Write the failing plain-chat test** — append to `cmd/spore/chat_test.go`:

```go
func TestRunPlainSlashRunsCompanionWithoutPostingAMessage(t *testing.T) {
	var got struct {
		Args []string `json:"args"`
	}
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "paused until Mon\n"})
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/companion pause 2h", false, &out)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if len(paths) != 1 || paths[0] != "POST /api/companion/command" {
		t.Fatalf("requests = %v, want one command POST and no message", paths)
	}
	if strings.Join(got.Args, " ") != "pause 2h" {
		t.Fatalf("args = %q", got.Args)
	}
	if out.String() != "paused until Mon\n" {
		t.Fatalf("out = %q", out.String())
	}
	// Bare /companion sends no arguments, which the daemon reads as status.
	handled, err = runPlainSlash(context.Background(), c, "s1", "/companion", false, &out)
	if err != nil || !handled || len(got.Args) != 0 {
		t.Fatalf("bare /companion: handled=%v err=%v args=%q", handled, err, got.Args)
	}
	// A word that only starts with /companion is an ordinary message.
	if handled, _ := runPlainSlash(context.Background(), c, "s1", "/companionship", false, &out); handled {
		t.Fatal("/companionship was taken as a command")
	}
}
```

Add `"strings"` to the test file's imports if missing.

- [ ] **Step 6: Run it to verify it fails**

Run: `go test -tags sqlite_fts5 ./cmd/spore -run TestRunPlainSlashRunsCompanion`
Expected: FAIL — `handled` is false.

- [ ] **Step 7: Implement the client methods and the plain-chat branch** — in `cmd/spore/client.go` (add `"strconv"` and the `daemon` import if missing; `daemon` is already imported if other methods return `daemon.*JSON`):

```go
// companionCommand runs one /companion line in the daemon and returns its
// text. The daemon reads no args as status.
func (c *client) companionCommand(ctx context.Context, args []string) (string, error) {
	if args == nil {
		args = []string{}
	}
	var out struct {
		Text string `json:"text"`
	}
	err := c.do(ctx, "POST", "/api/companion/command", map[string]any{"args": args}, &out)
	return out.Text, err
}

// companionProposals lists the companion's open proposals, newest first.
func (c *client) companionProposals(ctx context.Context) ([]daemon.ProposalJSON, error) {
	var out []daemon.ProposalJSON
	err := c.do(ctx, "GET", "/api/companion/proposals", nil, &out)
	return out, err
}

// answerProposal presses one of a proposal's buttons.
func (c *client) answerProposal(ctx context.Context, id int64, choice string) (string, error) {
	var out struct {
		Message string `json:"message"`
	}
	err := c.do(ctx, "POST", "/api/companion/proposals/"+strconv.FormatInt(id, 10), map[string]string{"choice": choice}, &out)
	return out.Message, err
}
```

In `cmd/spore/chat.go`, `runPlainSlash`, right after the `/model` branch:

```go
	if text == "/companion" || strings.HasPrefix(text, "/companion ") {
		report, err := c.companionCommand(ctx, strings.Fields(text)[1:])
		if err != nil {
			return true, err
		}
		_, err = fmt.Fprint(out, report)
		return true, err
	}
```

- [ ] **Step 8: Run the gates**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./internal/daemon ./cmd/spore`
Expected: PASS (including `TestAppJSRoutesAreRegistered`, which is unaffected yet).

- [ ] **Step 9: Commit**

```bash
git add internal/daemon/companion.go internal/daemon/server.go internal/daemon/companion_test.go cmd/spore/client.go cmd/spore/chat.go cmd/spore/chat_test.go
git commit -m "daemon: /api/companion/proposals and /command; /companion in plain chat

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 3: Discord — quiet sends and `/companion`

**Files:**
- Modify: `internal/bridge/discord/client.go` (`Message.Quiet`; `gatewayClient.Send` and `Respond` set `AllowedMentions`)
- Modify: `internal/bridge/discord/companion.go` (`CompanionCommander`; outreach and anchor are `Quiet`)
- Modify: `internal/bridge/discord/bridge.go` (`Options.Commands`, `Bridge.commands`, `/companion` branch, `handleCompanion`, `Deliver` is `Quiet`)
- Modify: `cmd/spore/wire.go:320` (`Commands: srv`)
- Test: `internal/bridge/discord/companion_test.go`

**Interfaces:**
- Consumes: `Server.CompanionCommand(ctx, args []string) (string, error)` (Task 2).
- Produces: `Message.Quiet bool`; `type CompanionCommander interface { CompanionCommand(ctx context.Context, args []string) (string, error) }`; `Options.Commands CompanionCommander`.

**Scope note:** `Bridge.Deliver` posts every note (job output, check-ins, the companion's answers) and does not know which session is the companion's, so all notes become quiet. That is wider than "notes in the companion thread" in the spec and is deliberate: a note never needs to ping. `Respond` (ephemeral button answers) is always quiet for the same reason. Turn output, approvals and `/usage` are untouched; the backlog entry in Task 7 covers them.

- [ ] **Step 1: Write the failing tests** — in `internal/bridge/discord/companion_test.go`:
  1. Give `fakeProposals` a command recorder (it becomes the fake for both interfaces):

```go
func (f *fakeProposals) CompanionCommand(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, append([]string(nil), args...))
	if len(args) > 0 && args[0] == "check" {
		return "", errors.New("usage: check runs from the terminal")
	}
	return "companion said: " + strings.Join(args, " ") + "\n", nil
}

func (f *fakeProposals) commandArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.commands...)
}
```

  and add the field `commands [][]string` to the `fakeProposals` struct.
  2. In `companionBridge`, add `Commands: props,` to the `Options` literal.
  3. Append the tests:

```go
func TestSlashCompanionRepliesQuietlyWithoutATurn(t *testing.T) {
	_, f, turns, _, props := companionBridge(t, config.CompanionDiscordThread)
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/companion pause 2h"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 0 })
	got := f.sentTo("C1")[0].Message
	if !strings.Contains(got.Content, "companion said: pause 2h") || !got.Quiet {
		t.Fatalf("reply = %+v, want the command's text, sent quiet", got)
	}
	if a := props.commandArgs(); len(a) != 1 || strings.Join(a[0], " ") != "pause 2h" {
		t.Fatalf("command args = %q", a)
	}
	f.deliver(Inbound{MessageID: "m2", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/companion check"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 1 })
	if got := f.sentTo("C1")[1].Message.Content; !strings.Contains(got, "terminal") {
		t.Fatalf("check reply = %q, want the refusal", got)
	}
	if turns.startCount() != 0 {
		t.Fatalf("/companion started %d turns, want 0", turns.startCount())
	}
}

// Text the model wrote must never ping: the anchor, every outreach and
// every note go out with no mentions allowed.
func TestCompanionSendsAreQuiet(t *testing.T) {
	ctx := context.Background()
	b, f, _, st, _ := companionBridge(t, config.CompanionDiscordThread)
	sid, _ := st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
	ch := b.CompanionChannel(config.CompanionDiscordConfig{Mode: config.CompanionDiscordThread, ChannelID: "CC", UserIDs: []string{"U"}})
	if err := ch.Send(ctx, sid, store.Outreach{ID: 1, Kind: store.OutreachProposal, Text: "@everyone watch ZS?"}); err != nil {
		t.Fatal(err)
	}
	b.Deliver(ctx, sid, "Watching ZS. @here")
	sent := f.allSent()
	if len(sent) != 3 {
		t.Fatalf("sent %d messages, want anchor, outreach and note: %+v", len(sent), sent)
	}
	for _, s := range sent {
		if !s.Message.Quiet {
			t.Errorf("%q was sent with mentions allowed", s.Message.Content)
		}
	}
}
```

  If `C1` is not an admitted channel in `testDiscordConfig()`, use the channel id `TestSlashUsageInAChannelReportsOnlyTheTotal` (`usage_test.go`) uses.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/bridge/discord -run 'TestSlashCompanion|TestCompanionSendsAreQuiet'`
Expected: FAIL to compile — `Commands` / `Quiet` unknown.

- [ ] **Step 3: Implement**

  a. `internal/bridge/discord/client.go`, in `Message`, after `Selects`:

```go
	// Quiet sends with no mentions allowed: an @everyone, role or user
	// mention in Content renders as text but pings nobody. Set it on any
	// message whose text a model or the companion wrote.
	Quiet bool
```

  In `gatewayClient.Send`, build the struct first and set the field when quiet:

```go
	send := &discordgo.MessageSend{
		Content:    m.Content,
		Embeds:     embedsFor(m.Embeds),
		Components: componentsFor(m.Buttons, m.Selects),
	}
	if m.Quiet {
		send.AllowedMentions = noMentions()
	}
	msg, err := c.sess.ChannelMessageSendComplex(channelID, send, discordgo.WithContext(ctx))
```

  In `gatewayClient.Respond`, add `AllowedMentions: noMentions(),` to the `InteractionResponseData` literal. Add the helper below `Respond`:

```go
// noMentions allows no mention to ping. An empty Parse list, not a nil
// one, is what tells Discord "none": nil leaves its default, which pings.
func noMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}
```

  Check in discordgo's source (`go doc github.com/bwmarrin/discordgo.MessageAllowedMentions`) that `Parse` has no `omitempty` tag; if it does, report it and stop, because an empty list would then be dropped.

  b. `internal/bridge/discord/companion.go`: below `ProposalAnswerer`, add

```go
// CompanionCommander runs a /companion line. The daemon is one.
type CompanionCommander interface {
	CompanionCommand(ctx context.Context, args []string) (string, error)
}
```

  In `CompanionChannel.Send`, change `m := Message{Content: o.Text}` to `m := Message{Content: o.Text, Quiet: true}`. In `channelFor`, add `Quiet: true` to the anchor's `Message{...}`.

  c. `internal/bridge/discord/bridge.go`:
   - `Options`: after `Proposals`, add
     ```go
     	// Commands runs /companion. Nil means /companion says the companion
     	// is off.
     	Commands CompanionCommander
     ```
   - `Bridge` struct: after `proposals ProposalAnswerer`, add `commands CompanionCommander`; in `New`, after `proposals: o.Proposals,`, add `commands: o.Commands,`.
   - In `handle`, after the `/model` branch:
     ```go
     	if content == "/companion" || strings.HasPrefix(content, "/companion ") {
     		b.handleCompanion(in, strings.Fields(content)[1:])
     		b.settle(in) // no turn runs, so nothing else settles it; see /new
     		return
     	}
     ```
   - After `handleUsage`, add:
     ```go
     // companionReplyLimit keeps a /companion reply inside Discord's 2000
     // character message cap with room for the code fence.
     const companionReplyLimit = 1900

     // handleCompanion answers /companion where it was typed. Who may run it
     // is who may talk here: admission has already run. It opens no thread
     // and no session.
     func (b *Bridge) handleCompanion(in Inbound, args []string) {
     	text := "the companion is not enabled"
     	if b.commands != nil {
     		out, err := b.commands.CompanionCommand(b.ctx, args)
     		if err != nil {
     			text = "companion: " + err.Error()
     		} else {
     			out = strings.TrimRight(out, "\n")
     			if r := []rune(out); len(r) > companionReplyLimit {
     				out = string(r[:companionReplyLimit]) + "\n…"
     			}
     			text = "```\n" + out + "\n```"
     		}
     	}
     	if _, err := b.client.Send(b.ctx, in.ChannelID, Message{Content: text, Quiet: true}); err != nil {
     		slog.Warn("discord /companion reply failed", "err", err)
     	}
     }
     ```
   - In `Deliver`, change `Message{Content: text}` to `Message{Content: text, Quiet: true}` and extend its comment: "Notes go out quiet: one never needs to ping, and a job's output is model text."

  d. `cmd/spore/wire.go`: in `buildBridge`'s `discord.Options` literal, after `Proposals:   srv,`, add `Commands:    srv,`.

- [ ] **Step 4: Run the gates**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./internal/bridge/discord ./cmd/spore`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/bridge/discord/client.go internal/bridge/discord/companion.go internal/bridge/discord/bridge.go internal/bridge/discord/companion_test.go cmd/spore/wire.go
git commit -m "discord: /companion, and companion sends and notes ping nobody

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 4: TUI — backend methods, pinned row, companion unread

**Files:**
- Modify: `internal/tui/backend.go` (three methods on `Backend`)
- Modify: `cmd/spore/tui_backend.go` (implement them)
- Modify: `internal/tui/app_test.go` (`fakeBackend` implements them; scene + tests)
- Modify: `internal/tui/sidebar.go` (`row.pin`; companion root pinned first; `sessionRow` draws it)
- Modify: `internal/tui/cache.go:192` (a `job_note` marks the companion session unread)
- Modify: `internal/tui/app.go` (`markSeen` accepts `companion`; `eventMsg` marks seen on `job_note` when on screen)
- Test: `internal/tui/sidebar_test.go`, `internal/tui/app_test.go`

**Interfaces:**
- Consumes: `client.companionCommand`, `client.companionProposals`, `client.answerProposal`, `daemon.ProposalJSON` (Task 2).
- Produces (Task 5 relies on these):
  - `Backend.Companion(ctx context.Context, args []string) (string, error)`
  - `Backend.Proposals(ctx context.Context) ([]daemon.ProposalJSON, error)`
  - `Backend.AnswerProposal(ctx context.Context, id int64, choice string) (string, error)`
  - `fakeBackend` fields: `proposals []daemon.ProposalJSON`, `proposalsErr error`, `proposalFetches int`, `answers []string` (each `"<id>:<choice>"`), `companionArgs [][]string`.
  - test helpers `companionScene(t) (*Model, *fakeBackend)` and `runCmd(m *Model, cmd tea.Cmd)`.

- [ ] **Step 1: Add the methods to `Backend`** — in `internal/tui/backend.go`, at the end of the interface:

```go
	// Companion runs one /companion line and returns its text.
	Companion(ctx context.Context, args []string) (string, error)
	// Proposals lists the companion's open proposals, newest first.
	Proposals(ctx context.Context) ([]daemon.ProposalJSON, error)
	// AnswerProposal presses one of a proposal's buttons and returns the
	// line the daemon answers with.
	AnswerProposal(ctx context.Context, id int64, choice string) (string, error)
```

In `cmd/spore/tui_backend.go`:

```go
func (b tuiBackend) Companion(ctx context.Context, args []string) (string, error) {
	return b.c.companionCommand(ctx, args)
}

func (b tuiBackend) Proposals(ctx context.Context) ([]daemon.ProposalJSON, error) {
	return b.c.companionProposals(ctx)
}

func (b tuiBackend) AnswerProposal(ctx context.Context, id int64, choice string) (string, error) {
	return b.c.answerProposal(ctx, id, choice)
}
```

In `internal/tui/app_test.go`, add the fields listed above to `fakeBackend` (a new group after `setErr`), and:

```go
func (f *fakeBackend) Companion(_ context.Context, args []string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.companionArgs = append(f.companionArgs, append([]string(nil), args...))
	return "companion: " + strings.Join(args, " ") + "\n", nil
}

func (f *fakeBackend) Proposals(context.Context) ([]daemon.ProposalJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.proposalFetches++
	return append([]daemon.ProposalJSON(nil), f.proposals...), f.proposalsErr
}

// AnswerProposal answers like the daemon: the proposal leaves the open list.
func (f *fakeBackend) AnswerProposal(_ context.Context, id int64, choice string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, fmt.Sprintf("%d:%s", id, choice))
	for i, p := range f.proposals {
		if p.ID == id {
			f.proposals = append(f.proposals[:i:i], f.proposals[i+1:]...)
			break
		}
	}
	return "answered " + choice, nil
}

// runCmd runs cmd the way the runtime would and feeds back what it returns.
func runCmd(m *Model, cmd tea.Cmd) {
	for _, msg := range expand(cmd) {
		run(m, msg)
	}
}

// companionScene has a chat (selected) and the companion session, unread.
func companionScene(t *testing.T) (*Model, *fakeBackend) {
	t.Helper()
	fb := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "s1", Title: "chat", Source: "chat", Workspace: "/w"},
		{ID: "sp", Title: "spore", Source: "companion", Unread: true},
	}}
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	return m, fb
}
```

- [ ] **Step 2: Write the failing tests**

In `internal/tui/sidebar_test.go`:

```go
func TestTheCompanionSessionIsPinnedFirst(t *testing.T) {
	c := seed(
		daemon.SessionJSON{ID: "c1", Title: "chat", Workspace: "/a", Source: "chat"},
		daemon.SessionJSON{ID: "sp", Title: "spore", Source: "companion"},
	)
	rs := c.rows(false, "", "")
	if len(rs) < 2 || rs[0].id != "sp" || !rs[0].pin {
		t.Fatalf("first row = %+v, want the companion pinned", rs[0])
	}
	if rs[1].key != folderKey {
		t.Fatalf("second row = %+v, want the jobs folder", rs[1])
	}
	for _, r := range rs[1:] {
		if r.id == "sp" {
			t.Fatal("the companion session is listed twice")
		}
	}
	c.get("sp").info.Unread = true
	out := ansi.Strip(renderSidebar(c, c.rows(false, "", ""), "", "", sidebarWidth, 10))
	first := strings.Split(out, "\n")[0]
	if !strings.Contains(first, "◆ spore") || !strings.Contains(first, "new") {
		t.Fatalf("first line = %q, want the pinned row with an unread badge", first)
	}
	if w := ansi.StringWidth(first); w > sidebarWidth {
		t.Fatalf("pinned row is %d cells wide in a %d-cell sidebar", w, sidebarWidth)
	}
	// A filter that does not match it hides it like any other session.
	for _, r := range c.rows(false, "chat", "") {
		if r.pin {
			t.Fatalf("filter 'chat' still shows the pinned row: %+v", r)
		}
	}
}
```

In `internal/tui/app_test.go`:

```go
func TestOpeningTheCompanionSessionMarksItSeen(t *testing.T) {
	m, fb := companionScene(t)
	runCmd(m, m.selectSession("sp"))
	if len(fb.seen) != 1 || fb.seen[0] != "sp" {
		t.Fatalf("seen = %v, want sp marked seen on the daemon", fb.seen)
	}
	if m.cache.get("sp").info.Unread {
		t.Fatal("the companion session still reads unread after it was opened")
	}
}

func TestOutreachWhileAwayMarksTheCompanionUnread(t *testing.T) {
	m, fb := companionScene(t)
	m.cache.get("sp").info.Unread = false
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireJobNote, Text: "Want me to watch ZS?"})
	if !m.cache.get("sp").info.Unread {
		t.Fatal("outreach that arrived while another session was open must read unread")
	}
	if len(fb.seen) != 0 {
		t.Fatalf("seen = %v, want nothing marked while it was not open", fb.seen)
	}
	runCmd(m, m.selectSession("sp"))
	fb.seen = nil
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireJobNote, Text: "Still?"})
	if m.cache.get("sp").info.Unread || len(fb.seen) != 1 {
		t.Fatalf("outreach on screen: unread=%v seen=%v, want it marked seen at once", m.cache.get("sp").info.Unread, fb.seen)
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/tui -run 'TestTheCompanionSessionIsPinnedFirst|TestOpeningTheCompanionSessionMarksItSeen|TestOutreachWhileAwayMarksTheCompanionUnread'`
Expected: FAIL (compile error on `rs[0].pin`).

- [ ] **Step 4: Implement**

  a. `internal/tui/sidebar.go`:
   - In `row`, after `key string`, add:
     ```go
     	// pin marks the companion session's row, drawn first as "◆ spore".
     	pin bool
     ```
   - In `rows`, declare `var roots, jobRuns, pinned []*sessionView`, and in the switch add, before `default:`:
     ```go
     		case sv.info.Source == "companion":
     			// The companion is where spore reaches out unprompted, so it
     			// sits above everything instead of under a workspace.
     			pinned = append(pinned, sv)
     ```
     Add `byRecent(pinned)` beside the other `byRecent` calls. Immediately before `out = append(out, c.jobFolder(...)...)`, add:
     ```go
     	for _, p := range pinned {
     		if !subtree(p) {
     			continue
     		}
     		out = append(out, row{id: p.info.ID, pin: true})
     		addKids(p, 1, filter == "" && visible(p))
     	}
     ```
   - In `sessionRow`, right after the `glyph` switch, add:
     ```go
     	if r.pin {
     		line := glyph + " " + styAccent.Bold(true).Render("◆ spore")
     		if sv.info.Unread {
     			line += " " + styBadge.Render(" new ")
     		}
     		line = clip(line, width)
     		if selected {
     			return fill(line, width, colFillSel)
     		}
     		return line
     	}
     ```
  b. `internal/tui/cache.go`, in `Apply`'s `case daemon.WireJobNote:` add after `sv.add(...)`:
     ```go
     		if sv.info.Source == "companion" {
     			// Outreach: unread until someone opens it; the model clears
     			// it at once when the companion is the session on screen.
     			sv.info.Unread = true
     		}
     ```
  c. `internal/tui/app.go`:
   - `markSeen`: change the guard to
     ```go
     	if !sv.info.Unread || (sv.info.Source != "job" && sv.info.Source != "companion") {
     ```
     and its comment to "markSeen clears a job run's or the companion's unread mark here and on the daemon, so the badges stay right across restarts. Only those are counted, so only they are marked."
   - In `Update`'s `case eventMsg:`, add a case to the `switch msg.ev.Type` after the existing one:
     ```go
     		case daemon.WireJobNote:
     			if id == m.selected {
     				return m.markSeen(id)
     			}
     ```

- [ ] **Step 5: Run the TUI package**

Run: `go test -tags sqlite_fts5 ./internal/tui`
Expected: PASS. If a golden file now differs only because a test scene contains a companion session, inspect the diff; do not run `-update` for any other reason.

- [ ] **Step 6: Run the gates**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./internal/tui ./cmd/spore`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/tui/backend.go cmd/spore/tui_backend.go internal/tui/app_test.go internal/tui/sidebar.go internal/tui/sidebar_test.go internal/tui/cache.go internal/tui/app.go
git commit -m "tui: pin the companion session first with an unread badge

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 5: TUI — proposal bar, `alt+y/n/x`, `/companion`

**Files:**
- Create: `internal/tui/proposal.go` (state, fetch, keys, view)
- Modify: `internal/tui/app.go` (model field; `Update` cases; `selectSession`; `sessionsMsg`; `eventMsg` job_note; alt-key block; `slashLine`; `command`; `commandNames`)
- Modify: `internal/tui/view.go` (`sync` height; `mainView` parts; `helpText`)
- Modify: `internal/tui/header.go` (`keyHints` INSERT; `normalHints`; `approvalHints`)
- Create: `internal/tui/proposal_test.go`
- Golden: `internal/tui/testdata/proposal-bar-120.golden`, `internal/tui/testdata/proposal-approval-first-120.golden` (new); any help golden that changes

**Interfaces:**
- Consumes: Task 4's `Backend` methods, `fakeBackend` fields, `companionScene`, `runCmd`.
- Produces: `Model.proposals []daemon.ProposalJSON`; `proposalsMsg`, `proposalAnsweredMsg`; `(m *Model) fetchProposals(id string) tea.Cmd`, `proposalOpen() bool`, `proposalKey(k string) (tea.Cmd, bool)`, `proposalView() string`, `companion(args []string) tea.Cmd`.

- [ ] **Step 1: Write the failing tests** — create `internal/tui/proposal_test.go`:

```go
package tui

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/daemon"
)

func zsProposal() daemon.ProposalJSON {
	return daemon.ProposalJSON{ID: 4, Text: "You've checked ZS three days running. Want me to watch it at the close on weekdays?", InterestKey: "stock:zs"}
}

// companionOpen selects the companion session with the given proposals open.
func companionOpen(t *testing.T, ps ...daemon.ProposalJSON) (*Model, *fakeBackend) {
	t.Helper()
	m, fb := companionScene(t)
	fb.proposals = ps
	runCmd(m, m.selectSession("sp"))
	return m, fb
}

func TestTheBarShowsTheNewestProposal(t *testing.T) {
	older := daemon.ProposalJSON{ID: 3, Text: "Watch Go releases?", InterestKey: "topic:go"}
	m, _ := companionOpen(t, zsProposal(), older)
	v := m.View()
	for _, want := range []string{"spore proposes", "checked ZS", "alt+y", "yes", "not now", "never", "(+1)"} {
		if !strings.Contains(v, want) {
			t.Errorf("view is missing %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Watch Go releases") {
		t.Error("only the newest proposal is shown")
	}
	golden(t, "proposal-bar-120", v)
}

func TestAltYAnswersTheProposal(t *testing.T) {
	for _, tc := range []struct{ key, choice string }{{"alt+y", "yes"}, {"alt+n", "not-now"}, {"alt+x", "never"}} {
		m, fb := companionOpen(t, zsProposal())
		press(m, "i", tc.key) // from INSERT
		if len(fb.answers) != 1 || fb.answers[0] != "4:"+tc.choice {
			t.Fatalf("%s: answers = %v, want 4:%s", tc.key, fb.answers, tc.choice)
		}
		if len(m.proposals) != 0 || strings.Contains(m.View(), "spore proposes") {
			t.Fatalf("%s: the bar is still up after the answer", tc.key)
		}
		if len(fb.sent) != 0 {
			t.Fatalf("%s: sent %v, want no message", tc.key, fb.sent)
		}
	}
	// And from NORMAL.
	m, fb := companionOpen(t, zsProposal())
	press(m, "alt+y")
	if len(fb.answers) != 1 {
		t.Fatalf("NORMAL alt+y: answers = %v", fb.answers)
	}
}

func TestAnApprovalAnswersBeforeAProposal(t *testing.T) {
	m, fb := companionOpen(t, zsProposal())
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireApproval, PendingID: 9, Tool: "web_fetch", Args: `{"url":"x"}`})
	if strings.Contains(m.View(), "spore proposes") {
		t.Fatal("the proposal bar must hide while an approval waits")
	}
	press(m, "i", "alt+x")
	if len(fb.answers) != 0 || len(fb.resolved) != 0 {
		t.Fatalf("alt+x with an approval up: answers=%v resolved=%v, want nothing", fb.answers, fb.resolved)
	}
	press(m, "alt+y") // still in INSERT: alt+x changed nothing
	if len(fb.answers) != 0 || len(fb.resolved) != 1 {
		t.Fatalf("alt+y with an approval up: answers=%v resolved=%v, want the approval answered", fb.answers, fb.resolved)
	}
}

func TestAnApprovalFirstGolden(t *testing.T) {
	m, _ := companionOpen(t, zsProposal())
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireApproval, PendingID: 9, Tool: "web_fetch", Args: `{"url":"x"}`})
	golden(t, "proposal-approval-first-120", m.View())
}

func TestProposalKeysFallThroughWithNothingOpen(t *testing.T) {
	// In a chat session alt+n is Esc then n: a new session, as before.
	m, fb := companionScene(t)
	fb.created = "s9"
	press(m, "i", "alt+n")
	if len(fb.answers) != 0 {
		t.Fatalf("answers = %v, want none outside the companion", fb.answers)
	}
	if m.selected != "s9" {
		t.Fatalf("selected = %q, want the new session alt+n fell through to", m.selected)
	}
	// In the companion session with nothing open, alt+y answers nothing.
	m, fb = companionOpen(t)
	press(m, "i", "alt+y")
	if len(fb.answers) != 0 {
		t.Fatalf("answers = %v, want none with no open proposal", fb.answers)
	}
}

func TestAJobNoteRefetchesProposals(t *testing.T) {
	m, fb := companionOpen(t, zsProposal())
	before := fb.proposalFetches
	fb.proposals = nil // answered on Discord
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireJobNote, Text: "Not now. I'll ask again in two weeks."})
	if fb.proposalFetches != before+1 {
		t.Fatalf("fetches = %d, want one more after the note", fb.proposalFetches)
	}
	if strings.Contains(m.View(), "spore proposes") {
		t.Fatal("the bar still shows a proposal answered elsewhere")
	}
	// Text streaming in the session does not refetch.
	feed(m, daemon.WireEvent{Session: "sp", Type: daemon.WireText, Text: "hi"})
	if fb.proposalFetches != before+1 {
		t.Fatalf("fetches = %d after a text event, want no refetch", fb.proposalFetches)
	}
}

func TestOtherSessionsNeverFetchProposals(t *testing.T) {
	m, fb := companionScene(t)
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireJobNote, Text: "job ran"})
	if fb.proposalFetches != 0 {
		t.Fatalf("fetches = %d, want none outside the companion", fb.proposalFetches)
	}
}

func TestSlashCompanionShowsTheText(t *testing.T) {
	m, fb := companionScene(t)
	press(m, "i")
	typeText(m, "/companion pause 2h")
	press(m, "enter")
	if len(fb.companionArgs) != 1 || strings.Join(fb.companionArgs[0], " ") != "pause 2h" {
		t.Fatalf("companion args = %q", fb.companionArgs)
	}
	if len(fb.sent) != 0 {
		t.Fatalf("sent %v, want no message", fb.sent)
	}
	if !strings.Contains(m.View(), "companion: pause 2h") {
		t.Fatalf("view is missing the command's text:\n%s", m.View())
	}
}
```

If `m.selected` is not `s9` because `newSessionMsg` does not select the new session, assert instead whatever the existing test for `n` in NORMAL asserts (search app_test.go for `fb.created`).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -tags sqlite_fts5 ./internal/tui -run 'Proposal|AltY|ApprovalAnswersBefore|ApprovalFirst|JobNoteRefetches|SlashCompanion'`
Expected: FAIL to compile — `m.proposals` undefined.

- [ ] **Step 3: Create `internal/tui/proposal.go`**

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// proposalChoices maps the alt keys to a proposal's three buttons.
var proposalChoices = map[string]string{"y": "yes", "n": "not-now", "x": "never"}

// proposalsMsg carries the companion's open proposals for session.
type proposalsMsg struct {
	session string
	list    []daemon.ProposalJSON
	err     error
}

// proposalAnsweredMsg is the daemon's answer to a proposal key.
type proposalAnsweredMsg struct {
	session, text string
	err           error
}

// fetchProposals asks for the open proposals when id is the companion
// session, and clears them otherwise: only the companion session has a bar.
func (m *Model) fetchProposals(id string) tea.Cmd {
	if m.cache.get(id).info.Source != "companion" {
		m.proposals = nil
		return nil
	}
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		list, err := be.Proposals(ctx)
		return proposalsMsg{session: id, list: list, err: err}
	}
}

// proposalOpen reports whether the session on screen has a proposal open.
func (m *Model) proposalOpen() bool {
	return len(m.proposals) > 0 && m.cache.get(m.selected).info.Source == "companion"
}

// proposalKey answers the newest open proposal for alt+y/n/x. ok is false
// when the key is not the proposal's and must fall through as before. An
// approval waiting in the session answers first: alt+y and alt+n fall
// through to it, and alt+x does nothing rather than reach "cancel agent".
func (m *Model) proposalKey(k string) (tea.Cmd, bool) {
	choice, isKey := proposalChoices[k]
	if !isKey || !m.proposalOpen() {
		return nil, false
	}
	if m.waiting() {
		if k == "x" {
			m.flash = "answer the approval first"
			return nil, true
		}
		return nil, false
	}
	p := m.proposals[0]
	m.proposals = m.proposals[1:]
	m.flash = "answering…"
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		text, err := be.AnswerProposal(ctx, p.ID, choice)
		return proposalAnsweredMsg{session: id, text: text, err: err}
	}, true
}

// proposalView is the open proposal docked above the input, or "" when
// none shows: not the companion, nothing open, a view or help on screen, or
// an approval waiting (it answers first). It is not drawn over the
// transcript as the approval card is, because a proposal can stay open for
// days and the conversation about it must stay readable.
func (m *Model) proposalView() string {
	if !m.proposalOpen() || m.table != nil || m.help || m.waiting() {
		return ""
	}
	w := max(10, m.mainWidth()-2)
	inner := max(4, w-2)
	text := strings.Join(strings.Fields(visible(m.proposals[0].Text)), " ")
	lines := strings.Split(lipgloss.NewStyle().Width(inner).Render(text), "\n")
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = ansi.Truncate(strings.TrimRight(lines[1], " "), inner-1, "") + "…"
	}
	keys := styKey.Render("alt+y") + styMuted.Render(" yes   ") +
		styKey.Render("alt+n") + styMuted.Render(" not now   ") +
		styKey.Render("alt+x") + styMuted.Render(" never")
	if n := len(m.proposals) - 1; n > 0 {
		keys += styMuted.Render(fmt.Sprintf("   (+%d)", n))
	}
	rows := append([]string{styAccent.Bold(true).Render("spore proposes")}, lines...)
	rows = append(rows, clip(keys, inner))
	return styProposal.Width(w).Render(strings.Join(rows, "\n"))
}

// proposalHeight is how many rows the bar takes from the transcript.
func (m *Model) proposalHeight() int {
	if v := m.proposalView(); v != "" {
		return lipgloss.Height(v)
	}
	return 0
}

// companion runs /companion [args] and shows its text as a notice.
func (m *Model) companion(args []string) tea.Cmd {
	be, ctx, id := m.be, m.ctx, m.selected
	m.cache.get(id).add(kindNotice, "companion…")
	return func() tea.Msg {
		out, err := be.Companion(ctx, args)
		if err != nil {
			return noticeMsg{session: id, text: "companion failed: " + err.Error(), isErr: true}
		}
		return noticeMsg{session: id, text: strings.TrimRight(out, "\n")}
	}
}
```

In `internal/tui/style.go`, after `styInputIdle`, add:

```go
	// styProposal frames the companion's open proposal above the input.
	styProposal = styInputBox
```

- [ ] **Step 4: Wire it into the model** — in `internal/tui/app.go`:
   - On `Model`, add a field (next to other per-session view state):
     ```go
     	// proposals are the companion's open proposals, newest first, while
     	// the companion session is selected; nil anywhere else.
     	proposals []daemon.ProposalJSON
     ```
   - In `Update`, add two cases (next to `case noticeMsg:`):
     ```go
     	case proposalsMsg:
     		if msg.session != m.selected {
     			return nil
     		}
     		// An error means the companion is off or the daemon refused:
     		// there is nothing to answer, so there is no bar.
     		m.proposals = nil
     		if msg.err == nil {
     			m.proposals = msg.list
     		}
     		return nil

     	case proposalAnsweredMsg:
     		if msg.err != nil {
     			m.flash = "proposal: " + msg.err.Error()
     		} else {
     			m.flash = oneLine(msg.text)
     		}
     		if msg.session != m.selected {
     			return nil
     		}
     		return m.fetchProposals(msg.session)
     ```
   - In `selectSession`, replace the tail (from `seen := m.markSeen(id)` to the end) with:
     ```go
     	m.proposals = nil
     	cmds := []tea.Cmd{m.markSeen(id), m.fetchProposals(id)}
     	if !m.cache.get(id).loaded {
     		cmds = append(cmds, m.loadTranscript(id))
     	}
     	return tea.Batch(cmds...)
     ```
   - In `case sessionsMsg:`, before its final `return nil`, return a fetch when the session on screen is the companion and has no proposals loaded (the TUI can start on it):
     ```go
     		if m.proposals == nil {
     			return m.fetchProposals(m.selected)
     		}
     		return nil
     ```
   - Replace the `case daemon.WireJobNote:` that Task 4 added in `eventMsg` with:
     ```go
     		case daemon.WireJobNote:
     			// Outreach arrives and answers are posted as notes, so a note
     			// is when the open proposals may have changed.
     			if id == m.selected {
     				return tea.Batch(m.markSeen(id), m.fetchProposals(id))
     			}
     ```
   - In the alt-key block (the `if k.Alt && k.Type == tea.KeyRunes && ...` in the key handler), insert right after the closing brace of the `if m.mode == modeInsert && m.table == nil { ... }` approval block and before `m.mode = modeNormal`:
     ```go
     		// alt+y/n/x answer the companion's open proposal; an approval
     		// above has already had its turn at alt+y/n.
     		if m.table == nil {
     			if cmd, ok := m.proposalKey(string(k.Runes)); ok {
     				return cmd
     			}
     		}
     ```
   - In `slashLine`, add `case "companion": return m.companion(fields[1:])` to the switch. In `command`, add `case "companion": return m.companion(args)` beside `case "refine":`.
   - In `commandNames`, insert `"companion"` after `"compact"`.

- [ ] **Step 5: Lay it out** — in `internal/tui/view.go`:
   - In `sync`, change the viewport height line to:
     ```go
     	m.vp.Height = max(1, m.paneHeight()-1-lipgloss.Height(m.inputView())-m.draftLineHeight()-m.proposalHeight())
     ```
   - In `mainView`, replace `parts := []string{clip(m.sessionFacts(), m.mainWidth()), vpView, m.inputView()}` with:
     ```go
     	parts := []string{clip(m.sessionFacts(), m.mainWidth()), vpView}
     	if pv := m.proposalView(); pv != "" {
     		parts = append(parts, pv)
     	}
     	parts = append(parts, m.inputView())
     ```
   - In `helpText`, after the APPROVAL block's last line (`"  n answers the approval, not \"new session\", until it is answered",`) and its following `"",`, add:
     ```go
     		styKey.Render("COMPANION") + "  (in the spore session, while a proposal shows)",
     		"  alt+y watch it · alt+n not now · alt+x never · an approval waiting answers first",
     		"  /companion [status | interests | outreach | pause [dur] | resume | answer <id> yes|not-now|never]",
     		"",
     ```
     and add `:companion` to the COMMANDS line after `:context`.

- [ ] **Step 6: Hints** — in `internal/tui/header.go`:
   - `keyHints`, `case modeInsert:` — after the `if m.waiting() {...}` block add:
     ```go
     		if !m.waiting() && m.proposalOpen() {
     			parts = append(parts, hint("alt+y/n/x", "proposal"))
     		}
     ```
   - `normalHints` — in the chat-pane path, right after `var parts []string`, add:
     ```go
     	if m.proposalOpen() {
     		parts = append(parts, hint("alt+y/n/x", "proposal"))
     	}
     ```
   - `approvalHints` — before the next-blocked hint, add:
     ```go
     	if m.proposalOpen() {
     		parts = append(parts, styMuted.Render("proposal after the approval"))
     	}
     ```

- [ ] **Step 7: Run the tests, create the two goldens**

Run: `go test -tags sqlite_fts5 ./internal/tui -run 'TestTheBarShowsTheNewestProposal|TestAnApprovalFirstGolden' -update`
Then open both new files in `internal/tui/testdata/` and check by eye: `proposal-bar-120` shows the bar between the transcript and the input with the text on at most two lines and `(+1)`; `proposal-approval-first-120` shows the approval card and no proposal bar.

Run: `go test -tags sqlite_fts5 ./internal/tui`
Expected: PASS. If a help golden fails, the only acceptable diff is the COMPANION lines and `:companion`; then rerun that one test with `-update`. Any other golden diff is a bug in this task — fix the code, not the golden.

- [ ] **Step 8: Run the gates**

Run: `go build -tags sqlite_fts5 ./... && go vet -tags sqlite_fts5 ./... && go test -tags sqlite_fts5 ./internal/tui ./cmd/spore`
Expected: PASS

- [ ] **Step 9: Commit**

```bash
git add internal/tui/proposal.go internal/tui/proposal_test.go internal/tui/app.go internal/tui/view.go internal/tui/header.go internal/tui/style.go internal/tui/testdata/
git commit -m "tui: proposal bar with alt+y/n/x, and /companion

An approval waiting in the companion session answers first. The bar is
docked above the input rather than drawn over the transcript.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 6: Web — pin, unread, proposal bar, `/companion`

**Files:**
- Modify: `web/index.html:47` (add `<div id="proposals"></div>` right after `<div id="approvals"></div>`)
- Modify: `web/app.js`
- Modify: `web/style.css` (after the `.approval` rules)
- Test: `internal/daemon/web_test.go` (`TestAppJSRoutesAreRegistered` covers the new routes automatically; add one test below)

**Interfaces:**
- Consumes: `GET /api/companion/proposals`, `POST /api/companion/proposals/{id}`, `POST /api/companion/command` (Task 2).

- [ ] **Step 1: Write the failing test** — append to `internal/daemon/web_test.go` (match the file's way of reading the embedded `app.js`; `TestModelPanelIsWired` does this):

```go
// The companion's web surfaces are wired: the bar, its routes and the
// /companion command.
func TestCompanionWebIsWired(t *testing.T) {
	js := readAsset(t, "app.js")
	for _, want := range []string{
		`"/api/companion/proposals"`, `"/api/companion/proposals/{id}"`, `"/api/companion/command"`,
		"function renderProposals", `"/companion"`, `s.source === "companion"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js is missing %s", want)
		}
	}
	if html := readAsset(t, "index.html"); !strings.Contains(html, `id="proposals"`) {
		t.Error(`index.html has no id="proposals" slot`)
	}
}
```

If `web_test.go` has no `readAsset` helper, write one in the same style as the code `TestModelPanelIsWired` uses to read `app.js` (do not invent a new loading mechanism).

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -tags sqlite_fts5 ./internal/daemon -run TestCompanionWebIsWired`
Expected: FAIL — app.js is missing the strings.

- [ ] **Step 3: Implement** — `web/app.js`:

  a. Unread (in `loadSessions`), replace the two lines
  ```js
      // Only a job run is "unread", as in the TUI: nobody watched it happen.
      unread: !!s.unread && s.source === "job",
  ```
  with
  ```js
      // A job run or the companion's outreach is "unread", as in the TUI:
      // nobody watched it arrive.
      unread: !!s.unread && (s.source === "job" || s.source === "companion"),
  ```

  b. Pin (in `renderSidebar`): replace `for (const r of roots.filter(visible)) {` with
  ```js
  for (const r of roots.filter((r) => r.source !== "companion" && visible(r))) {
  ```
  and, right after `S.order = [];` … the `const row = (r, depth) => {…};` definition, before `for (const [ws, list] of ordered) {`, add:
  ```js
    // The companion is where spore reaches out unprompted: pinned above the
    // workspaces, as in the TUI.
    for (const r of roots.filter((r) => r.source === "companion" && visible(r))) {
      nav.appendChild(h("div", { class: "group pinned" }, h("h2", { text: "spore" })));
      row(r, 0);
    }
  ```

  c. Proposals. Add `proposals: []` to the `S` state object. Add a new section after `answer(...)` (the approvals section):
  ```js
  // ---------- companion proposals ----------

  // companionOpen reports whether the open session is the companion's.
  function companionOpen() {
    const r = S.open && S.sessions.get(S.open);
    return !!r && r.source === "companion";
  }

  // loadProposals fetches the open proposals for the companion session; any
  // other session has none. A failure (the companion is off) shows no bar.
  async function loadProposals() {
    const sid = S.open;
    if (!companionOpen()) {
      S.proposals = [];
      renderProposals();
      return;
    }
    let list = [];
    try {
      list = (await api("GET", "/api/companion/proposals", {})) || [];
    } catch (err) {
      list = [];
    }
    if (S.open !== sid) return;
    S.proposals = list;
    renderProposals();
  }

  // renderProposals draws the newest open proposal above the composer. An
  // approval waiting in the session answers first, so the bar hides.
  function renderProposals() {
    const host = el("proposals");
    host.textContent = "";
    const r = S.open && S.sessions.get(S.open);
    if (!companionOpen() || !S.proposals.length || (r && r.approvals.length)) return;
    const p = S.proposals[0];
    const card = h("div", { class: "proposal", role: "region", "aria-label": "spore proposes" });
    card.appendChild(h("h3", { text: "spore proposes" }));
    card.appendChild(h("div", { class: "text", text: p.text }));
    const buttons = h("div", { class: "buttons" });
    for (const [label, choice, cls] of [["Yes, watch it", "yes", "allow"], ["Not now", "not-now", ""], ["Never", "never", "deny"]]) {
      buttons.appendChild(h("button", { type: "button", class: cls, onclick: () => answerProposal(p, choice, card) }, label));
    }
    if (S.proposals.length > 1) {
      buttons.appendChild(h("span", { class: "more", text: "+" + (S.proposals.length - 1) + " more" }));
    }
    card.appendChild(buttons);
    host.appendChild(card);
  }

  async function answerProposal(p, choice, card) {
    for (const b of card.querySelectorAll("button")) b.disabled = true;
    try {
      await api("POST", "/api/companion/proposals/{id}", { id: p.id }, { choice });
    } catch (err) {
      addLine("msg error", "could not answer the proposal: " + err.message);
    }
    await loadProposals();
  }
  ```
  At the end of `renderApprovals()`, after `tickCountdowns();`, add `renderProposals();` (an approval arriving or resolving changes whether the bar shows).

  d. Refetch points:
   - `selectSession`: after `el("approvals").textContent = "";` add `el("proposals").textContent = ""; S.proposals = [];`. Replace `await loadTranscript(id, gen);` with
     ```js
       await loadTranscript(id, gen);
       if (S.generation === gen) await loadProposals();
     ```
   - Feed handler, `case "job_note":` — replace its body with:
     ```js
         if (isOpen) {
           addNote(ev.text);
           // Outreach arrives and answers are posted as notes: the open
           // proposals may have changed, and the note has been seen.
           if (r.source === "companion") {
             loadProposals();
             api("POST", "/api/sessions/{id}/seen", { id }).catch(() => {});
           }
         } else if (r.source === "companion") {
           r.unread = true;
         }
         break;
     ```
     (Check the handler's local names: it uses `r`, `id` and `isOpen` in the neighbouring cases; use the same.)
   - `case "session_deleted":` inside `if (isOpen) {…}` add `el("proposals").textContent = "";`.

  e. `/companion`: below `const slashCommands = …`, add
  ```js
  // runCompanion runs /companion [args] in the daemon and shows its text.
  async function runCompanion(text) {
    const args = text.split(/\s+/).slice(1).filter(Boolean);
    const res = await api("POST", "/api/companion/command", {}, { args });
    addLine("msg note usage", ((res && res.text) || "").replace(/\n+$/, ""));
  }
  ```
  In `send()`, replace `const cmd = slashCommands[text];` with
  ```js
    const cmd = slashCommands[text] ||
      (text === "/companion" || text.startsWith("/companion ") ? () => runCompanion(text) : null);
  ```
  In `SHORTCUTS`, after the `/model` row, add
  ```js
    ["/companion", "status, interests, outreach, pause, resume, answer (type it in the composer)"],
  ```

  f. `web/style.css`, after the `.approval .fine` rule:
  ```css
  #proposals { padding: 0 16px; }
  .proposal {
    max-width: 780px; margin: 8px auto;
    border: 1px solid var(--accent); border-radius: 6px; background: var(--panel);
    padding: 12px;
  }
  .proposal h3 { margin: 0 0 4px; font-size: 14px; color: var(--accent); }
  .proposal .text { white-space: pre-wrap; overflow-wrap: anywhere; }
  .proposal .buttons { display: flex; gap: 6px; margin-top: 8px; flex-wrap: wrap; align-items: center; }
  .proposal .buttons .allow { border-color: var(--accent); }
  .proposal .buttons .deny { border-color: var(--error); color: var(--error); }
  .proposal .more { color: var(--dim); font-size: 12px; }
  ```

- [ ] **Step 4: Run the web tests**

Run: `go test -tags sqlite_fts5 ./internal/daemon -run 'Web|AppJS|Index|Static|Secure|Companion'`
Expected: PASS. `TestAppJSRoutesAreRegistered` must pass: every `api(...)` route in app.js is registered.

Also run `node --check web/app.js` if `node` is installed; it must print nothing.

- [ ] **Step 5: Commit**

```bash
git add web/index.html web/app.js web/style.css internal/daemon/web_test.go
git commit -m "web: pin the companion session, proposal bar, /companion

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

### Task 7: Docs, backlog, spec as-built, full gate

**Files:**
- Modify: `README.md` (Companion section, around lines 1095–1190)
- Modify: `docs/backlog.md` (new entry before `## Companion: enhancements left out of the first design`)
- Modify: `docs/superpowers/specs/2026-10-10-companion-design.md` (append "As built — PR 2b")

- [ ] **Step 1: README** — in the Companion section:
  - Replace the sentence "The pinned TUI row, the web list and the `/companion` chat command come in a later release." with "Its session is pinned at the top of the TUI and web session lists as **spore**, and `/companion` works in every chat."
  - Replace "Without a channel id, the companion's messages appear in its own `spore` session in the TUI and web UI, and `spore companion answer <id> yes` answers a proposal." with: "Without a channel id, the companion's messages appear in its pinned `spore` session in the TUI and web UI. An open proposal shows above the input: press `alt+y` (watch it), `alt+n` (not now) or `alt+x` (never) in the TUI, or the buttons in the web UI. A tool approval waiting in that session is answered first. `spore companion answer <id> yes` works from any terminal."
  - After the `spore companion check` block, add a short paragraph: "In any chat (TUI, web, Discord, plain `spore chat`), `/companion` runs the same subcommands as `spore companion` except `check`: `/companion`, `/companion interests`, `/companion pause 2h`, `/companion answer 4 never`."
- [ ] **Step 2: Backlog** — add:

```markdown
## Discord mentions outside the companion

Known gap. Since PR 2b, companion sends, notes, `/companion` replies and
button answers go out with `allowed_mentions: {parse: []}`, so model text
there pings nobody. Turn output (the streamed reply), approval cards and
`/usage` still use Discord's default, so a reply containing `@everyone`
in a channel where the bot may mention everyone would ping. Open question:
set `Quiet` on every send, or only on model text (the renderer's messages)?
```

- [ ] **Step 3: Spec as-built** — append to the spec:

```markdown
## As built — PR 2b

- `Bridge.Deliver` sends every note quiet, not only the companion's: it
  cannot tell which session is the companion's, and a note never needs to
  ping. Button answers (`Respond`) are quiet too.
- The TUI bar is docked above the input and shrinks the transcript by its
  height; the approval card is still drawn over the transcript.
- Clients refetch proposals on open and on `job_note` in the companion
  session; a `job_note` there also marks it unread unless it is on screen.
```

- [ ] **Step 4: Full CI gate** — run every CI step, not only `make test`:

Run: `make vet fmtcheck lint vulncheck tidycheck test`
Expected: every step exits 0. If `vulncheck` reports a new advisory unrelated to this branch, stop and report it rather than changing dependencies.

- [ ] **Step 5: Commit**

```bash
git add README.md docs/backlog.md docs/superpowers/specs/2026-10-10-companion-design.md
git commit -m "docs: companion 2b in the README, backlog and spec

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01AqymXx5oZ2M7c2UcE8eWv4"
```

---

## Manual gate (after Task 7, by the orchestrator, not a worker)

Isolated daemon only — never a second daemon on the user's Discord bot token.

1. `go build -tags sqlite_fts5 -o /tmp/claude-scratch/spore ./cmd/spore`; run it with `HOME=<scratch>`, `daemon.addr = 127.0.0.1:7788`, a real model, `companion.enabled = true`, no `[bridge.discord]`.
2. Seed an interest by chatting about one topic, then `spore companion check --promote <key> --force` → a proposal is delivered.
3. TUI: the pinned `◆ spore` row shows `new`; opening it clears it; the bar shows; `alt+y` creates a watch (`spore companion status` / jobs view), the bar clears.
4. Second proposal; web UI (Playwright): the pinned row and bar render; "Not now" clears the bar; the TUI's bar clears on the note without a key press.
5. `/companion status` and `/companion pause 1h` in both the TUI and the web return the CLI's text; `/companion check` gives the refusal.
6. Discord: the user verifies live on their own daemon.
