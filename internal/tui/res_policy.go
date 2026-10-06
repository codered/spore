package tui

import (
	"context"
	"fmt"
	"strconv"

	"github.com/codered/spore/internal/daemon"
)

// policyRes lists every rule in force, per profile, in evaluation order.
type policyRes struct{}

func (policyRes) Name() string   { return "policy" }
func (policyRes) Hotkey() string { return "P" }
func (policyRes) Scoped() bool   { return false }
func (policyRes) Columns() []Column {
	return []Column{
		{Title: "PROFILE", Min: 7},
		{Title: "DECISION", Min: 8},
		{Title: "SOURCE", Min: 8},
		{Title: "RULE", Min: 10, Flex: true},
	}
}

func (policyRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	p, err := v.Policy(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(p.Rules))
	seen := map[string]int{}
	for _, r := range p.Rules {
		// The same text can appear twice in one profile (written by hand and
		// learned), so the id carries a count to stay unique.
		id := r.Profile + "/" + r.Decision + "/" + r.Rule
		seen[id]++
		if n := seen[id]; n > 1 {
			id += "#" + strconv.Itoa(n)
		}
		rows = append(rows, Row{ID: id, Cells: []string{r.Profile, r.Decision, r.Source, r.Rule}, Data: r})
	}
	return rows, nil
}

func (policyRes) Detail(r Row) string {
	p, ok := r.Data.(daemon.PolicyRuleJSON)
	if !ok {
		return ""
	}
	s := fmt.Sprintf("profile: %s\ndecision: %s\nsource: %s\n\nrule:\n%s", p.Profile, p.Decision, p.Source, p.Rule)
	if p.Source != "learned" {
		s += "\n\nedit config.toml to change this rule"
	}
	return s
}

func (policyRes) Actions() []Action {
	return []Action{{
		Key:   "x",
		Label: "revoke",
		Applies: func(r Row) bool {
			p, ok := r.Data.(daemon.PolicyRuleJSON)
			return ok && p.Source == "learned"
		},
		Why: func(r Row) string {
			p, _ := r.Data.(daemon.PolicyRuleJSON)
			return fmt.Sprintf("x revokes learned rules only; this one is %s (edit config.toml), move to a learned row with j/k", p.Source)
		},
		Confirm: func(r Row) string { return fmt.Sprintf("revoke %q?", r.Data.(daemon.PolicyRuleJSON).Rule) },
		Run: func(ctx context.Context, v Views, r Row) error {
			p := r.Data.(daemon.PolicyRuleJSON)
			return v.Revoke(ctx, p.Decision, p.Rule)
		},
	}}
}
