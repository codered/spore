package policy

import (
	"errors"
	"fmt"
	"sync"

	"github.com/codered/spore/internal/config"
)

// Reloader keeps the running engine in step with the spore-managed policy
// block. It rebuilds from the policy the daemon started with, replacing only
// the learned rules with a fresh read of the block: a hand edit anywhere
// else in config.toml still needs a restart, so a half-finished edit can
// never change a running daemon's policy.
type Reloader struct {
	mu   sync.Mutex
	path string
	base config.PolicyConfig
	g    *Guard
}

func NewReloader(path string, base config.PolicyConfig, g *Guard) *Reloader {
	return &Reloader{path: path, base: base, g: g}
}

// Learn writes one rule into the block and makes it live.
func (r *Reloader) Learn(d Decision, rule string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := config.LearnRule(r.path, string(d), rule); err != nil {
		return err
	}
	return r.rebuild()
}

// Unlearn removes one rule from the block and makes that live. A rule the
// block does not hold is config.ErrNotLearned; the engine is rebuilt anyway,
// because the block may have been edited by hand since it was last read and
// the file is what the engine must match.
func (r *Reloader) Unlearn(d Decision, rule string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := config.UnlearnRule(r.path, string(d), rule)
	if err != nil && !errors.Is(err, config.ErrNotLearned) {
		return err
	}
	if rerr := r.rebuild(); rerr != nil {
		return rerr
	}
	return err
}

// rebuild swaps in an engine built from the block as it is now. On failure
// the old engine stays: the file write already happened and stands, and the
// next successful rebuild or a restart applies it.
func (r *Reloader) rebuild() error {
	learned, err := config.ReadLearned(r.path)
	if err != nil {
		return fmt.Errorf("the config was written but policy was not reloaded: %w", err)
	}
	pc := r.base
	pc.Learned = learned
	e, err := NewEngine(pc)
	if err != nil {
		return fmt.Errorf("the config was written but policy was not reloaded: %w", err)
	}
	r.g.SetEngine(e)
	return nil
}
