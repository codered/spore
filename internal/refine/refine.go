// Package refine is continual refinement: a reviewer pass over a session's
// recent conversation that records what was learned as memory facts and
// workspace project notes. Every edit is ledgered with its before and after
// content so it can be rolled back, and edits from sessions spore does not
// trust (Discord, jobs) are only proposed until a human accepts them.
//
// This package is the only writer refinement has. The planner model returns
// JSON; nothing it says is executed, only validated and applied here.
package refine

import (
	"context"
	"sync"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// Trigger is what started a round. It is recorded on every ledger row.
type Trigger string

const (
	TriggerManual     Trigger = "manual"
	TriggerCompaction Trigger = "compaction"
	TriggerIdle       Trigger = "idle"
	TriggerModel      Trigger = "model"
)

// Edit kinds: the closed vocabulary the planner may use.
const (
	KindFactCreate   = "fact.create"
	KindFactUpdate   = "fact.update"
	KindFactDelete   = "fact.delete"
	KindNotesAppend  = "notes.append"
	KindNotesReplace = "notes.replace"
)

// Refiner runs refinement rounds. One per daemon.
type Refiner struct {
	Store    *store.Store
	Registry *provider.Registry
	Router   *router.Router
	Cfg      *config.Config
	// Facts is the same cache the agent assembles prompts from and the
	// memory tool reloads; its Dir is the fact directory.
	Facts *memory.Cache
	// Notify, when set, is told the note each round writes, so a live view
	// can show it. Set before any round runs; never changed after.
	Notify func(sessionID, text string)

	// ctx is what background rounds run under; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards inFlight and requests.
	mu       sync.Mutex
	inFlight map[string]bool
	requests map[string]string
	// fileMu serialises read-check-write on target files across rounds,
	// accepts and rollbacks.
	fileMu sync.Mutex
	// beforeApply, when set, runs between the planner call and apply. Tests
	// use it to change a file underneath a round.
	beforeApply func()
}

func New(st *store.Store, reg *provider.Registry, rt *router.Router, cfg *config.Config, facts *memory.Cache) *Refiner {
	ctx, cancel := context.WithCancel(context.Background())
	return &Refiner{
		Store: st, Registry: reg, Router: rt, Cfg: cfg, Facts: facts,
		ctx: ctx, cancel: cancel,
		inFlight: map[string]bool{}, requests: map[string]string{},
	}
}

// Close cancels background rounds and waits for them to return.
func (r *Refiner) Close() {
	r.cancel()
	r.wg.Wait()
}
