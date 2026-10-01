package tui

import (
	"context"
	"fmt"

	"github.com/codered/spore/internal/daemon"
)

// memoryRes lists the fact files, or with a query the recall hits for it.
type memoryRes struct{ query string }

func (m memoryRes) Name() string {
	if m.query == "" {
		return "memory"
	}
	return fmt.Sprintf("memory · %q", m.query)
}
func (memoryRes) Hotkey() string { return "M" }
func (memoryRes) Scoped() bool   { return false }
func (m memoryRes) Columns() []Column {
	if m.query != "" {
		return []Column{
			{Title: "NAME", Min: 12},
			{Title: "SCORE", Min: 6, Right: true},
			{Title: "EXCERPT", Min: 10, Flex: true},
		}
	}
	return []Column{
		{Title: "NAME", Min: 12},
		{Title: "TYPE", Min: 8},
		{Title: "DESCRIPTION", Min: 10, Flex: true},
	}
}

func (m memoryRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	res, err := v.Memory(ctx, m.query)
	if err != nil {
		return nil, err
	}
	if m.query != "" {
		rows := make([]Row, 0, len(res.Hits))
		for _, h := range res.Hits {
			rows = append(rows, Row{ID: h.Name, Cells: []string{h.Name, fmt.Sprintf("%.2f", h.Score), h.Excerpt}, Data: h})
		}
		return rows, nil
	}
	rows := make([]Row, 0, len(res.Facts))
	for _, f := range res.Facts {
		rows = append(rows, Row{ID: f.Name, Cells: []string{f.Name, f.Type, f.Description}, Data: f})
	}
	return rows, nil
}

func (memoryRes) Detail(r Row) string {
	switch d := r.Data.(type) {
	case daemon.FactJSON:
		return d.Body
	case daemon.MemoryHitJSON:
		return d.Excerpt
	}
	return ""
}

func (memoryRes) Actions() []Action {
	return []Action{{
		Key:     "x",
		Label:   "delete",
		Confirm: func(r Row) string { return fmt.Sprintf("delete fact %q?", r.ID) },
		Run: func(ctx context.Context, v Views, r Row) error {
			return v.DeleteFact(ctx, r.ID)
		},
	}}
}
