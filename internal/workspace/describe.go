// Package workspace renders the environment section of spore's system
// prompt: where the agent is working and what is in that directory. The
// listing carries names only, never file contents, and it honours .gitignore
// so a model's first impression of a repository matches the operator's.
package workspace

import (
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// maxEntries bounds the listing. A prompt is not a file browser: past a
	// couple of hundred names the model gains nothing and the turn pays for
	// every one of them on every request.
	maxEntries = 200
	// maxDepth bounds how far below the root the walk descends, counted in
	// path segments. It is the cheap guard for the case where the workspace
	// is a whole home directory rather than one repository.
	maxDepth = 4
	// cacheTTL is how long a rendered listing is reused. The working tree
	// changes during a session -- often because the agent changed it -- so
	// the listing must not be frozen for the life of the process, but it
	// need not be rebuilt for every turn either.
	cacheTTL = 30 * time.Second
)

// noiseDirs are never descended into or listed. .git is excluded because git
// itself never reports it; the rest are the build and dependency trees that
// would otherwise consume the whole entry budget in a repository whose
// .gitignore does not name them.
var noiseDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
}

// Describe renders the environment section for root. It returns the empty
// string when root is empty or cannot be read, so a workspace that has gone
// missing degrades to a prompt without the section rather than to an error.
func Describe(root string) string {
	if root == "" {
		return ""
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		return ""
	}
	entries, truncated := list(root)

	var b strings.Builder
	b.WriteString("\n\n## Environment\n\nWorking directory: ")
	b.WriteString(root)
	b.WriteString("\n")
	if len(entries) == 0 {
		b.WriteString("\nThe working directory is empty.\n")
		return b.String()
	}
	b.WriteString("\nFiles here, excluding anything .gitignore covers. " +
		"This is a listing only: read a file when you need its contents.\n\n")
	for _, e := range entries {
		b.WriteString(e)
		b.WriteString("\n")
	}
	if truncated {
		b.WriteString("\nThe listing stopped at " + itoa(maxEntries) +
			" entries. Use fs_list, fs_glob or fs_grep to see the rest.\n")
	}
	return b.String()
}

// itoa keeps the one small integer conversion in this file from pulling in
// strconv for a single call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d [20]byte
	i := len(d)
	for n > 0 {
		i--
		d[i] = byte('0' + n%10)
		n /= 10
	}
	return string(d[i:])
}

// list walks root breadth-unaware but depth-first in sorted order and returns
// the paths to show, relative to root, directories marked with a trailing
// slash. The second result reports whether the entry budget cut the walk
// short.
func list(root string) ([]string, bool) {
	var out []string
	truncated := false

	var walk func(dir string, depth int, rules ignoreStack)
	walk = func(dir string, depth int, rules ignoreStack) {
		if truncated {
			return
		}
		rules = rules.load(root, dir)
		entries, err := os.ReadDir(joinRel(root, dir))
		if err != nil {
			return // an unreadable directory is skipped, not fatal
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if noiseDirs[e.Name()] {
				continue
			}
			rel := e.Name()
			if dir != "" {
				rel = dir + "/" + rel
			}
			isDir := e.IsDir()
			if !isDir && !e.Type().IsRegular() {
				continue // sockets, devices and dangling links are not content
			}
			if rules.ignored(rel, isDir) {
				continue
			}
			if len(out) >= maxEntries {
				truncated = true
				return
			}
			if isDir {
				out = append(out, rel+"/")
				if depth+1 < maxDepth {
					walk(rel, depth+1, rules)
					if truncated {
						return
					}
				}
				continue
			}
			out = append(out, rel)
		}
	}
	walk("", 0, nil)
	return out, truncated
}

// joinRel joins a slash-separated relative path onto the root using the
// operating system's separator.
func joinRel(root, rel string) string {
	if rel == "" {
		return root
	}
	return root + string(os.PathSeparator) + strings.ReplaceAll(path.Clean(rel), "/", string(os.PathSeparator))
}

// Describer renders the environment section for a fixed root and reuses the
// result for cacheTTL. The agent calls it once per turn, so without the cache
// a large workspace would be re-walked on every message.
type Describer struct {
	root string

	mu   sync.Mutex
	text string
	at   time.Time
	now  func() time.Time // overridden by tests
}

// NewDescriber binds a describer to a workspace root.
func NewDescriber(root string) *Describer {
	return &Describer{root: root, now: time.Now}
}

// Describe returns the environment section, rebuilding it when the cached
// copy has expired.
func (d *Describer) Describe() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if !d.at.IsZero() && now.Sub(d.at) < cacheTTL {
		return d.text
	}
	d.text = Describe(d.root)
	d.at = now
	return d.text
}
