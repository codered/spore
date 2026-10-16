package tui

import (
	"context"
	"errors"
	"time"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// ErrApprovalGone is what Resolve returns when the daemon no longer has the
// approval waiting: it was answered, or it expired.
var ErrApprovalGone = errors.New("that approval is no longer waiting")

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
	// Refine runs a manual refinement round and returns its note line.
	Refine(ctx context.Context, id, instructions string) (string, error)
	// RefineRollback undoes the session's most recent applied round.
	RefineRollback(ctx context.Context, id string) (string, error)
	// Models is what /model shows for a session; fresh relists every
	// provider instead of using the daemon's cached listing.
	Models(ctx context.Context, id string, fresh bool) (daemon.ModelsJSON, error)
	// SetModel chooses ref for op and returns the updated view.
	SetModel(ctx context.Context, id, op, ref string) (daemon.ModelsJSON, error)
	// Companion runs one /companion line and returns its text.
	Companion(ctx context.Context, args []string) (string, error)
	// Proposals lists the companion's open proposals, newest first.
	Proposals(ctx context.Context) ([]daemon.ProposalJSON, error)
	// AnswerProposal presses one of a proposal's buttons and returns the
	// line the daemon answers with.
	AnswerProposal(ctx context.Context, id int64, choice string) (string, error)
	// StartGrant lets calls that policy would ask about run without asking,
	// on root session id, for duration (one of GrantDurations). It returns
	// when the grant ends.
	StartGrant(ctx context.Context, id, duration string) (time.Time, error)
	// EndGrant ends the root session's timed grant, if it has one.
	EndGrant(ctx context.Context, id string) error
	// GrantDurations is the list of durations a grant may be started for;
	// empty when timed grants are off.
	GrantDurations(ctx context.Context) ([]string, error)
}
