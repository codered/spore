package recall

import (
	"context"
	"log/slog"
	"sync"
)

// Fallback searches a primary backend and drops to a secondary when the
// primary fails. It is correct without any reconciliation afterwards because
// the secondary is the keyword index, which is written inside the same
// transaction as the message it indexes and is therefore never behind.
type Fallback struct {
	primary   Recall
	secondary Recall
	log       *slog.Logger

	mu     sync.Mutex
	reason string // non-empty while the last primary call failed
}

func NewFallback(primary, secondary Recall, log *slog.Logger) *Fallback {
	if log == nil {
		log = slog.Default()
	}
	return &Fallback{primary: primary, secondary: secondary, log: log}
}

// Index writes only the primary. The secondary already holds every chunk;
// writing it here would duplicate rows the message transaction wrote.
func (f *Fallback) Index(ctx context.Context, chunks []Chunk) error {
	return f.primary.Index(ctx, chunks)
}

func (f *Fallback) Search(ctx context.Context, q Query) ([]Hit, error) {
	hits, err := f.primary.Search(ctx, q)
	if err == nil {
		f.setReason("")
		return hits, nil
	}
	f.setReason(err.Error())
	// A degraded search is not a failed turn. A sidecar being unreachable
	// degrades and never fails a turn, so the error is logged and keyword
	// hits are returned in its place.
	f.log.Warn("vector search unavailable, using keyword search", "error", err)
	return f.secondary.Search(ctx, q)
}

// Status reports the primary when it is healthy and the secondary when it is
// not, because a degraded status should describe what is actually searchable.
func (f *Fallback) Status(ctx context.Context) (Status, error) {
	st, err := f.primary.Status(ctx)
	if err == nil && !st.Degraded && f.lastReason() == "" {
		return st, nil
	}

	reason := f.lastReason()
	switch {
	case err != nil:
		reason = err.Error()
	case st.Degraded && st.Reason != "":
		reason = st.Reason
	}

	secondary, serr := f.secondary.Status(ctx)
	if serr != nil {
		return Status{Backend: st.Backend, Degraded: true, Reason: reason}, nil
	}
	secondary.Degraded = true
	secondary.Reason = reason
	return secondary, nil
}

func (f *Fallback) setReason(r string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reason = r
}

func (f *Fallback) lastReason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reason
}
