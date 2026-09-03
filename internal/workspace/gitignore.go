package workspace

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// pattern is one line of a .gitignore file, pre-split into path segments.
// The subset implemented here is the one that appears in real repositories:
// comments, blank lines, negation with "!", directory-only with a trailing
// "/", anchoring with a leading or embedded "/", the "**" segment, and the
// shell globs path.Match already understands.
type pattern struct {
	negate   bool
	dirOnly  bool
	anchored bool
	segs     []string
}

// parsePattern converts one .gitignore line into a pattern. It returns
// ok=false for lines git itself ignores: blanks and comments.
func parsePattern(line string) (pattern, bool) {
	line = strings.TrimRight(line, " \t")
	if line == "" || strings.HasPrefix(line, "#") {
		return pattern{}, false
	}
	var p pattern
	if strings.HasPrefix(line, "!") {
		p.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		p.dirOnly = true
		line = strings.TrimSuffix(line, "/")
	}
	if line == "" {
		return pattern{}, false
	}
	// A slash anywhere but the end anchors the pattern to the directory
	// holding the .gitignore; without one it matches a name at any depth.
	p.anchored = strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return pattern{}, false
	}
	p.segs = strings.Split(line, "/")
	return p, true
}

// match reports whether the pattern covers rel, a slash-separated path
// relative to the directory that holds the .gitignore file.
func (p pattern) match(rel string, isDir bool) bool {
	if p.dirOnly && !isDir {
		return false
	}
	if !p.anchored {
		ok, err := path.Match(p.segs[0], path.Base(rel))
		return err == nil && ok
	}
	return matchSegs(p.segs, strings.Split(rel, "/"))
}

// matchSegs matches segment lists, giving "**" its git meaning of "any run of
// segments, including none".
func matchSegs(pat, name []string) bool {
	if len(pat) == 0 {
		return len(name) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(name); i++ {
			if matchSegs(pat[1:], name[i:]) {
				return true
			}
		}
		return false
	}
	if len(name) == 0 {
		return false
	}
	if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
		return false
	}
	return matchSegs(pat[1:], name[1:])
}

// ruleSet is the patterns of one .gitignore file plus the directory it
// governs, held relative to the walk root so matching needs no absolute paths.
type ruleSet struct {
	dir      string // slash-separated, relative to the root; "" at the root
	patterns []pattern
}

// ignoreStack is the .gitignore files seen so far, outermost first. Later
// files override earlier ones, and within one file the last matching line
// wins, which is git's own precedence.
type ignoreStack []ruleSet

// load reads dir/.gitignore and returns the stack with that file appended.
// A missing or unreadable file leaves the stack unchanged, so a directory
// without rules simply inherits its parents'.
func (s ignoreStack) load(root, dir string) ignoreStack {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(dir), ".gitignore"))
	if err != nil {
		return s
	}
	defer f.Close()
	var rs ruleSet
	rs.dir = dir
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		if p, ok := parsePattern(scan.Text()); ok {
			rs.patterns = append(rs.patterns, p)
		}
	}
	if len(rs.patterns) == 0 {
		return s
	}
	// Copy rather than append in place: sibling directories share the parent
	// stack, and appending to a shared backing array would leak one sibling's
	// rules into the next.
	out := make(ignoreStack, len(s), len(s)+1)
	copy(out, s)
	return append(out, rs)
}

// ignored reports whether rel (slash-separated, relative to the root) is
// excluded by the rules in scope.
func (s ignoreStack) ignored(rel string, isDir bool) bool {
	ignored := false
	for _, rs := range s {
		sub := rel
		if rs.dir != "" {
			prefix := rs.dir + "/"
			if !strings.HasPrefix(rel, prefix) {
				continue // the file governs another subtree
			}
			sub = strings.TrimPrefix(rel, prefix)
		}
		for _, p := range rs.patterns {
			if p.match(sub, isDir) {
				ignored = !p.negate
			}
		}
	}
	return ignored
}
