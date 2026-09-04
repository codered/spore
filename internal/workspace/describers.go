package workspace

import "sync"

// Describers holds one Describer per root. One daemon serves sessions in
// different directories, and each needs its own cached environment section:
// a single describer would rebuild on every turn as sessions alternate, and
// worse, could hand one session the other's file listing.
type Describers struct {
	mu     sync.Mutex
	byRoot map[string]*Describer
}

func NewDescribers() *Describers {
	return &Describers{byRoot: map[string]*Describer{}}
}

// Describe renders the environment section for one root. An empty root means
// the caller has no session workspace, and gets no environment section rather
// than a description of somewhere it is not working.
func (d *Describers) Describe(root string) string {
	if root == "" {
		return ""
	}
	d.mu.Lock()
	dd, ok := d.byRoot[root]
	if !ok {
		dd = NewDescriber(root)
		d.byRoot[root] = dd
	}
	d.mu.Unlock()
	return dd.Describe()
}
