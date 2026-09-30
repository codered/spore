package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

// The spore-managed policy block. Rules accepted with "always this pattern"
// are written between these markers; everything outside them is the user's,
// and is preserved byte for byte.
const (
	ManagedBegin = "# >>> spore-managed policy — written by \"always allow this pattern\"; edit or delete freely"
	ManagedEnd   = "# <<< spore-managed policy"
)

// learnMu serialises rewrites of the config file. LearnRule is a
// read-modify-write, and the agent dispatches read-only tool calls
// concurrently, so two "always this pattern" answers arriving at once would
// otherwise interleave and silently drop one of the rules. This guards one
// process; spore is a single daemon, so a file lock is not warranted.
var learnMu sync.Mutex

// ErrNotLearned is UnlearnRule's answer for a rule the managed block does not
// hold under that decision. Hand-written rules are never in the block, so
// they are refused with it too.
var ErrNotLearned = errors.New("rule is not in the spore-managed policy block")

// LearnRule adds one rule to the managed block of a config file, creating the
// block if it is absent. Existing learned rules are preserved and duplicates
// are collapsed. Safe for concurrent callers.
func LearnRule(path, decision, rule string) error {
	if err := checkDecision(decision); err != nil {
		return err
	}
	// The block is rendered with basic TOML strings, so a rule containing a
	// quote, a backslash or a newline is refused rather than escaped.
	if strings.ContainsAny(rule, "\"\\\n\r") {
		return fmt.Errorf("learned rule %q contains characters that cannot be written to config", rule)
	}
	if strings.TrimSpace(rule) == "" {
		return fmt.Errorf("learned rule is empty")
	}
	return rewriteLearned(path, func(l *LearnedPolicy) error {
		list := l.list(decision)
		*list = appendUnique(*list, rule)
		return nil
	})
}

// UnlearnRule removes one rule from the managed block. The rule is compared
// with surrounding whitespace ignored, because the policy engine shows rules
// trimmed and that is the text a revoke sends back. A rule the block does not
// hold under that decision is ErrNotLearned, and nothing is written.
func UnlearnRule(path, decision, rule string) error {
	if err := checkDecision(decision); err != nil {
		return err
	}
	want := strings.TrimSpace(rule)
	return rewriteLearned(path, func(l *LearnedPolicy) error {
		list := l.list(decision)
		i := slices.IndexFunc(*list, func(v string) bool { return strings.TrimSpace(v) == want })
		if i < 0 {
			return ErrNotLearned
		}
		*list = slices.Delete(*list, i, i+1)
		return nil
	})
}

// ReadLearned returns the rules in the managed block, or none when the file
// has no block. It is what a running daemon re-reads after it rewrites the
// block: the rest of the file is never read again after startup.
func ReadLearned(path string) (LearnedPolicy, error) {
	learnMu.Lock()
	defer learnMu.Unlock()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return LearnedPolicy{}, fmt.Errorf("read config: %w", err)
	}
	body := string(raw)
	if err := checkMarkers(path, body); err != nil {
		return LearnedPolicy{}, err
	}
	_, existing, _, found := splitManaged(body)
	if !found {
		return LearnedPolicy{}, nil
	}
	return parseManaged(existing)
}

func checkDecision(decision string) error {
	switch decision {
	case "allow", "ask", "deny":
		return nil
	}
	return fmt.Errorf("learned rule decision must be allow, ask or deny, got %q", decision)
}

// list returns the slice a decision's rules live in, so an edit can change it
// in place. The decision has already been checked.
func (l *LearnedPolicy) list(decision string) *[]string {
	switch decision {
	case "allow":
		return &l.Allow
	case "ask":
		return &l.Ask
	}
	return &l.Deny
}

// More than one marker means the file has a stale or half-deleted block, or
// the marker text inside the user's own prose. splitManaged would pair the
// wrong two, so refuse with something the user can act on rather than
// rewriting around it.
func checkMarkers(path, body string) error {
	if n := strings.Count(body, ManagedBegin); n > 1 {
		return fmt.Errorf("%s contains %d spore-managed policy markers; remove all but one block by hand before spore can learn new rules", path, n)
	}
	return nil
}

// parseManaged decodes the managed block on its own: it is the only part of
// the file spore rewrites, so a syntax error elsewhere cannot be made worse
// by a write.
func parseManaged(inner string) (LearnedPolicy, error) {
	var doc struct {
		Policy struct {
			Learned LearnedPolicy `toml:"learned"`
		} `toml:"policy"`
	}
	if _, err := toml.Decode(inner, &doc); err != nil {
		return LearnedPolicy{}, fmt.Errorf("the spore-managed policy block is not valid TOML: %w", err)
	}
	return doc.Policy.Learned, nil
}

