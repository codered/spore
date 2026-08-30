package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// The spore-managed policy block. Rules accepted with "always this pattern"
// are written between these markers; everything outside them is the user's,
// and is preserved byte for byte.
const (
	ManagedBegin = "# >>> spore-managed policy — written by \"always allow this pattern\"; edit or delete freely"
	ManagedEnd   = "# <<< spore-managed policy"
)

// LearnRule adds one rule to the managed block of a config file, creating the
// block if it is absent. Existing learned rules are preserved and duplicates
// are collapsed.
func LearnRule(path, decision, rule string) error {
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

	// Write through a temp file in the same directory so an interrupted
	// write cannot leave a half-rewritten config behind.
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
