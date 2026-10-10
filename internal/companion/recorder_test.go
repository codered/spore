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
