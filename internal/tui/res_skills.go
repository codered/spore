package tui

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/daemon"
)

// skillsRes lists the skills available to the selected session.
type skillsRes struct{}

func (skillsRes) Name() string   { return "skills" }
func (skillsRes) Hotkey() string { return "S" }
func (skillsRes) Scoped() bool   { return true }
func (skillsRes) Columns() []Column {
	return []Column{
		{Title: "NAME", Min: 16},
		{Title: "LOADED", Min: 6},
		{Title: "TOKENS", Min: 6, Right: true},
		{Title: "DESCRIPTION", Min: 10, Flex: true},
	}
}

func (skillsRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
	list, err := v.Skills(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(list.Skills)+len(list.Errors))
	for _, s := range list.Skills {
		loaded := ""
		if s.Loaded {
			loaded = "✓"
		}
		rows = append(rows, Row{ID: s.Name, Cells: []string{s.Name, loaded, humanTokens(s.BodyTokens), s.Description}, Data: s})
	}
	for i, e := range list.Errors {
		rows = append(rows, Row{ID: fmt.Sprintf("!%d", i), Cells: []string{"!", "", "", e}, Data: e})
	}
	return rows, nil
}

func (skillsRes) Detail(r Row) string {
	switch d := r.Data.(type) {
	case daemon.SkillJSON:
		return d.Name + "\n" + d.Description + "\n\n" + d.Body
	case string:
		return "load error\n\n" + d
	}
	return ""
}

func (skillsRes) Actions() []Action { return nil }
