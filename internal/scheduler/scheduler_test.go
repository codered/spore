package scheduler

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

type fakeRunner struct {
	mu    sync.Mutex
	calls []store.Job
	next  int
	err   error
}

func (f *fakeRunner) StartJob(ctx context.Context, j store.Job) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.calls = append(f.calls, j)
	f.next++
	return "sess-" + string(rune('a'+f.next-1)), nil
}

func (f *fakeRunner) started() []store.Job {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.Job(nil), f.calls...)
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTickFiresADueCronJobAndReschedulesIt(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)

	id, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing",
		Enabled: true, NextRun: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	started := r.started()
	if len(started) != 1 || started[0].ID != id {
		t.Fatalf("started = %+v, want exactly job %d", started, id)
	}

	jobs, _ := st.ListJobs(ctx)
	want := time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	if !jobs[0].NextRun.Equal(want) {
		t.Errorf("next_run = %v, want %v", jobs[0].NextRun, want)
	}
	if jobs[0].LastSessionID == "" {
		t.Error("last_session_id was not recorded")
	}
	if !jobs[0].Enabled {
		t.Error("a cron job was disabled after firing")
	}

	// A second tick at the same instant must do nothing: next_run has moved.
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Errorf("job fired %d times on two ticks, want 1", len(got))
	}
}

func TestAMissedRunFiresOnceAndIsNotBackfilled(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// The daemon was down for eight days; a daily job has eight missed slots.
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "daily",
		Enabled: true, NextRun: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Fatalf("a job missed 8 times started %d turns, want exactly 1", len(got))
	}
	jobs, _ := st.ListJobs(ctx)
	want := time.Date(2026, 9, 10, 9, 0, 0, 0, time.UTC)
	if !jobs[0].NextRun.Equal(want) {
		t.Errorf("next_run = %v, want the next FUTURE slot %v", jobs[0].NextRun, want)
	}
}

func TestAOneShotJobIsDisabledAfterItFires(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "once", Spec: "2026-09-01T09:00:00Z", Prompt: "one time",
		Enabled: true, NextRun: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	jobs, _ := st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("a one-shot job is still enabled after firing")
	}
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	if got := r.started(); len(got) != 1 {
		t.Errorf("one-shot job fired %d times, want 1", len(got))
	}
}

func TestAJobWithAnUnparseableSpecIsDisabledNotRetriedForever(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	if _, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "not a schedule", Prompt: "broken",
		Enabled: true, NextRun: now.Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	r := &fakeRunner{}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick returned an error for one bad job: %v", err)
	}
	if got := r.started(); len(got) != 0 {
		t.Errorf("a job with an invalid spec was started: %+v", got)
	}
	jobs, _ := st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("a job with an unparseable spec is still enabled and will be retried every tick")
	}
}

func TestOneFailingJobDoesNotStopTheOthers(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)
	for _, prompt := range []string{"first", "second"} {
		if _, err := st.CreateJob(ctx, store.Job{
			Kind: "cron", Spec: "0 9 * * *", Prompt: prompt,
			Enabled: true, NextRun: now.Add(-time.Minute),
		}); err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
	}
	r := &fakeRunner{err: context.DeadlineExceeded}
	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick propagated a job failure: %v", err)
	}
	// Both jobs must still have been advanced, so a runner that is briefly
	// unavailable does not leave every job permanently due.
	jobs, _ := st.ListJobs(ctx)
	for _, j := range jobs {
		if !j.NextRun.After(now) {
			t.Errorf("job %d was left due after a failed start (next_run %v)", j.ID, j.NextRun)
		}
	}
}
