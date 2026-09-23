package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/subagent"
)

// agentsRes lists the sub-agents the selected session launched.
type agentsRes struct{}

// agentRow carries the parent a stop must be sent through.
type agentRow struct {
	subagent.Status
	parent string
}

func (agentsRes) Name() string   { return "agents" }
func (agentsRes) Hotkey() string { return "A" }
func (agentsRes) Scoped() bool   { return true }
func (agentsRes) Columns() []Column {
	return []Column{
		{Title: "ID", Min: 8},
		{Title: "STATE", Min: 11},
		{Title: "AGE", Min: 4, Right: true},
		{Title: "COST", Min: 8, Right: true},
		{Title: "PROMPT", Min: 10, Flex: true},
	}
}

func (agentsRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
	list, err := v.Agents(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(list.Agents))
	for _, a := range list.Agents {
		ran := clock().Sub(a.Started)
		if !a.Ended.IsZero() {
			ran = a.Ended.Sub(a.Started)
		}
		id := a.ID
		if len(id) > 8 {
			id = id[:8]
		}
		rows = append(rows, Row{
			ID:    a.ID,
			Cells: []string{id, a.State, age(ran), fmt.Sprintf("$%.4f", a.CostUSD), a.Prompt},
			Data:  agentRow{Status: a, parent: sessionID},
		})
	}
	return rows, nil
}

func (agentsRes) Detail(r Row) string {
	a, ok := r.Data.(agentRow)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sub-agent %s\nstate: %s\ncost: $%.4f\nstarted: %s\n", a.ID, a.State, a.CostUSD, a.Started.UTC().Format(time.RFC3339))
	if !a.Ended.IsZero() {
		fmt.Fprintf(&b, "ended: %s\n", a.Ended.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "\nprompt:\n%s\n", a.Prompt)
	if a.Result != "" {
		fmt.Fprintf(&b, "\nresult:\n%s\n", a.Result)
	}
	if a.Error != "" {
		fmt.Fprintf(&b, "\nerror:\n%s\n", a.Error)
	}
	return b.String()
}

func (agentsRes) Actions() []Action {
	return []Action{{
		Key:   "x",
		Label: "stop",
		Applies: func(r Row) bool {
			a, ok := r.Data.(agentRow)
			return ok && a.State == "running"
		},
		Confirm: func(r Row) string { return "stop sub-agent " + short(r.ID) + "?" },
		Run: func(ctx context.Context, v Views, r Row) error {
			a := r.Data.(agentRow)
			return v.CancelAgent(ctx, a.parent, a.ID)
		},
	}}
}

// Open selects the child's own session.
func (agentsRes) Open(r Row) string { return r.ID }
