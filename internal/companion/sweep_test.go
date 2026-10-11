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
