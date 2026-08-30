package provider

import (
	"fmt"
	"strings"
	"sync"
)

// ProviderPrice is USD per million tokens.
type ProviderPrice struct{ In, Out float64 }

func (p ProviderPrice) Cost(u Usage) float64 {
	return float64(u.InputTokens)/1e6*p.In + float64(u.OutputTokens)/1e6*p.Out
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
