// Package skill owns spore's skill files: the human-editable markdown that
// the model may pull into a turn on demand. The file is the source of truth --
// nothing else stores a skill -- so this package is filesystem-only: it holds
// no database handle and knows nothing about sessions.
//
// The skills directory sits outside policy.Workspace, so the filesystem tools
// cannot reach it and this package does its own confinement instead. That is
// what makes the directory read-only by construction: the one write path is
// the skill_install tool, which is ask-gated and never learned into a rule.
package skill

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

// Skill is one directory holding a SKILL.md. A skill carries no token count:
// sizing belongs to the estimator in internal/agent, and a filesystem package
// must not depend on the agent to describe a file.
type Skill struct {
	Name        string
	Description string
	Body        string
	Path        string
}

// ErrReadDir is a sentinel for directory-level read failures. Load returns it
// wrapped with %w when it cannot read the directory (permission denied,
// unmounted volume). Cache uses it to tell a transient directory error from a
// per-file one, so a blip never blanks the loaded set.
var ErrReadDir = errors.New("read skills dir")

// nameRE repeats memory's rule rather than importing it: three lines is the
// better trade against a dependency between two independent filesystem
// packages, and the rule is security-relevant here because skill_load turns a
// model-supplied string into a path.
var nameRE = regexp.MustCompile(`\A[a-z0-9]+(-[a-z0-9]+)*\z`)

// ValidName reports whether a name may be turned into a path.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("skill name %q must be lowercase kebab-case (letters, digits and single hyphens)", name)
	}
	return nil
}

// Dir is the only place a name becomes a directory path.
func Dir(dir, name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// Validate checks a skill is safe to write and complete enough to be useful.
func (s Skill) Validate() error {
	if err := ValidName(s.Name); err != nil {
		return err
	}
	if strings.TrimSpace(s.Description) == "" {
		return errors.New("skill description is required: it is all the model sees until the skill is loaded")
	}
	if strings.ContainsAny(s.Description, "\r\n") {
		return errors.New("skill description must be a single line")
	}
	if strings.TrimSpace(s.Body) == "" {
		return errors.New("skill body is required")
	}
	return nil
}

// Load reads every skill in dir, sorted by name. Errors are per-file and are
// returned alongside the skills that did parse: a human edits these by hand,
// so one broken file must cost exactly one skill and never a whole turn. A
// missing directory is zero skills and no error.
func Load(dir string) ([]Skill, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, []error{fmt.Errorf("%w %s: %v", ErrReadDir, dir, err)}
	}
	var skills []Skill
	var errs []error
	for _, e := range entries {
		// A loose file is not a skill, and a dotted directory holds the
		// temporary file Write renames from -- neither is loadable.
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, e.Name(), "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // a directory with no SKILL.md is not a skill
			}
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		s, err := parse(string(data))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.Name(), err))
			continue
		}
		// The directory name is the identity skill_load uses, so frontmatter
		// that disagrees with it is a defect, not a preference.
		if s.Name != e.Name() {
			errs = append(errs, fmt.Errorf("%s: frontmatter name %q does not match the directory name", e.Name(), s.Name))
			continue
		}
		s.Path = path
		skills = append(skills, s)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, errs
}

// Read loads one skill by name. This is the path skill_load takes, so the
// name is validated before it reaches the filesystem.
func Read(dir, name string) (Skill, error) {
	d, err := Dir(dir, name)
	if err != nil {
		return Skill{}, err
	}
	path := filepath.Join(d, "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Skill{}, fmt.Errorf("no skill named %q", name)
		}
		return Skill{}, err
	}
	s, err := parse(string(data))
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", name, err)
	}
	if s.Name != name {
		return Skill{}, fmt.Errorf("%s: frontmatter name %q does not match the directory name", name, s.Name)
	}
	s.Path = path
	return s, nil
}

// Exists reports whether a skill of this name is already on disk, so an
// install can tell a human it is about to update rather than create.
func Exists(dir, name string) bool {
	d, err := Dir(dir, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(d, "SKILL.md"))
	return err == nil
}

// parse reads the fixed two-key frontmatter. This is not YAML and does not
// pretend to be: two known keys do not justify a dependency, and a hand
// parser gives error messages that name the actual problem.
func parse(text string) (Skill, error) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return Skill{}, errors.New("missing opening --- frontmatter delimiter")
	}
	head, body, ok := strings.Cut(rest, "\n---")
	if !ok {
		return Skill{}, errors.New("missing closing --- frontmatter delimiter")
	}
	var s Skill
	for _, line := range strings.Split(head, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return Skill{}, fmt.Errorf("frontmatter line %q is not key: value", line)
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "name":
			s.Name = value
		case "description":
			s.Description = value
		default:
			return Skill{}, fmt.Errorf("unknown frontmatter key %q", strings.TrimSpace(key))
		}
	}
	s.Body = strings.TrimSpace(body)
	if err := s.Validate(); err != nil {
		return Skill{}, err
	}
	return s, nil
}

// Render is the on-disk form. Write and the tests both go through it so the
// format has exactly one definition.
func Render(s Skill) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(s.Name)
	b.WriteString("\ndescription: ")
	b.WriteString(s.Description)
	b.WriteString("\n---\n\n")
	b.WriteString(strings.TrimSpace(s.Body))
	b.WriteString("\n")
	return b.String()
}

// Write validates, then replaces SKILL.md atomically so a reader never sees a
// half-written skill. The temporary file is a dotfile in the skill's own
// directory, which Load skips, so an orphan left by a crash is never loaded.
func Write(dir string, s Skill) error {
	if err := s.Validate(); err != nil {
		return err
	}
	d, err := Dir(dir, s.Name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(d, ".SKILL-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(Render(s)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(d, "SKILL.md"))
}
