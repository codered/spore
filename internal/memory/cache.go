package memory

import (
	"errors"
	"sync"
)

// Cache holds the loaded facts for the process. Assembly runs on every turn
// and a directory scan per turn buys nothing: the daemon reloads after a
// write instead, which is the only way the set changes while spore runs.
type Cache struct {
	dir   string
	mu    sync.RWMutex
	facts []Fact
}

func NewCache(dir string) *Cache { return &Cache{dir: dir} }

func (c *Cache) Dir() string { return c.dir }

// Reload rereads the directory and returns the per-file errors so the caller
// can warn about them. On a directory-level failure, the cache preserves its
// existing facts and returns the error: a transient permission issue or unmounted
// volume should not silently blank the user's memory. Per-file errors follow the
// degradation rule: one broken file costs one fact, never the whole set.
func (c *Cache) Reload() []error {
	facts, errs := Load(c.dir)
	c.mu.Lock()
	defer c.mu.Unlock()
	// Check if Load hit a directory-level error (sentinel wrapped in the error).
	// If so, preserve the existing facts; otherwise update to the new set.
	var dirErr bool
	for _, e := range errs {
		if errors.Is(e, ErrReadDir) {
			dirErr = true
			break
		}
	}
	if !dirErr {
		c.facts = facts
	}
	return errs
}

// Facts returns a copy: a caller that trims for a token budget must not be
// able to edit the shared set.
func (c *Cache) Facts() []Fact {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Fact, len(c.facts))
	copy(out, c.facts)
	return out
}
