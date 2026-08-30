package policy

import (
	"os"
	"path/filepath"
	"strings"
)

// Resolve turns a tool-supplied path into an absolute, symlink-resolved path.
// It expands a leading "~", resolves relative paths against the workspace,
// cleans "..", and follows symlinks on the longest existing prefix so a link
// cannot hide an escape behind a name that looks local. A path that does not
// exist yet still resolves: its existing ancestor is resolved and the
// remaining names are appended.
func Resolve(workspace, p string) (string, error) {
	if p == "" {
		return "", os.ErrInvalid
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	return resolveExisting(filepath.Clean(p)), nil
}

// resolveExisting walks up until a path component exists, resolves that
// prefix through symlinks, and rejoins the tail.
func resolveExisting(p string) string {
	tail := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			if tail == "" {
				return resolved
			}
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p // reached the root without finding anything that exists
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}

// Inside reports whether p resolves to the workspace itself or something
// beneath it. Comparison is on path boundaries, so "/ws-evil" is not inside
// "/ws".
func Inside(workspace, p string) bool {
	ws, err := Resolve(workspace, workspace)
	if err != nil {
		return false
	}
	abs, err := Resolve(ws, p)
	if err != nil {
		return false
	}
	if abs == ws {
		return true
	}
	rel, err := filepath.Rel(ws, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
