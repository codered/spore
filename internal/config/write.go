package config

import (
	"fmt"
	"os"
	"path/filepath"
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

// LearnRule adds one rule to the managed block of a config file, creating the
// block if it is absent. Existing learned rules are preserved and duplicates
// are collapsed. Safe for concurrent callers.
func LearnRule(path, decision, rule string) error {
	learnMu.Lock()
	defer learnMu.Unlock()
	switch decision {
	case "allow", "ask", "deny":
	default:
		return fmt.Errorf("learned rule decision must be allow, ask or deny, got %q", decision)
	}
	// The block is rendered with basic TOML strings, so a rule containing a
	// quote, a backslash or a newline is refused rather than escaped.
	if strings.ContainsAny(rule, "\"\\\n\r") {
		return fmt.Errorf("learned rule %q contains characters that cannot be written to config", rule)
	}
	if strings.TrimSpace(rule) == "" {
		return fmt.Errorf("learned rule is empty")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	body := string(raw)

	// More than one marker means the file has a stale or half-deleted block,
	// or the marker text inside the user's own prose. splitManaged would pair
	// the wrong two, so refuse with something the user can act on rather than
	// rewriting around it.
	if n := strings.Count(body, ManagedBegin); n > 1 {
		return fmt.Errorf("%s contains %d spore-managed policy markers; remove all but one block by hand before spore can learn new rules", path, n)
	}

	before, existing, after, found := splitManaged(body)
	learned := LearnedPolicy{}
	if found {
		// The managed block is parsed on its own: it is the only part of the
		// file spore rewrites, so a syntax error elsewhere cannot be made
		// worse by this write.
		var doc struct {
			Policy struct {
				Learned LearnedPolicy `toml:"learned"`
			} `toml:"policy"`
		}
		if _, err := toml.Decode(existing, &doc); err != nil {
			return fmt.Errorf("the spore-managed policy block is not valid TOML: %w", err)
		}
		learned = doc.Policy.Learned
	}

	switch decision {
	case "allow":
		learned.Allow = appendUnique(learned.Allow, rule)
	case "ask":
		learned.Ask = appendUnique(learned.Ask, rule)
	case "deny":
		learned.Deny = appendUnique(learned.Deny, rule)
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
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(out); err != nil {
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
