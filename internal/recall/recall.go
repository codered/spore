// Package recall declares the search interface spore's memory hides behind.
// The default backend is keyword search over SQLite FTS5 and ships in Plan 5a;
// a semantic backend implements the same interface in 5b, which is why every
// caller depends on this package and never on a backend.
package recall

import (
	"context"
	"time"
)

// Kinds of indexed content. Tool results are deliberately absent: they carry
// third-party text, and making it retrievable would let one injected page
// reappear in a later turn.
const (
	KindMessage = "message"
	KindSummary = "summary"
	KindFact    = "fact"
)

const (
	// DefaultK is the hit count when a caller names none.
	DefaultK = 8
	// MaxK bounds one search, so a model cannot spend a turn's whole budget
	// on retrieved text.
	MaxK = 25
)

// Chunk is one indexed unit. ID is the kind's own identifier: a message id, a
// session id for a summary, a fact name for a fact.
type Chunk struct {
	ID        string
	Kind      string
	Text      string
	SessionID string
	CreatedAt time.Time
}

// Hit is a Chunk with its match. Score's meaning is backend-defined; only the
// ordering the backend returns is contractual.
type Hit struct {
	Chunk
	Score   float64
	Excerpt string
}

// Query is a struct rather than positional arguments because scoping is not
// optional: the recall_search tool narrows by session and kind under the
// remote trust profile, and a second backend must not force a signature change
// to gain a filter.
type Query struct {
	Text      string
	K         int
	Kinds     []string
	SessionID string
}

// Status describes the backend for `spore recall status`. Degraded is always
// false for a local backend; it exists for the 5b case where a configured
// vector store is unreachable and search has fallen back.
type Status struct {
	Backend  string
	Counts   map[string]int
	Degraded bool
	Reason   string
}

type Recall interface {
	Index(ctx context.Context, chunks []Chunk) error
	Search(ctx context.Context, q Query) ([]Hit, error)
	Status(ctx context.Context) (Status, error)
}

// ClampK applies DefaultK and MaxK. Backends share it so `k` means the same
// thing whichever one is configured.
func ClampK(k int) int {
	switch {
	case k <= 0:
		return DefaultK
	case k > MaxK:
		return MaxK
	default:
		return k
	}
}
