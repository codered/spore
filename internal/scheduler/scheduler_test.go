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

// TestAdvanceIsDurableBeforeStarting verifies that a job's next_run is
// persisted to the database BEFORE StartJob is called. This is the crash-safety
// invariant: if the process dies between the advance and the start, next_run
// is already in the future, so on restart the job is not due and does not
// duplicate. This test uses a runner that inspects the database state when
// StartJob is called to prove the advance has already been written.
func TestAdvanceIsDurableBeforeStarting(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)

	_, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "test advance durability",
		Enabled: true, NextRun: now.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Create a runner that asserts the job's next_run has moved at call time
	r := &assertingRunner{
		st:  st,
		now: now,
		t:   t,
	}

	s := New(st, r, func() time.Time { return now })
	if err := s.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The runner should have been called and should have passed its assertion
	if !r.called {
		t.Fatal("StartJob was never called")
	}
}

// assertingRunner is a test runner that asserts the job's next_run has already
// moved past 'now' when StartJob is called. This proves the advance is durable
// before the side effect.
type assertingRunner struct {
	st     *store.Store
	now    time.Time
	t      *testing.T
	called bool
}

func (a *assertingRunner) StartJob(ctx context.Context, job store.Job) (string, error) {
	a.called = true
	// Read the job back from the store to check its next_run
	jobs, err := a.st.ListJobs(ctx)
	if err != nil {
		a.t.Fatalf("ListJobs in StartJob: %v", err)
	}
	for _, j := range jobs {
		if j.ID != job.ID {
			continue
		}
		// The critical assertion: next_run must be strictly after now
		if !j.NextRun.After(a.now) {
			a.t.Fatalf("at StartJob call time, job %d next_run is %v, want strictly after now (%v). Advance was not durable before start.", job.ID, j.NextRun, a.now)
		}
		return "sess-check", nil
	}
	a.t.Fatalf("job %d not found in database when StartJob was called", job.ID)
	return "", nil
}

type panicRunner struct {
	inner   *fakeRunner
	panicOn int64
}

func (p *panicRunner) StartJob(ctx context.Context, j store.Job) (string, error) {
	if j.ID == p.panicOn {
		panic("intentional panic for job")
	}
	return p.inner.StartJob(ctx, j)
}

func TestAPanicInOneJobDoesNotAbortTheRest(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 9, 1, 9, 30, 0, 0, time.UTC)

	// Create two jobs, both due at this tick.
	id1, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "will panic",
		Enabled: true, NextRun: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob 1: %v", err)
	}

	id2, err := st.CreateJob(ctx, store.Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "will run",
		Enabled: true, NextRun: now.Add(-30 * time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}

	// A runner that panics on the first job and succeeds on the second.
	inner := &fakeRunner{next: 0}
	runner := &panicRunner{inner: inner, panicOn: id1}

	s := New(st, runner, func() time.Time { return now })
	err = s.Tick(ctx) // Should not panic or error
	if err != nil {
		t.Fatalf("Tick should not error when a job panics: %v", err)
	}

	// The first job panics in the runner (before reaching StartJob), but the
	// second job should still be attempted and started by the inner runner.
	started := inner.started()
	if len(started) != 1 {
		t.Fatalf("inner.started() = %d jobs, want 1 (only the second, since the first panicked before StartJob)", len(started))
	}

	// Verify that job 2 was actually started and recorded
	jobs, _ := st.ListJobs(ctx)
	for _, j := range jobs {
		if j.ID == id2 {
			if j.LastSessionID == "" {
				t.Error("job 2 was not recorded as started")
			}
		}
	}
}
