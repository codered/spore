package workspace

import (
	"fmt"
	"path/filepath"

	"github.com/codered/spore/internal/policy"
)

// Request is everything needed to decide where a new session is rooted.
type Request struct {
	// Requested is the directory the creator asked for. Empty means the
	// creator has none -- a bridge, the web UI, the scheduler -- and the
	// session gets one of its own.
	Requested string
	// Ceiling is policy.workspace: not a location, but the bound every
	// session's root must lie within.
	Ceiling string
	// RemoteRoot is [policy.remote] workspace. Empty keeps a remote session
	// in its own directory, which holds nothing but its own transcript.
	RemoteRoot string
	// Remote reports that the creator is on the remote trust profile.
	Remote bool
}

// Root decides the workspace to record for a new session. It returns "" when
// the store should allocate a session directory, and an error when the
// requested root is not one this daemon is allowed to hand out -- refused at
// creation rather than quietly moved, so a creator learns immediately that it
// asked for something outside the ceiling.
//
// A session directory spore allocates is never checked against the ceiling:
// spore allocated it, and a ceiling naming a project directory would
// otherwise reject spore's own storage. That is why the allocation path
// returns before any containment check rather than after one.
func Root(req Request) (string, error) {
	// A remote creator does not get to name a directory: the request arrives
	// from an untrusted party, and the operator's answer to "where may a
	// bridge user work" is [policy.remote] workspace, checked at config load.
	if req.Remote {
		return req.RemoteRoot, nil
	}
	if req.Requested == "" {
		return "", nil
	}
	if !filepath.IsAbs(req.Requested) {
		return "", fmt.Errorf("workspace %q must be an absolute path: the daemon has no working directory to resolve it against", req.Requested)
	}
	abs, err := policy.Resolve(req.Requested, req.Requested)
	if err != nil {
		return "", fmt.Errorf("workspace %q: %w", req.Requested, err)
	}
	if !policy.Inside(req.Ceiling, abs) {
		return "", fmt.Errorf("workspace %s is outside the configured ceiling %s (policy.workspace)", abs, req.Ceiling)
	}
	return abs, nil
}

// Allocated reports whether ws is a session directory spore allocated. It is
// what decides whether the daemon may create the directory on the session's
// first turn: spore makes its own storage, and never makes a directory a
// human named.
func Allocated(sessionsDir, ws string) bool {
	return policy.Inside(sessionsDir, ws)
}
