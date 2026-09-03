// Package memory owns spore's fact files: the human-editable markdown under
// <data_dir>/memory that is loaded into every turn. The file is the source of
// truth -- nothing else stores a fact -- so this package is filesystem-only:
// it holds no database handle and knows nothing about sessions.
//
// The directory sits outside policy.Workspace, so the filesystem tools cannot
// reach it and this package does its own confinement instead.
package memory

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Fact is one file. A fact carries no token count: sizing belongs to the
// estimator in internal/agent, and a filesystem package must not depend on
// the agent to describe a file.
type Fact struct {
	Name        string
	Description string
	Type        string
	Body        string
	Path        string
}

// Types is the closed set a fact may declare. A closed set keeps the system
// block's headings predictable and catches typos at write time.
var Types = []string{"user", "feedback", "project", "reference"}

// ErrReadDir is a sentinel error for directory-level read failures. Load returns
// this wrapped with %w when it cannot read the directory (permission denied,
// unmounted, etc.). Reload uses this to distinguish transient directory errors
// from per-file errors, so it preserves cached facts on directory-level failures.
var ErrReadDir = errors.New("read memory dir")

// nameRE is deliberately narrower than "a legal filename": lowercase kebab
// only. The model chooses this string, and it becomes a path, so anything
// that could traverse, collide case-insensitively, or need quoting is out.
var nameRE = regexp.MustCompile(`\A[a-z0-9]+(-[a-z0-9]+)*\z`)

// ValidName reports whether a name may be turned into a path.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("fact name %q must be lowercase kebab-case (letters, digits and single hyphens)", name)
	}
	return nil
}

func validType(t string) error {
	for _, k := range Types {
		if t == k {
			return nil
		}
	}
	return fmt.Errorf("fact type %q must be one of %s", t, strings.Join(Types, ", "))
}

// Validate checks a fact is safe to write and complete enough to be useful.
func (f Fact) Validate() error {
	if err := ValidName(f.Name); err != nil {
		return err
	}
	if err := validType(f.Type); err != nil {
		return err
	}
	if strings.TrimSpace(f.Description) == "" {
		return errors.New("fact description is required: it is what the model sees when the body does not fit the budget")
	}
	if strings.ContainsAny(f.Description, "\r\n") {
		return errors.New("fact description must be a single line")
	}
	if strings.TrimSpace(f.Body) == "" {
		return errors.New("fact body is required")
	}
	return nil
}

// Path is the only place a name becomes a filename.
func Path(dir, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".md"), nil
}

// Load reads every fact in dir, sorted by name. Errors are per-file and are
// returned alongside the facts that did parse: a human edits these by hand,
// so one broken file must cost exactly one fact and never a whole turn. A
// missing directory is zero facts and no error.
func Load(dir string) ([]Fact, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("%w %s: %v", ErrReadDir, dir, err)}
	}
	var facts []Fact
	var errs []error
	for _, e := range entries {
		// No recursion: a scratch subdirectory must never become context.
		// Skip dotfiles: Write uses temporary files with dotfile names to remain
		// invisible to Load, and any orphaned temp (e.g. if process dies mid-write)
		// must not be loaded. This also guards against future temp file patterns.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		f, err := parse(string(data))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		// The filename is the identity used by Delete and by the recall index,
		// so frontmatter that disagrees with it is a defect, not a preference.
		if want := strings.TrimSuffix(e.Name(), ".md"); f.Name != want {
			errs = append(errs, fmt.Errorf("%s: frontmatter name %q does not match the filename", e.Name(), f.Name))
			continue
		}
		f.Path = path
		facts = append(facts, f)
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].Name < facts[j].Name })
	return facts, errs
}

// parse reads the fixed three-key frontmatter. This is not YAML and does not
// pretend to be: three known keys do not justify a dependency, and a hand
// parser gives error messages that name the actual problem.
func parse(text string) (Fact, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Fact{}, errors.New("missing opening --- frontmatter delimiter")
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Fact{}, errors.New("missing closing --- frontmatter delimiter")
	}
	var f Fact
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Fact{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			f.Name = value
		case "description":
			f.Description = value
		case "type":
			f.Type = value
		default:
			return Fact{}, fmt.Errorf("unknown frontmatter key %q", strings.TrimSpace(key))
		}
	}
	f.Body = strings.TrimSpace(body)
	if err := f.Validate(); err != nil {
		return Fact{}, err
	}
	return f, nil
}

// Render is the on-disk form. Write and the tests both go through it so the
// format has exactly one definition.
func Render(f Fact) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(f.Name)
	b.WriteString("\ndescription: ")
	b.WriteString(f.Description)
	b.WriteString("\ntype: ")
	b.WriteString(f.Type)
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimSpace(f.Body))
	b.WriteString("\n")
	return b.String()
}

// Write validates, then replaces the file atomically so a reader never sees a
// half-written fact.
func Write(dir string, f Fact) error {
	if err := f.Validate(); err != nil {
		return err
	}
	path, err := Path(dir, f.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create memory dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".fact-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(Render(f)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Delete removes one fact. A name that does not exist is an error the model
// can read and correct, not a silent success.
func Delete(dir, name string) error {
	path, err := Path(dir, name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("no fact named %q", name)
		}
		return err
	}
	return nil
}
