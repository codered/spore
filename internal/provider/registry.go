package provider

import (
	"fmt"
	"strings"
	"sync"
)

// ProviderPrice is USD per million tokens. CacheWrite and CacheRead are
// optional: unset, they default to the usual multipliers of In. They are
// settable because the multipliers are not universal -- Fable 5.1 reads at
// 0.025x -- and an operator should not wait for a spore release to price
// their model correctly.
type ProviderPrice struct{ In, Out, CacheWrite, CacheRead float64 }

const (
	defaultCacheWriteMultiplier = 1.25
	defaultCacheReadMultiplier  = 0.10
)

func (p ProviderPrice) Cost(u Usage) float64 {
	write, read := p.CacheWrite, p.CacheRead
	if write == 0 {
		write = p.In * defaultCacheWriteMultiplier
	}
	if read == 0 {
		read = p.In * defaultCacheReadMultiplier
	}
	return float64(u.InputTokens)/1e6*p.In +
		float64(u.OutputTokens)/1e6*p.Out +
		float64(u.CacheWriteTokens)/1e6*write +
		float64(u.CacheReadTokens)/1e6*read
}

type entry struct {
	p     Provider
	price ProviderPrice
}

type Registry struct {
	mu      sync.RWMutex
	entries map[string]entry
}

func NewRegistry() *Registry { return &Registry{entries: map[string]entry{}} }

func (r *Registry) Register(name string, p Provider, price ProviderPrice) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[name] = entry{p: p, price: price}
}

// Resolve splits a "provider/model" ref and returns the registered provider,
// the bare model id to send upstream, and its pricing.
func (r *Registry) Resolve(ref string) (Provider, string, ProviderPrice, error) {
	name, model, ok := strings.Cut(ref, "/")
	if !ok || name == "" || model == "" {
		return nil, "", ProviderPrice{}, fmt.Errorf("model ref %q must be of the form provider/model", ref)
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[name]
	if !ok {
		return nil, "", ProviderPrice{}, fmt.Errorf("no provider %q configured (ref %q)", name, ref)
	}
	return e.p, model, e.price, nil
}
