// Package mirror brings a secondary recall backend up to date with the
// keyword index. The keyword index is written inside the transaction that
// writes a message; a vector store cannot join that transaction, so it is
// caught up afterwards from a watermark instead.
package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/recall"
	"github.com/codered/spore/internal/store"
)

// batchSize is how many rows one pass reads and sends. It bounds memory
// during a backfill of a long history; the pass loops until it is caught up.
const batchSize = 100

// tombstoneTTL is how long a tombstone is kept before being swept. A long-offline
// backend can lose deletions, but seven days is far longer than a sidecar is
// plausibly down.
const tombstoneTTL = 7 * 24 * time.Hour

// Source is the store as the mirror needs it. The narrow interface is what
// lets the mirror be tested without a database.
type Source interface {
	IndexRowsSince(ctx context.Context, cursor int64, limit int) ([]store.IndexRow, error)
	SyncCursor(ctx context.Context, backend string) (int64, error)
	SetSyncCursor(ctx context.Context, backend string, cursor int64) error
	SyncDelCursor(ctx context.Context, backend string) (int64, error)
	SetSyncDelCursor(ctx context.Context, backend string, delCursor int64) error
	// TombstonesSince returns tombstones with id > delCursor, oldest first.
	TombstonesSince(ctx context.Context, delCursor int64, limit int) ([]store.TombstoneRow, error)
	// HasTombstoneKey checks whether recall_fts holds a row for (kind, ref_id).
	HasTombstoneKey(ctx context.Context, kind, refID string) (bool, error)
	// SweepTombstones removes tombstones older than before.
	SweepTombstones(ctx context.Context, before time.Time) error
	// ResetDelCursor zeroes the del_cursor for a backend.
	ResetDelCursor(ctx context.Context, backend string) error
	// ClearTombstones removes all tombstones.
	ClearTombstones(ctx context.Context) error
}

type Mirror struct {
	src     Source
	target  recall.Recall
	backend string
	log     *slog.Logger
}

func New(src Source, target recall.Recall, backend string, log *slog.Logger) *Mirror {
	if log == nil {
		log = slog.Default()
	}
	return &Mirror{src: src, target: target, backend: backend, log: log}
}

// Once pushes every row written since the watermark and returns how many
// chunks it wrote. A first run on an existing database is a full backfill,
// which is why setup needs no separate backfill path.
func (m *Mirror) Once(ctx context.Context) (int, error) {
	cursor, err := m.src.SyncCursor(ctx, m.backend)
	if err != nil {
		return 0, err
	}
	written := 0
	for {
		rows, err := m.src.IndexRowsSince(ctx, cursor, batchSize)
		if err != nil {
			return written, err
		}
		if len(rows) == 0 {
			// After inserts are drained, process tombstones.
			if err := m.processTombstones(ctx); err != nil {
				return written, err
			}
			return written, nil
		}

		chunks := make([]recall.Chunk, 0, len(rows))
		last := cursor
		for _, r := range rows {
			last = r.RowID
			if strings.TrimSpace(r.Text) == "" {
				continue // nothing to embed; the cursor still moves past it
			}
			when, _ := time.Parse(time.RFC3339, r.CreatedAt)
			chunks = append(chunks, recall.Chunk{
				ID: r.RefID, Kind: r.Kind, Text: r.Text,
				SessionID: r.SessionID, CreatedAt: when,
			})
		}

		if len(chunks) > 0 {
			// The cursor moves only after the target accepted the batch. A
			// crash in between re-sends rows, and re-sending is harmless
			// because object ids are derived from kind and ref id.
			if err := m.target.Index(ctx, chunks); err != nil {
				return written, err
			}
			written += len(chunks)
		}
		if err := m.src.SetSyncCursor(ctx, m.backend, last); err != nil {
			return written, err
		}
		cursor = last
	}
}

// Run catches up on a timer until ctx is cancelled. A failed pass is logged
// and retried on the next tick: the vector store being down is a degraded
// state to sit in, not a reason to stop the daemon.
func (m *Mirror) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := m.Once(ctx)
			if err != nil {
				m.log.Warn("recall mirror fell behind", "backend", m.backend, "error", err)
				continue
			}
			if n > 0 {
				m.log.Info("recall mirror caught up", "backend", m.backend, "chunks", n)
			}
		}
	}
}

// processTombstones carries deletions to the target under a cursor of their
// own. It runs after the insert phase, not before: a row read into an in-flight
// batch can be deleted in SQLite before that batch lands, which would leave the
// object created after its own tombstone had been drained.
func (m *Mirror) processTombstones(ctx context.Context) error {
	delCursor, err := m.src.SyncDelCursor(ctx, m.backend)
	if err != nil {
		return err
	}

	for {
		tombstones, err := m.src.TombstonesSince(ctx, delCursor, 100)
		if err != nil {
			return err
		}
		if len(tombstones) == 0 {
			return m.src.SweepTombstones(ctx, time.Now().Add(-tombstoneTTL))
		}

		for _, t := range tombstones {
			// A key that is back in recall_fts was re-indexed after this
			// tombstone was written. Object ids derive from kind and ref id
			// alone, so applying the delete now would destroy the live vector
			// the insert phase is about to overwrite.
			indexed, err := m.src.HasTombstoneKey(ctx, t.Kind, t.RefID)
			if err != nil {
				return err
			}
			if indexed {
				delCursor = t.ID
				if err := m.src.SetSyncDelCursor(ctx, m.backend, delCursor); err != nil {
					return err
				}
				continue
			}

			if err := m.target.Delete(ctx, t.Kind, t.RefID); err != nil {
				// A failed delete stops the pass with the cursor where it was,
				// so the next tick retries this same tombstone. The ones behind
				// it wait: head-of-line blocking is the intended behaviour, and
				// `recall reindex` is the escape hatch. The error is returned
				// rather than swallowed so Run reports the mirror as behind
				// instead of logging that it caught up.
				return fmt.Errorf("delete %s %s: %w", t.Kind, t.RefID, err)
			}
			// The cursor moves only after the target accepted, so a crash
			// re-sends the delete rather than skipping it.
			delCursor = t.ID
			if err := m.src.SetSyncDelCursor(ctx, m.backend, delCursor); err != nil {
				return err
			}
		}
	}
}

// Reset rewinds the watermark so the next pass re-sends the whole corpus.
// `recall reindex` rebuilds recall_fts from the source tables, which
// renumbers every rowid, so the old watermark points at nothing meaningful
// afterwards.
func (m *Mirror) Reset(ctx context.Context) error {
	if err := m.src.ResetDelCursor(ctx, m.backend); err != nil {
		return err
	}
	if err := m.src.ClearTombstones(ctx); err != nil {
		return err
	}
	return m.src.SetSyncCursor(ctx, m.backend, 0)
}
