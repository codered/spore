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
