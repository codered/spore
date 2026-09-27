package refine

import (
	"context"
	"log/slog"
	"time"
)

// Sweep starts a round for every session idle past idle_minutes with a user
// message the refiner has not reviewed. running reports whether a session has
// a turn in flight; nil means none do.
func (r *Refiner) Sweep(ctx context.Context, now time.Time, running func(string) bool) error {
	if !r.Cfg.Refine.On() {
		return nil
	}
	cutoff := now.Add(-time.Duration(r.Cfg.Refine.IdleMinutes) * time.Minute)
	ids, err := r.Store.IdleSessions(ctx, cutoff)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if running != nil && running(id) {
			continue
		}
		r.Go(id, TriggerIdle, "", 0)
	}
	return nil
}

// RunSweeper sweeps on every tick until ctx ends.
func (r *Refiner) RunSweeper(ctx context.Context, every time.Duration, running func(string) bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := r.Sweep(ctx, now, running); err != nil {
				slog.Warn("refinement sweep failed", "error", err)
			}
		}
	}
}
