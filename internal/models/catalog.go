// Package models is what /model reads and writes: the catalog of models the
// configured providers serve, and the per-operation choice made from it.
package models

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/provider"
)

const (
	// listTimeout bounds one provider's listing. A provider that is down
	// must cost /model a short wait, not a hang. Unsloth Studio rebuilds its
	// list after a few seconds idle, and the rebuild takes up to about 6s.
	listTimeout = 10 * time.Second
	// cacheTTL keeps opening /model cheap; a choice re-checks fresh.
	cacheTTL = 60 * time.Second
)

// Group is one provider's models, as provider/model refs. A Stale group's
// listing failed: Refs is the last good listing, shown but not choosable.
type Group struct {
	Provider string   `json:"provider"`
	Refs     []string `json:"refs"`
	Error    string   `json:"error,omitempty"`
	Stale    bool     `json:"stale,omitempty"`
}

// Catalog lists what each provider serves.
type Catalog struct {
	reg        *provider.Registry
	configured []string
	timeout    time.Duration
	ttl        time.Duration
	now        func() time.Time

	mu     sync.Mutex
	cached []Group
	at     time.Time
	// last is each listing provider's last good refs, kept so that one
	// slow or failed listing does not empty the provider's models.
	last map[string][]string
	// timeouts replaces timeout for the providers it names.
	timeouts map[string]time.Duration
}

// NewCatalog lists the providers in reg. configured is every ref the config
// names; a provider that cannot list offers the ones with its prefix.
func NewCatalog(reg *provider.Registry, configured []string) *Catalog {
	return &Catalog{reg: reg, configured: configured, timeout: listTimeout, ttl: cacheTTL, now: time.Now, last: map[string][]string{}, timeouts: map[string]time.Duration{}}
}

// SetListTimeout makes d the listing timeout for the named provider. A
// non-positive d restores the default.
func (c *Catalog) SetListTimeout(name string, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d <= 0 {
		delete(c.timeouts, name)
		return
	}
	c.timeouts[name] = d
}

// List returns one group per provider, sorted by name. fresh skips the cache.
func (c *Catalog) List(ctx context.Context, fresh bool) []Group {
	c.mu.Lock()
	if !fresh && c.cached != nil && c.now().Sub(c.at) < c.ttl {
		g := c.cached
		c.mu.Unlock()
		return g
	}
	c.mu.Unlock()

	groups := c.fetch(ctx)
	// A listing cut short by the caller says nothing about the providers, so
	// it must not stand in for the next 60 seconds of answers.
	if ctx.Err() != nil {
		return groups
	}
	c.mu.Lock()
	c.cached, c.at = groups, c.now()
	c.mu.Unlock()
	return groups
}

func (c *Catalog) fetch(ctx context.Context) []Group {
	names := c.reg.Names()
	groups := make([]Group, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			groups[i] = c.group(ctx, name)
		}()
	}
	wg.Wait()
	return groups
}

func (c *Catalog) group(ctx context.Context, name string) Group {
	g := Group{Provider: name, Refs: []string{}}
	p, _ := c.reg.Provider(name)
	l, ok := p.(provider.Lister)
	if !ok {
		for _, ref := range c.configured {
			if strings.HasPrefix(ref, name+"/") && !slices.Contains(g.Refs, ref) {
				g.Refs = append(g.Refs, ref)
			}
		}
		sort.Strings(g.Refs)
		return g
	}
	c.mu.Lock()
	timeout, ok := c.timeouts[name]
	c.mu.Unlock()
	if !ok {
		timeout = c.timeout
	}
	lctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ids, err := l.ListModels(lctx)
	if err != nil {
		g.Error = err.Error()
		c.mu.Lock()
		last, ok := c.last[name]
		c.mu.Unlock()
		if ok {
			g.Refs, g.Stale = slices.Clone(last), true
			g.Error += "; showing the last list"
		}
		return g
	}
	for _, id := range ids {
		g.Refs = append(g.Refs, name+"/"+id)
	}
	sort.Strings(g.Refs)
	c.mu.Lock()
	c.last[name] = slices.Clone(g.Refs)
	c.mu.Unlock()
	return g
}

// Has reports whether ref is served right now. It always lists fresh: a
// choice is checked against what the provider says at that moment, and a
// provider that is down makes its models unavailable, even those its last
// good listing named.
func (c *Catalog) Has(ctx context.Context, ref string) bool {
	for _, g := range c.List(ctx, true) {
		if !g.Stale && slices.Contains(g.Refs, ref) {
			return true
		}
	}
	return false
}

// Refs is every ref in the current listing, in display order.
func (c *Catalog) Refs(ctx context.Context) []string {
	var out []string
	for _, g := range c.List(ctx, false) {
		out = append(out, g.Refs...)
	}
	return out
}