// rewriteLearned is the read-modify-write both LearnRule and UnlearnRule
// are. edit changes the parsed block; an error from it aborts with nothing
// written.
func rewriteLearned(path string, edit func(*LearnedPolicy) error) error {
	learnMu.Lock()
	defer learnMu.Unlock()

	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	body := string(raw)
	if err := checkMarkers(path, body); err != nil {
		return err
	}

	before, existing, after, found := splitManaged(body)
	learned := LearnedPolicy{}
	if found {
		if learned, err = parseManaged(existing); err != nil {
			return err
		}
	}
	if err := edit(&learned); err != nil {
		return err
	}

	block := renderManaged(learned)
	var out string
	if found {
		out = before + block + after
	} else {
		out = strings.TrimRight(body, "\n") + "\n\n" + block
	}

	// Never replace the user's config with something that will not load. This
	// turns any future bug in this function into a refusal instead of an agent
	// that cannot start — the user's own file is the thing at stake.
	var probe Config
	if _, err := toml.Decode(out, &probe); err != nil {
		return fmt.Errorf("refusing to write %s: the result would not parse as TOML (%w)", path, err)
	}

	// Write through a temp file in the same directory so an interrupted
	// write cannot leave a half-rewritten config behind. The result is 0600:
	// a config may name a literal API key, so the first learned rule tightens
	// a world- or group-readable file rather than preserving its mode.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.toml")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(out); err != nil {
		_ = tmp.Close()
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

func appendUnique(list []string, v string) []string {
	for _, existing := range list {
		if existing == v {
			return list
		}
	}
	return append(list, v)
}

// splitManaged returns the text before the managed block, the block's inner
// body, the text after it, and whether a block was found.
func splitManaged(body string) (before, inner, after string, found bool) {
	i := strings.Index(body, ManagedBegin)
	if i < 0 {
		return body, "", "", false
	}
	rest := body[i+len(ManagedBegin):]
	j := strings.Index(rest, ManagedEnd)
	if j < 0 {
		return body, "", "", false
	}
	return body[:i], rest[:j], rest[j+len(ManagedEnd):], true
}

func renderManaged(l LearnedPolicy) string {
	var b strings.Builder
	b.WriteString(ManagedBegin)
	b.WriteString("\n[policy.learned]\n")
	writeList := func(name string, vs []string) {
		b.WriteString(name + " = [")
		for i, v := range vs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("\"" + v + "\"")
		}
		b.WriteString("]\n")
	}
	writeList("allow", l.Allow)
	writeList("ask", l.Ask)
	writeList("deny", l.Deny)
	b.WriteString(ManagedEnd)
	b.WriteString("\n")
	return b.String()
}

// assignsKey reports whether a trimmed config line assigns to key. Matching a
// bare prefix is not enough: "enabled_at" starts with "enabled", and
// overwriting the wrong line would silently change a setting the operator did
// not ask spore to touch.
func assignsKey(trimmed, key string) bool {
	if !strings.HasPrefix(trimmed, key) {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(trimmed[len(key):]), "=")
}

// setSectionKey rewrites one key inside one section of a TOML file, adding
// the key or the whole section when either is missing. It edits a single line
// and leaves every other byte alone, comments included: a setup verb has no
// business reformatting a file the operator maintains by hand. literal is
// written as-is, so callers pass an already-quoted string or a bare number or
// bool.
//
// It rewrites an existing section rather than appending a second one of the
// same name: duplicate sections make the file fail to load, which would turn
// a successful setup into a broken install.
func setSectionKey(path, section, key, literal string) error {
	body, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	lines := strings.Split(string(body), "\n")
	header := "[" + section + "]"
	setting := key + " = " + literal

	inSection, replaced, sectionAt := false, false, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inSection = trimmed == header
			if inSection {
				sectionAt = i
			}
			continue
		}
		if inSection && assignsKey(trimmed, key) {
			lines[i] = setting
			replaced = true
		}
	}
	switch {
	case replaced:
	case sectionAt >= 0:
		rest := append([]string{setting}, lines[sectionAt+1:]...)
		lines = append(lines[:sectionAt+1], rest...)
	default:
		lines = append(lines, "", header, setting, "")
	}
	//nolint:gosec // G703: path is from the config file and validated
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// SetRecallBackend records the backend `spore recall setup` provisioned.
func SetRecallBackend(path, backend string) error {
	learnMu.Lock()
	defer learnMu.Unlock()
	switch backend {
	case RecallSQLiteFTS, RecallWeaviate:
	default:
		return fmt.Errorf("recall backend must be %s or %s, got %q", RecallSQLiteFTS, RecallWeaviate, backend)
	}
	return setSectionKey(path, "recall", "backend", strconv.Quote(backend))
}

// SetTraceEnabled records whether `spore trace setup` left tracing on. It is
// the only key the trace verbs write: endpoint, sample_rate and redact stay
// the operator's.
func SetTraceEnabled(path string, enabled bool) error {
	learnMu.Lock()
	defer learnMu.Unlock()
	return setSectionKey(path, "trace", "enabled", strconv.FormatBool(enabled))
}
