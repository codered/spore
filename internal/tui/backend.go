package tui

import (
	"context"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// Backend is the daemon as the interface sees it. cmd/spore adapts its HTTP
// client to it; tests use a fake. Events returns once the stream is open and
// the channel closes when the stream ends for any reason.
type Backend interface {
	Sessions(ctx context.Context) ([]daemon.SessionJSON, error)
	Transcript(ctx context.Context, id string) (daemon.TranscriptJSON, error)
	Events(ctx context.Context) (<-chan daemon.WireEvent, error)
	Send(ctx context.Context, id, text string) error
	Stop(ctx context.Context, id string) error
	Resolve(ctx context.Context, id string, pendingID int64, ans policy.Answer) error
	CancelAgent(ctx context.Context, parent, child string) error
	NewSession(ctx context.Context, workspace string) (string, error)
	// Slash runs clear, compact, context, usage, skills or agents and
	// returns the text to show.
	Slash(ctx context.Context, id, cmd string) (string, error)
	// DeleteSessions deletes the named sessions (or all of them) with their
	// sub-agents; discord asks the bridge to delete its copy too.
	DeleteSessions(ctx context.Context, ids []string, all, discord bool) (daemon.DeleteSessionsJSON, error)
	// MarkSeen records that the session has been opened.
	MarkSeen(ctx context.Context, id string) error
}
