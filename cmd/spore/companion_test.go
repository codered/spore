package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
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
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "KEY") {
		t.Fatalf("want a header and one row, got:\n%s", out.String())
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
