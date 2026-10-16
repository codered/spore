package policy

import (
	"sync"
	"time"
)

// grantTable holds the timed grants, keyed by root session id. It lives in
// memory only: a daemon restart drops every grant.
type grantTable struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// StartGrant lets every ask in the root's session tree run without a prompt
// until the deadline. It starts a grant, or replaces the root's current one.
// Only the human-facing API calls this, never a sub-agent.
func (g *Guard) StartGrant(root string, until time.Time) {
	g.grants.mu.Lock()
	defer g.grants.mu.Unlock()
	if g.grants.until == nil {
		g.grants.until = map[string]time.Time{}
	}
	g.grants.until[root] = until
}

// EndGrant stops the root's grant. Asks after it prompt again.
func (g *Guard) EndGrant(root string) {
	g.grants.mu.Lock()
	defer g.grants.mu.Unlock()
	delete(g.grants.until, root)
}

// GrantUntil reports the root's grant deadline. It returns false when there is
// no grant or the grant has expired. An expired grant is removed here.
func (g *Guard) GrantUntil(root string) (time.Time, bool) {
	g.grants.mu.Lock()
	defer g.grants.mu.Unlock()
	until, ok := g.grants.until[root]
	if !ok {
		return time.Time{}, false
	}
	if !g.now().Before(until) {
		delete(g.grants.until, root)
		return time.Time{}, false
	}
	return until, true
}

// Grants returns every live grant keyed by root session id. Expired grants are
// left out and removed. Clients attaching to the global feed use it to learn
// which roots are granted.
func (g *Guard) Grants() map[string]time.Time {
	g.grants.mu.Lock()
	defer g.grants.mu.Unlock()
	now := g.now()
	out := make(map[string]time.Time, len(g.grants.until))
	for root, until := range g.grants.until {
		if !now.Before(until) {
			delete(g.grants.until, root)
			continue
		}
		out[root] = until
	}
	return out
}
