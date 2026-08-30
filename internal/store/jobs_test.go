package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestJobCRUDAndDueSelection(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	due, err := s.CreateJob(ctx, Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "summarise yesterday",
		Enabled: true, NextRun: base.Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	later, err := s.CreateJob(ctx, Job{
		Kind: "once", Spec: "2026-12-25T09:00:00Z", Prompt: "wrap presents",
		Enabled: true, NextRun: base.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	off, err := s.CreateJob(ctx, Job{
		Kind: "cron", Spec: "*/5 * * * *", Prompt: "disabled one",
		Enabled: false, NextRun: base.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	jobs, err := s.ListJobs(ctx)
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("ListJobs returned %d jobs, want 3", len(jobs))
	}

	got, err := s.DueJobs(ctx, base)
	if err != nil {
		t.Fatalf("DueJobs: %v", err)
	}
	if len(got) != 1 || got[0].ID != due {
		t.Fatalf("DueJobs = %+v, want only job %d (past next_run, enabled)", got, due)
	}
	if got[0].Prompt != "summarise yesterday" || got[0].Kind != "cron" {
		t.Errorf("due job round-tripped as %+v", got[0])
	}
	_ = later
	_ = off

	// Marking a run advances next_run and records the session the job opened,
	// so the same job is not picked up again on the next tick.
	next := base.Add(24 * time.Hour)
	if err := s.MarkJobRun(ctx, due, base, next, "sess-abc"); err != nil {
		t.Fatalf("MarkJobRun: %v", err)
	}
	if got, err = s.DueJobs(ctx, base); err != nil || len(got) != 0 {
		t.Fatalf("DueJobs after MarkJobRun = %+v (err %v), want none", got, err)
	}
	jobs, _ = s.ListJobs(ctx)
	for _, j := range jobs {
		if j.ID != due {
			continue
		}
		if !j.NextRun.Equal(next) {
			t.Errorf("next_run = %v, want %v", j.NextRun, next)
		}
		if j.LastSessionID != "sess-abc" {
			t.Errorf("last_session_id = %q, want sess-abc", j.LastSessionID)
		}
	}

	// Disabling a job must remove it from DueJobs. The "later" job (due at
	// base+1h) is our test: disable it, then query at a time past its next_run
	// and assert it's gone. The only reason it's absent is the disabling.
	if err := s.SetJobEnabled(ctx, later, false); err != nil {
		t.Fatalf("SetJobEnabled: %v", err)
	}
	// At base+2h, the later job's next_run (base+1h) is in the past,
	// but it's disabled so DueJobs must exclude it.
	if got, err := s.DueJobs(ctx, base.Add(2*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("disabled job came back due: %+v (err %v)", got, err)
	}
	// Verify the disable actually happened via ListJobs
	jobs, _ = s.ListJobs(ctx)
	for _, j := range jobs {
		if j.ID != later {
			continue
		}
		if j.Enabled {
			t.Errorf("later job still enabled after SetJobEnabled(false)")
		}
	}
}

// MarkJobRun with a zero next time retires a one-shot job: disables it and
// records the run. It must not be picked up again by DueJobs.
func TestMarkJobRunRetires(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	base := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)

	// Create a one-time job
	id, err := s.CreateJob(ctx, Job{
		Kind: "once", Spec: "2026-12-25T09:00:00Z", Prompt: "one-time task",
		Enabled: true, NextRun: base,
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// Retire it (zero next time disables the job)
	if err := s.MarkJobRun(ctx, id, base, time.Time{}, "sess-xyz"); err != nil {
		t.Fatalf("MarkJobRun retire: %v", err)
	}

	// Verify via ListJobs that it's disabled and the run was recorded
	jobs, _ := s.ListJobs(ctx)
	for _, j := range jobs {
		if j.ID != id {
			continue
		}
		if j.Enabled {
			t.Errorf("retired job still enabled")
		}
		if j.LastSessionID != "sess-xyz" {
			t.Errorf("last_session_id = %q, want sess-xyz", j.LastSessionID)
		}
		if j.LastRun.IsZero() {
			t.Errorf("last_run not recorded on retire")
		}
	}

	// Verify via DueJobs that it never comes back, even if queried way in the future
	if got, err := s.DueJobs(ctx, base.Add(365*24*time.Hour)); err != nil || len(got) != 0 {
		t.Fatalf("retired job came back due: %+v (err %v)", got, err)
	}
}

// The Plan 2 schema shipped an unused jobs table with a different shape.
// Opening a database that still has it must produce the new shape rather
// than failing on a missing column.
func TestOpenMigratesThePlan2JobsStub(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spore.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec(`DROP TABLE jobs`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := s.db.Exec(`CREATE TABLE jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		schedule TEXT NOT NULL, prompt TEXT NOT NULL, session_id TEXT,
		enabled INTEGER NOT NULL DEFAULT 1, last_run TEXT, created_at TEXT NOT NULL)`); err != nil {
		t.Fatalf("recreate stub: %v", err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen over the stub: %v", err)
	}
	jobID, err := s2.CreateJob(context.Background(), Job{
		Kind: "cron", Spec: "0 9 * * *", Prompt: "after migration",
		Enabled: true, NextRun: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("CreateJob after migration: %v", err)
	}
	s2.Close()

	// Verify idempotence: re-opening the database must not destroy existing jobs.
	// The migration guard runs on every Open, so this is critical: a faulty guard
	// would delete the user's jobs on daemon restart.
	s3, err := Open(path)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	jobs3, err := s3.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs after second reopen: %v", err)
	}
	if len(jobs3) != 1 || jobs3[0].ID != jobID {
		t.Fatalf("job lost after second Open: got %d jobs, want 1", len(jobs3))
	}
	s3.Close()

	// Third open to be thorough
	s4, err := Open(path)
	if err != nil {
		t.Fatalf("third reopen: %v", err)
	}
	jobs4, err := s4.ListJobs(context.Background())
	if err != nil {
		t.Fatalf("ListJobs after third reopen: %v", err)
	}
	if len(jobs4) != 1 || jobs4[0].ID != jobID {
		t.Fatalf("job lost after third Open: got %d jobs, want 1", len(jobs4))
	}
	s4.Close()
}
