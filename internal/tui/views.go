package tui

import (
	"context"
	"errors"

	"github.com/codered/spore/internal/daemon"
)

// ErrOlderDaemon is what a Views method returns when the daemon has no route
// for it: the daemon predates the TUI. The view says so instead of looking
// empty.
var ErrOlderDaemon = errors.New("this daemon is older than the TUI — restart it")

// Views is the daemon as the resource views see it. It sits beside Backend so
// the chat screen's interface stays small; cmd/spore's adapter implements
// both, and New picks it up from the Backend it is given.
type Views interface {
	Skills(ctx context.Context, sessionID string) (daemon.SkillsJSON, error)
	Agents(ctx context.Context, sessionID string) (daemon.AgentsJSON, error)
	Jobs(ctx context.Context) ([]daemon.JobJSON, error)
	JobRuns(ctx context.Context, jobID int64) ([]daemon.JobRunJSON, error)
	Usage(ctx context.Context, sessionID string) (daemon.UsageJSON, error)
	CancelAgent(ctx context.Context, parent, child string) error
	CancelJob(ctx context.Context, id int64) error
}
