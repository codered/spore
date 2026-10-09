package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// The spore-managed routing block. /model writes the daemon-wide choices
// here, apart from the policy block, so neither rewrite can disturb the
// other and the hand-written [[route]] rules stay the operator's.
const (
	RoutingBegin = "# >>> spore-managed routing — written by /model; edit or delete freely"
	RoutingEnd   = "# <<< spore-managed routing"
)

// SetRoutingOverride records ref as the model for a daemon-wide call site;
// "" removes it. The block is created on first use and removed when its
// last entry goes, so a file whose overrides were all cleared reads as it
// did before /model touched it.
func SetRoutingOverride(path, site, ref string) error {
	if !slices.Contains(RoutingOverrideSites, site) {
		return fmt.Errorf("%q is not a call site /model sets daemon-wide", site)
	}
	if ref != "" {
		if err := ValidateModelRef(ref); err != nil {
			return err
		}
		// Rendered as a basic TOML string, so refuse what would need escaping.
		if strings.ContainsAny(ref, "\"\\\n\r") {
			return fmt.Errorf("model ref %q contains characters that cannot be written to config", ref)
		}
	}

	learnMu.Lock()
	defer learnMu.Unlock()

	raw, err := os.ReadFile(path) //nolint:gosec // G304: path is from the config file and validated
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	body := string(raw)
	if n := strings.Count(body, RoutingBegin); n > 1 {
		return fmt.Errorf("%s contains %d spore-managed routing markers; remove all but one block by hand", path, n)
	}

	before, inner, after, found := splitBlock(body, RoutingBegin, RoutingEnd)
	over := map[string]string{}
	if found {
		if over, err = parseRouting(inner); err != nil {
			return err
		}
	}
	if ref == "" {
		if _, ok := over[site]; !ok {
			return nil // nothing to clear; leave the file alone
		}
		delete(over, site)
	} else {
		over[site] = ref
	}

	block := renderRouting(over)
	var out string
	switch {
	case found && block == "":
		out = strings.TrimRight(before, "\n") + "\n"
		if rest := strings.TrimLeft(after, "\n"); rest != "" {
			out += "\n" + rest
		}
	case found:
		out = before + block + strings.TrimPrefix(after, "\n")
	default:
		out = strings.TrimRight(body, "\n") + "\n\n" + block
	}
	return writeConfigFile(path, out)
}

func parseRouting(inner string) (map[string]string, error) {
	var doc struct {
		Routing RoutingConfig `toml:"routing"`
	}
	if _, err := toml.Decode(inner, &doc); err != nil {
		return nil, fmt.Errorf("the spore-managed routing block is not valid TOML: %w", err)
	}
	if doc.Routing.Override == nil {
		return map[string]string{}, nil
	}
	return doc.Routing.Override, nil
}

// renderRouting returns the whole block, markers included, or "" for no
// overrides. Keys are sorted so a rewrite never reorders lines needlessly.
func renderRouting(over map[string]string) string {
	if len(over) == 0 {
		return ""
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(RoutingBegin)
	b.WriteString("\n[routing.override]\n")
	for _, k := range keys {
		b.WriteString(k + " = \"" + over[k] + "\"\n")
	}
	b.WriteString(RoutingEnd)
	b.WriteString("\n")
	return b.String()
}
