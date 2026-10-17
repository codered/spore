package store

import (
	"context"
	"testing"
	"time"
)

func TestCompanionRunRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	finished := started.Add(3 * time.Minute)
	watch := CompanionRun{InterestID: in.ID, Kind: RunWatch, JobID: 11, SessionID: "s-watch",
		Verdict: VerdictAlert, StartedAt: started, FinishedAt: finished}
	watchID, err := st.AddCompanionRun(ctx, watch)
	if err != nil {
		t.Fatal(err)
	}
	research := CompanionRun{InterestID: in.ID, Kind: RunResearch, JobID: 12, Question: "q",
		Verdict: VerdictPending, StartedAt: started.Add(time.Hour)}
	researchID, err := st.AddCompanionRun(ctx, research)
	if err != nil {
		t.Fatal(err)
	}

	runs, err := st.CompanionRuns(ctx, started.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != researchID || runs[1].ID != watchID {
		t.Fatalf("runs = %+v, want research then watch", runs)
	}
	got := runs[1]
	watch.ID = watchID
	if got.ID != watch.ID || got.InterestID != watch.InterestID || got.Kind != watch.Kind ||
		got.JobID != watch.JobID || got.SessionID != watch.SessionID || got.Question != watch.Question ||
		got.Verdict != watch.Verdict || !got.StartedAt.Equal(watch.StartedAt) ||
		!got.FinishedAt.Equal(watch.FinishedAt) || got.ReportedOutreachID != 0 || !got.SeenAt.IsZero() {
		t.Fatalf("watch row = %+v, want %+v", got, watch)
	}
	if got := runs[0]; got.Question != "q" || got.Verdict != VerdictPending || !got.FinishedAt.IsZero() {
		t.Fatalf("research row = %+v", got)
	}

	r, ok, err := st.CompanionRunByJob(ctx, 12, RunResearch)
	if err != nil || !ok || r.ID != researchID {
		t.Fatalf("CompanionRunByJob research = %+v %v %v", r, ok, err)
	}
	if _, ok, err := st.CompanionRunByJob(ctx, 99, RunWatch); err != nil || ok {
		t.Fatalf("unknown job: %v %v", ok, err)
	}
}

func TestFinishCompanionRunOnlyFromPending(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	id, err := st.AddCompanionRun(ctx, CompanionRun{InterestID: in.ID, Kind: RunResearch, JobID: 5,
		Verdict: VerdictPending, StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := st.FinishCompanionRun(ctx, id, VerdictDone, at.Add(time.Minute))
	if err != nil || !moved {
		t.Fatalf("pending -> done: %v %v", moved, err)
	}
	r, _, _ := st.CompanionRunByJob(ctx, 5, RunResearch)
	if r.Verdict != VerdictDone || !r.FinishedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("finished = %+v", r)
	}
	moved, err = st.FinishCompanionRun(ctx, id, VerdictFailed, at.Add(2*time.Minute))
	if err != nil || moved {
		t.Fatalf("second finish moved it: %v %v", moved, err)
	}
	if r, _, _ := st.CompanionRunByJob(ctx, 5, RunResearch); r.Verdict != VerdictDone {
		t.Fatalf("verdict = %q, want done", r.Verdict)
	}
}

func TestReportedAndSeenOnlyOnce(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 17, 14, 0, 0, 0, time.UTC)
	first, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachAlert, Text: "x", SentAt: at})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachAlert, Text: "y", SentAt: at})
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.AddCompanionRun(ctx, CompanionRun{InterestID: in.ID, Kind: RunWatch, JobID: 1, Verdict: VerdictAlert, StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.AddCompanionRun(ctx, CompanionRun{InterestID: in.ID, Kind: RunWatch, JobID: 2, Verdict: VerdictNothing, StartedAt: at})
	if err != nil {
		t.Fatal(err)
	}

	if err := st.SetCompanionRunsReported(ctx, []int64{a, b}, first); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCompanionRunsReported(ctx, []int64{a}, second); err != nil {
		t.Fatal(err)
	}
	runs, err := st.CompanionRuns(ctx, at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.ReportedOutreachID != first {
			t.Fatalf("run %d reported = %d, want %d", r.ID, r.ReportedOutreachID, first)
		}
	}

	seen := at.Add(time.Hour)
	if err := st.SetCompanionRunsSeen(ctx, []int64{a}, seen); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCompanionRunsSeen(ctx, []int64{a}, seen.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r, _, _ := st.CompanionRunByJob(ctx, 1, RunWatch); !r.SeenAt.Equal(seen) {
		t.Fatalf("seen = %v, want %v", r.SeenAt, seen)
	}

	if err := st.SetCompanionRunsReported(ctx, nil, second); err != nil {
		t.Fatalf("empty reported ids: %v", err)
	}
	if err := st.SetCompanionRunsSeen(ctx, []int64{}, at); err != nil {
		t.Fatalf("empty seen ids: %v", err)
	}
}

func TestCompanionRunsCascadeAndSweep(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	old := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cut := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	if _, err := st.AddCompanionRun(ctx, CompanionRun{InterestID: in.ID, Kind: RunWatch, JobID: 1, Verdict: VerdictNothing, StartedAt: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddCompanionRun(ctx, CompanionRun{InterestID: in.ID, Kind: RunWatch, JobID: 2, Verdict: VerdictNothing, StartedAt: cut.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	n, err := st.DeleteCompanionRunsBefore(ctx, cut)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d %v, want 1", n, err)
	}
	runs, err := st.CompanionRuns(ctx, time.Time{})
	if err != nil || len(runs) != 1 || runs[0].JobID != 2 {
		t.Fatalf("after sweep = %+v %v", runs, err)
	}

	if _, err := st.DB().Exec(`DELETE FROM interests WHERE id = ?`, in.ID); err != nil {
		t.Fatal(err)
	}
	runs, err = st.CompanionRuns(ctx, time.Time{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("after interest delete = %+v %v", runs, err)
	}
}
