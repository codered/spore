package refine

import (
	"context"
	"errors"
	"fmt"

	"github.com/codered/spore/internal/store"
)

// ErrSelfTooLarge refuses a self.md write over companion.self_max_bytes.
var ErrSelfTooLarge = errors.New("self.md would exceed companion.self_max_bytes")

// UpdateSelf rewrites self.md under the same lock and ledger as every other
// refinement write: edit receives the current content ("" when the file is
// absent) and returns the new content, the row is written before the file,
// and the round can be rolled back like any other. The size cap is checked
// here so no writer can skip it.
func (r *Refiner) UpdateSelf(ctx context.Context, sessionID string, trig Trigger, rationale string, edit func(cur string) (string, error)) (store.Refinement, error) {
	r.fileMu.Lock()
	defer r.fileMu.Unlock()
	path := r.Cfg.SelfPath()
	before, err := readTarget(path)
	if err != nil {
		return store.Refinement{}, err
	}
	cur := ""
	if before != nil {
		cur = *before
	}
	after, err := edit(cur)
	if err != nil {
		return store.Refinement{}, err
	}
	if max := r.Cfg.Companion.SelfMaxBytes; len(after) > max {
		return store.Refinement{}, fmt.Errorf("%w: %d bytes, cap %d", ErrSelfTooLarge, len(after), max)
	}
	row := store.Refinement{
		RoundID: newRoundID(), SessionID: sessionID, Trigger: string(trig), Kind: KindSelfUpdate,
		Target: path, Before: before, After: &after, Rationale: rationale, Status: store.RefineApplied,
	}
	if row.ID, err = r.Store.AddRefinement(ctx, row); err != nil { // the row first: no write goes unrecorded
		return store.Refinement{}, err
	}
	if err := r.write(ctx, row.Kind, row.Target, path, row.After); err != nil {
		_, _ = r.Store.SetRefinementStatus(ctx, row.ID, store.RefineApplied, store.RefineFailed)
		row.Status = store.RefineFailed
		return row, err
	}
	return row, nil
}
