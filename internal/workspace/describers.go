package workspace

import (
	"sync"
	"time"
)

type entry struct {
	describer *Describer
	used      time.Time
}

// Describers holds one Describer per root. One daemon serves sessions in
// different directories, and each needs its own cached environment section:
// a single describer would rebuild on every turn as sessions alternate, and
// worse, could hand one session the other's file listing. Idle roots are
// periodically evicted so the cache does not grow unbounded over the
// daemon's lifetime.
type Describers struct {
	mu     sync.Mutex
	byRoot map[string]*entry
	now    func() time.Time
}

func NewDescribers() *Describers {
	return &Describers{byRoot: map[string]*entry{}, now: time.Now}
}

// Describe renders the environment section for one root. An empty root means
// the caller has no session workspace, and gets no environment section rather
// than a description of somewhere it is not working.
func (d *Describers) Describe(root string) string {
	if root == "" {
		return ""
	}
	d.mu.Lock()
	e, ok := d.byRoot[root]
	if ok {
		e.used = d.now()
		d.mu.Unlock()
		return e.describer.Describe()
	}

	// Miss: sweep idle entries before inserting the new one.
	now := d.now()
	for k, v := range d.byRoot {
		if now.Sub(v.used) > idleTTL {
			delete(d.byRoot, k)
		}
	}

	// Create and cache the new describer.
	desc := NewDescriber(root)
	d.byRoot[root] = &entry{describer: desc, used: now}
	d.mu.Unlock()
	return desc.Describe()
}
