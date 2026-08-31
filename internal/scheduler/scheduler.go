package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/store"
)

var (
	ErrPromptRequired = errors.New("prompt is required")
	ErrNoFutureRun    = errors.New("schedule has no future run")
)

// Runner starts a job's turn. The scheduler holds no reference to the agent;
// the daemon supplies this.
type Runner interface {
	// StartJob opens a FRESH session for the job and begins its turn,
	// returning the new session id. It returns as soon as the turn is under
	// way — a long turn must not stall the tick loop.
	StartJob(ctx context.Context, job store.Job) (sessionID string, err error)
}

type Scheduler struct {
	store  *store.Store
	runner Runner
	now    func() time.Time
}

func New(st *store.Store, r Runner, now func() time.Time) *Scheduler {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Scheduler{store: st, runner: r, now: now}
}

// Tick fires every job whose next_run has passed. A job is advanced BEFORE
// it is started: a crash mid-turn then costs one skipped run rather than an
// endless re-fire loop on every restart.
//
// The next time is computed from NOW, not from the missed slot, which is the
// whole of the no-backfill rule: a daemon down for a week fires each daily
// job once, not seven times.
func (s *Scheduler) Tick(ctx context.Context) error {
	now := s.now().UTC()
	due, err := s.store.DueJobs(ctx, now)
	if err != nil {
		return fmt.Errorf("read due jobs: %w", err)
	}
	for _, job := range due {
		// Wrap each job in a recover to prevent a panic in one job from stopping
		// the others. The guarantee is that one failing job does not stop the
		// others, for both errors and panics.
		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("job panicked", "job", job.ID, "panic", r)
				}
			}()

			sched, err := Parse(job.Spec)
			if err != nil {
				// A spec that cannot be parsed will never become valid on its
				// own, and leaving it enabled means retrying it every tick
				// forever. Disable it and say why.
				slog.Error("disabling job with an invalid schedule", "job", job.ID, "spec", job.Spec, "err", err)
				if err := s.store.SetJobEnabled(ctx, job.ID, false); err != nil {
					slog.Error("could not disable the invalid job", "job", job.ID, "err", err)
				}
				return
			}
			next := sched.Next(now)
			// Persist the advance BEFORE starting the job. StartJob commits a
			// session row and launches a turn, so a crash after that point but
			// before the advance was written would leave next_run in the past
			// and re-fire the job on restart, duplicating the session. Writing
			// first means a crash costs one SKIPPED run instead, which is the
			// trade this scheduler deliberately makes.
			if err := s.store.MarkJobRun(ctx, job.ID, now, next, ""); err != nil {
				slog.Error("could not advance job; skipping this firing to avoid a re-fire loop",
					"job", job.ID, "err", err)
				return
			}
			sessionID, startErr := s.runner.StartJob(ctx, job)
			if startErr != nil {
				// The job has already been advanced, so a runner that is briefly
				// unavailable does not leave every job permanently due.
				slog.Error("job did not start", "job", job.ID, "err", startErr)
				return
			}
			if err := s.store.SetJobLastSession(ctx, job.ID, sessionID); err != nil {
				slog.Error("could not record the job's session", "job", job.ID, "err", err)
			}
		}()
	}
	return nil
}

// CreateJob validates a spec, computes the first fire time and stores the
// job. Both the HTTP API and the schedule builtin go through it, so a job
// the model created and a job a human created are indistinguishable
// afterwards and there is exactly one place that decides what a valid
// schedule is.
func CreateJob(ctx context.Context, st *store.Store, spec, prompt string, now time.Time) (store.Job, error) {
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return store.Job{}, ErrPromptRequired
	}
	sched, err := Parse(spec)
	if err != nil {
		return store.Job{}, err
	}
	next := sched.Next(now.UTC())
	if next.IsZero() {
		return store.Job{}, ErrNoFutureRun
	}
	job := store.Job{
		Kind: sched.Kind(), Spec: strings.TrimSpace(spec), Prompt: prompt,
		Enabled: true, NextRun: next,
	}
	id, err := st.CreateJob(ctx, job)
	if err != nil {
		return store.Job{}, err
	}
	job.ID = id
	return job, nil
}

// Run ticks until ctx is cancelled. It ticks once immediately, which is how
// a run missed while the daemon was down fires on the next start.
func (s *Scheduler) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 30 * time.Second
	}
	if err := s.Tick(ctx); err != nil {
		slog.Error("scheduler tick failed", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				slog.Error("scheduler tick failed", "err", err)
			}
		}
	}
}
