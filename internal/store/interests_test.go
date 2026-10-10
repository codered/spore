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
