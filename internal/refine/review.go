package refine

import (
	"context"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/store"
)

// Accept applies a proposed edit if its target still holds what the planner
// saw; otherwise the row becomes stale and nothing is written.
func (r *Refiner) Accept(ctx context.Context, id int64) (store.Refinement, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	row, ok, err := r.Store.Refinement(ctx, id)
	if err != nil {
		return store.Refinement{}, err
	}
	if !ok {
		return store.Refinement{}, fmt.Errorf("no refinement %d", id)
	}
	if row.Status != store.RefineProposed {
		return row, fmt.Errorf("refinement %d is %s, not proposed", id, row.Status)
	}
	path, err := r.pathFor(row)
	if err != nil {
		return row, err
	}
	cur, err := readTarget(path)
	if err != nil {
		return row, err
	}
	if !sameContent(cur, row.Before) {
		if _, err := r.Store.SetRefinementStatus(ctx, id, store.RefineProposed, store.RefineStale); err != nil {
			return row, err
		}
		row.Status = store.RefineStale
		return row, nil
	}
	moved, err := r.Store.SetRefinementStatus(ctx, id, store.RefineProposed, store.RefineApplied)
	if err != nil {
		return row, err
	}
	if !moved {
		return row, fmt.Errorf("refinement %d changed while it was being accepted", id)
	}
	if err := r.write(ctx, row.Kind, row.Target, path, row.After); err != nil {
		_, _ = r.Store.SetRefinementStatus(ctx, id, store.RefineApplied, store.RefineFailed)
		row.Status = store.RefineFailed
		return row, err
	}
	row.Status = store.RefineApplied
	return row, nil
}

// Reject discards a proposed edit.
func (r *Refiner) Reject(ctx context.Context, id int64) error {
	moved, err := r.Store.SetRefinementStatus(ctx, id, store.RefineProposed, store.RefineRejected)
	if err != nil {
		return err
	}
	if !moved {
		return fmt.Errorf("refinement %d is not proposed", id)
	}
	return nil
}

// RollbackResult is what a rollback did.
type RollbackResult struct {
	RoundID    string
	RolledBack []store.Refinement
	Stale      []store.Refinement
	Failed     []store.Refinement
}

// Rollback restores every applied edit in a round, newest first. An empty
// roundID means the session's most recent round with an applied edit. A
// target that no longer holds the edit's result is marked stale and left
// alone; a round has one edit per target, so the rest still roll back.
func (r *Refiner) Rollback(ctx context.Context, sessionID, roundID string) (RollbackResult, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	if roundID == "" {
		id, ok, err := r.Store.LatestAppliedRound(ctx, sessionID)
		if err != nil {
			return RollbackResult{}, err
		}
		if !ok {
			return RollbackResult{}, errors.New("no applied refinement to roll back in this session")
		}
		roundID = id
	}
	rows, err := r.Store.RoundRefinements(ctx, roundID)
	if err != nil {
		return RollbackResult{}, err
	}
	if len(rows) == 0 {
		return RollbackResult{}, fmt.Errorf("no refinement round %s", roundID)
	}
	if rows[0].SessionID != sessionID {
		return RollbackResult{}, fmt.Errorf("round %s belongs to another session", roundID)
	}
	out := RollbackResult{RoundID: roundID}
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		if row.Status != store.RefineApplied {
			continue
		}
		path, err := r.pathFor(row)
		if err != nil {
			return out, err
		}
		cur, err := readTarget(path)
		if err != nil {
			return out, err
		}
		if !sameContent(cur, row.After) {
			if _, err := r.Store.SetRefinementStatus(ctx, row.ID, store.RefineApplied, store.RefineStale); err != nil {
				return out, err
			}
			row.Status = store.RefineStale
			out.Stale = append(out.Stale, row)
			continue
		}
		moved, err := r.Store.SetRefinementStatus(ctx, row.ID, store.RefineApplied, store.RefineRolledBack)
		if err != nil {
			return out, err
		}
		if !moved {
			continue
		}
		if err := r.write(ctx, row.Kind, row.Target, path, row.Before); err != nil {
			_, _ = r.Store.SetRefinementStatus(ctx, row.ID, store.RefineRolledBack, store.RefineFailed)
			row.Status = store.RefineFailed
			out.Failed = append(out.Failed, row)
			continue
		}
		row.Status = store.RefineRolledBack
		out.RolledBack = append(out.RolledBack, row)
	}
	return out, nil
}
