// Package modelcmd is /model's text form and the option order every
// surface uses, so the TUI, the web UI, Discord and plain chat number and
// mark the same list the same way.
package modelcmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// Option is one choice for an op.
type Option struct {
	Ref       string
	Selected  bool
	Default   bool
	Available bool
}

// Choosable reports whether picking it can succeed: the default always can,
// because choosing it clears the override rather than naming a model.
func (o Option) Choosable() bool { return o.Available || o.Default }

func find(v models.View, op string) (models.Op, bool) {
	for _, o := range v.Ops {
		if o.Op == op {
			return o, true
		}
	}
	return models.Op{}, false
}

// Options lists an op's choices: the selected model, the default when it
// differs, then every other available model by provider.
func Options(v models.View, op string) []Option {
	o, ok := find(v, op)
	if !ok {
		return nil
	}
	avail := map[string]bool{}
	for _, g := range v.Groups {
		for _, r := range g.Refs {
			avail[r] = true
		}
	}
	var out []Option
	seen := map[string]bool{}
	add := func(ref string) {
		if ref == "" || seen[ref] {
			return
		}
		seen[ref] = true
		out = append(out, Option{Ref: ref, Selected: ref == o.Selected, Default: ref == o.Default, Available: avail[ref]})
	}
	add(o.Selected)
	add(o.Default)
	for _, g := range v.Groups {
		for _, r := range g.Refs {
			add(r)
		}
	}
	return out
}

func where(scope string) string {
	if scope == models.ScopeGlobal {
		return "every session"
	}
	return "this session"
}

// Overview is one line per op: what it runs on, and the default when that
// differs.
func Overview(v models.View) string {
	var b strings.Builder
	for _, o := range v.Ops {
		if o.Selected == o.Default {
			fmt.Fprintf(&b, "  %-10s -> %s *  (%s)\n", o.Op, o.Selected, where(o.Scope))
		} else {
			fmt.Fprintf(&b, "  %-10s -> %s  (%s; * %s)\n", o.Op, o.Selected, where(o.Scope), o.Default)
		}
	}
	return b.String()
}

// Render is the whole text form: the overview, each op's numbered list,
// and any provider that could not be listed.
func Render(v models.View) string {
	var b strings.Builder
	b.WriteString("Models per operation (-> selected, * default):\n")
	b.WriteString(Overview(v))
	for _, o := range v.Ops {
		fmt.Fprintf(&b, "\n%s (%s):\n", o.Op, where(o.Scope))
		for i, opt := range Options(v, o.Op) {
			mark := "  "
			if opt.Selected {
				mark = "->"
			}
			line := fmt.Sprintf("  %2d %s %s", i+1, mark, opt.Ref)
			if opt.Default {
				line += " *"
			}
			if !opt.Available {
				line += " (unavailable)"
			}
			b.WriteString(line + "\n")
		}
	}
	for _, g := range v.Groups {
		if g.Error != "" {
			fmt.Fprintf(&b, "\n  ! %s: %s\n", g.Provider, g.Error)
		}
	}
	b.WriteString("\nChoose with /model <operation> <number>.\n")
	return b.String()
}

const usage = "usage: /model, or /model <operation> <number> (operations: chat, compaction, title, classify, refinement, subagent)"

// Parse reads the words after /model: none shows the list; an operation and
// a number choose. A model ref is refused on purpose: choices come from the
// list.
func Parse(args []string) (op string, n int, err error) {
	if len(args) == 0 {
		return "", 0, nil
	}
	if len(args) != 2 || !router.ValidSite(args[0]) {
		return "", 0, fmt.Errorf("%s", usage)
	}
	n, err = strconv.Atoi(args[1])
	if err != nil || n < 1 {
		return "", 0, fmt.Errorf("%s", usage)
	}
	return args[0], n, nil
}

// Pick returns the ref of option n (1-based) for op.
func Pick(v models.View, op string, n int) (string, error) {
	opts := Options(v, op)
	if n < 1 || n > len(opts) {
		return "", fmt.Errorf("%s has options 1-%d", op, len(opts))
	}
	o := opts[n-1]
	if !o.Choosable() {
		return "", fmt.Errorf("%s is not available right now", o.Ref)
	}
	return o.Ref, nil
}

// Confirm says what op now runs on and where that applies.
func Confirm(v models.View, op string) string {
	o, _ := find(v, op)
	s := fmt.Sprintf("%s -> %s (%s)", op, o.Selected, where(o.Scope))
	if o.Selected == o.Default {
		s += ", the default"
	}
	return s + "\n"
}
