package models

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
)

// lister is a provider that lists models; the embedded Script makes it a
// provider.Provider.
type lister struct {
	*provider.Script
	ids   []string
	err   error
	delay time.Duration
	calls atomic.Int32
}

func (l *lister) ListModels(ctx context.Context) ([]string, error) {
	l.calls.Add(1)
	if l.delay > 0 {
		select {
		case <-time.After(l.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return l.ids, l.err
}

func newLister(ids ...string) *lister { return &lister{Script: provider.NewScript(), ids: ids} }

func TestListGroupsByProviderWithPrefixedRefs(t *testing.T) {
	reg := provider.NewRegistry()
	reg.Register("studio", newLister("unsloth/Qwen3.8-27B-GGUF", "a"), provider.ProviderPrice{})
	reg.Register("anthropic", provider.NewScript(), provider.ProviderPrice{})
	c := NewCatalog(reg, []string{"anthropic/claude-opus-5", "studio/x", "anthropic/claude-opus-5"})

	got := c.List(context.Background(), false)
	if len(got) != 2 || got[0].Provider != "anthropic" || got[1].Provider != "studio" {
		t.Fatalf("groups = %+v", got)
	}
	if !slices.Equal(got[0].Refs, []string{"anthropic/claude-opus-5"}) {
		t.Fatalf("non-listing provider refs = %v, want the configured ones", got[0].Refs)
	}
	if !slices.Equal(got[1].Refs, []string{"studio/a", "studio/unsloth/Qwen3.8-27B-GGUF"}) {
		t.Fatalf("listing provider refs = %v", got[1].Refs)
	}
	if !c.Has(context.Background(), "studio/unsloth/Qwen3.8-27B-GGUF") {
		t.Fatal("Has(id with a slash) = false")
	}
	if c.Has(context.Background(), "studio/x") {
		t.Fatal("a configured ref a listing provider does not serve must not be available")
	}
}

func TestASlowOrFailingProviderDoesNotHideTheOthers(t *testing.T) {
	reg := provider.NewRegistry()
	slow := newLister("never")
	slow.delay = time.Minute
	reg.Register("slow", slow, provider.ProviderPrice{})
	bad := newLister()
	bad.err = errors.New("connection refused")
	reg.Register("bad", bad, provider.ProviderPrice{})
	reg.Register("good", newLister("m"), provider.ProviderPrice{})
	c := NewCatalog(reg, nil)
	c.timeout = 50 * time.Millisecond

	start := time.Now()
	got := c.List(context.Background(), false)
	if time.Since(start) > 2*time.Second {
		t.Fatal("List waited for the slow provider")
	}
	byName := map[string]Group{}
	for _, g := range got {
		byName[g.Provider] = g
	}
	if byName["slow"].Error == "" || byName["bad"].Error == "" {
		t.Fatalf("errors not reported: %+v", got)
	}
	if !slices.Equal(byName["good"].Refs, []string{"good/m"}) {
		t.Fatalf("good refs = %v", byName["good"].Refs)
	}
}

func TestListCachesUntilFreshOrExpiry(t *testing.T) {
	reg := provider.NewRegistry()
	l := newLister("m")
	reg.Register("p", l, provider.ProviderPrice{})
	c := NewCatalog(reg, nil)
	now := time.Unix(0, 0)
	c.now = func() time.Time { return now }

	c.List(context.Background(), false)
	c.List(context.Background(), false)
	if n := l.calls.Load(); n != 1 {
		t.Fatalf("calls = %d, want 1 (cached)", n)
	}
	c.List(context.Background(), true)
	if n := l.calls.Load(); n != 2 {
		t.Fatalf("calls = %d, want 2 (fresh)", n)
	}
	now = now.Add(61 * time.Second)
	c.List(context.Background(), false)
	if n := l.calls.Load(); n != 3 {
		t.Fatalf("calls = %d, want 3 (expired)", n)
	}
}

func TestACancelledListingIsNotCached(t *testing.T) {
	reg := provider.NewRegistry()
	l := newLister("m")
	l.delay = time.Millisecond
	reg.Register("p", l, provider.ProviderPrice{})
	c := NewCatalog(reg, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.List(ctx, false)

	got := c.List(context.Background(), false)
	if len(got) != 1 || got[0].Error != "" || !slices.Equal(got[0].Refs, []string{"p/m"}) {
		t.Fatalf("second listing = %+v, want p/m with no error", got)
	}
	if n := l.calls.Load(); n != 2 {
		t.Fatalf("lister calls = %d, want 2 (the second listing was served from cache)", n)
	}
}
