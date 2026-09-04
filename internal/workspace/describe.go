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
	// maxPerDir bounds how many entries one directory below the root may
	// contribute. Depth alone does not bound a walk: a single cache
	// directory holds more files than the whole budget, and without this cap
	// it spends the budget before any sibling is reached. The root itself is
	// exempt -- it is the directory the operator asked about, and a flat
	// repository with many files at the top deserves the whole budget.
	maxPerDir = 40
	// cacheTTL is how long a rendered listing is reused. The working tree
	// changes during a session -- often because the agent changed it -- so
	// the listing must not be frozen for the life of the process, but it
	// need not be rebuilt for every turn either.
	cacheTTL = 30 * time.Second
)

// noiseDirs are never descended into or listed. .git is excluded because git
// itself never reports it; the rest are the build, dependency and cache trees
// that would otherwise consume the whole entry budget. The caches are named
// here because a workspace is not always a repository: a home directory has
// no .gitignore to exclude them, and they are the largest trees on a
// developer's machine by a wide margin.
var noiseDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	".cache": true, ".cargo": true, ".rustup": true, ".npm": true,
	".nvm": true, ".gradle": true, ".m2": true, ".pyenv": true,
	".local": true, "__pycache__": true, ".mypy_cache": true,
	".pytest_cache": true, ".tox": true, ".next": true,
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
	entries, truncated, filtered := list(root)

	var b strings.Builder
	b.WriteString("\n\n## Environment\n\nWorking directory: ")
	b.WriteString(root)
	b.WriteString("\n")
	if len(entries) == 0 {
		b.WriteString("\nThe working directory is empty.\n")
		return b.String()
	}
	// Only claim the .gitignore filtering when a .gitignore was actually
	// read. A workspace that is not a repository has none, and telling the
	// model the listing is filtered when nothing filtered it is a lie it
	// cannot check.
	if filtered {
		b.WriteString("\nFiles here, excluding anything .gitignore covers. " +
			"This is a listing only: read a file when you need its contents.\n\n")
	} else {
		b.WriteString("\nFiles here. " +
			"This is a listing only: read a file when you need its contents.\n\n")
	}
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

// list walks root breadth-first and returns the paths to show, relative to
// root, directories marked with a trailing slash. Breadth first is the point:
// a depth-first walk in sorted order spends the whole entry budget inside
// whichever subtree sorts earliest, so the operator's own top-level files
// never appear. The paths are sorted before returning, which puts them back
// in tree order for reading.
//
// The second result reports whether the entry budget cut the walk short. The
// third reports whether any .gitignore was read, so the caller can describe
// the listing honestly.
func list(root string) ([]string, bool, bool) {
	type frame struct {
		dir   string
		depth int
		rules ignoreStack
	}

	var out []string
	truncated := false
	filtered := false

	queue := []frame{{}}
	for len(queue) > 0 && !truncated {
		f := queue[0]
		queue = queue[1:]

		rules, loaded := f.rules.load(root, f.dir)
		if loaded {
			filtered = true
		}
		entries, err := os.ReadDir(joinRel(root, f.dir))
		if err != nil {
			continue // an unreadable directory is skipped, not fatal
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		kept := entries[:0]
		for _, e := range entries {
			if noiseDirs[e.Name()] {
				continue
			}
			if !e.IsDir() && !e.Type().IsRegular() {
				continue // sockets, devices and dangling links are not content
			}
			rel := e.Name()
			if f.dir != "" {
				rel = f.dir + "/" + rel
			}
			if rules.ignored(rel, e.IsDir()) {
				continue
			}
			kept = append(kept, e)
		}

		show := len(kept)
		if f.depth > 0 && show > maxPerDir {
			show = maxPerDir
		}
		for _, e := range kept[:show] {
			if len(out) >= maxEntries {
				truncated = true
				break
			}
			rel := e.Name()
			if f.dir != "" {
				rel = f.dir + "/" + rel
			}
			if !e.IsDir() {
				out = append(out, rel)
				continue
			}
			out = append(out, rel+"/")
			if f.depth+1 < maxDepth {
				queue = append(queue, frame{dir: rel, depth: f.depth + 1, rules: rules})
			}
		}
		if truncated || show == len(kept) {
			continue
		}
		// The cap hid entries the operator may care about. Say how many, so
		// the model knows to reach for fs_list rather than assuming the
		// directory holds what it can see.
		if len(out) >= maxEntries {
			truncated = true
			break
		}
		marker := "... and " + itoa(len(kept)-show) + " more entries"
		if f.dir != "" {
			marker = f.dir + "/" + marker
		}
		out = append(out, marker)
	}

	sort.Strings(out)
	return out, truncated, filtered
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
