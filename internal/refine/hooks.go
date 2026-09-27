package refine

import (
	"context"
	"errors"
	"log/slog"

	"github.com/codered/spore/internal/store"
)

// AfterCompact reviews the range compaction just folded, in the background.
// Compaction never deletes rows, so the round reads the full originals.
func (r *Refiner) AfterCompact(sessionID string, through int) {
	if !r.Cfg.Refine.On() {
		return
	}
	r.Go(sessionID, TriggerCompaction, "", through)
}

// AfterTurn starts the round the model asked for during the turn, if any.
func (r *Refiner) AfterTurn(sessionID string) {
	r.mu.Lock()
	instr, ok := r.requests[sessionID]
	delete(r.requests, sessionID)
	r.mu.Unlock()
	if !ok || !r.Cfg.Refine.On() {
		return
	}
	r.Go(sessionID, TriggerModel, instr, 0)
}

// Request records the model's ask. The round runs when the turn ends; a
// second request in the same turn replaces the instructions.
func (r *Refiner) Request(ctx context.Context, sessionID, instructions string) error {
	if !r.Cfg.Refine.On() {
		return errors.New("refinement is turned off in config ([refine] enabled = false)")
	}
	sess, ok, err := r.Store.Session(ctx, sessionID)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("no such session")
	}
	if sess.Source == store.SourceSubagent || sess.ParentID != "" {
		return ErrSubagent
	}
	r.mu.Lock()
	r.requests[sessionID] = instructions
	r.mu.Unlock()
	return nil
}

// Go runs a round in the background under the Refiner's own context, which
// Close cancels. Busy and sub-agent refusals are expected and not logged.
func (r *Refiner) Go(sessionID string, trig Trigger, instructions string, through int) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		_, err := r.Round(r.ctx, sessionID, trig, instructions, through)
		if err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrSubagent) && r.ctx.Err() == nil {
			slog.Warn("refinement failed", "session", sessionID, "trigger", string(trig), "error", err)
		}
	}()
}

// Wait blocks until every background round has returned. Tests use it.
func (r *Refiner) Wait() { r.wg.Wait() }
