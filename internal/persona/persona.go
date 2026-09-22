// Package persona reads the two markdown files that carry spore's standing
// instructions: soul.md, which is personality and global, and agent.md, which
// is standing instructions for one workspace.
//
// Neither is a fact. They carry no frontmatter, have no closed set of types,
// and are not indexed -- they are prose the user writes and spore reads. This
// package is I/O only; the sections they render into live in agent/context.go
// beside the skills index and the facts, which is the same split those two
// already use.
package persona

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
)

// warnBytes is where a persona file stops looking deliberate. It is roughly
// 4000 tokens: large enough that no soul.md written on purpose reaches it by
// accident, small enough that a runaway file is reported before it has eaten
// a noticeable share of the window.
const warnBytes = 16 << 10

// Load reads a persona file. A missing file, or an empty path, returns "" and
// no error: not having a soul.md is the ordinary case, not a failure, and
// AgentPath returns "" for a session with no workspace.
//
// A file past warnBytes is still returned whole. These are the user's own
// standing instructions, deliberate in a way a fact is not, so truncating
// them silently would be worse than the tokens they cost -- the warning goes
// to the daemon log, where it is visible without the prompt lying about what
// it contains.
func Load(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the path is built from config, not from model output
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	if len(b) > warnBytes {
		slog.Warn("persona file is unusually large", "path", path, "bytes", len(b), "warn_above", warnBytes)
	}
	return string(b), nil
}
