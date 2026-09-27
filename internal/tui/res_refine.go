package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/daemon"
)

// refinementsRes lists refinement ledger rows, proposals first: the review
// queue for edits from sessions spore does not trust, and the undo list for
// the ones it applied.
type refinementsRes struct{}

func (refinementsRes) Name() string   { return "refinements" }
func (refinementsRes) Hotkey() string { return "R" }
func (refinementsRes) Scoped() bool   { return false }
func (refinementsRes) Columns() []Column {
	return []Column{
		{Title: "ID", Min: 4, Right: true},
		{Title: "STATUS", Min: 8},
		{Title: "KIND", Min: 12},
		{Title: "TARGET", Min: 12},
		{Title: "AGE", Min: 4},
		{Title: "WHY", Min: 10, Flex: true},
	}
}

func refineTarget(x daemon.RefinementJSON) string {
	if strings.HasPrefix(x.Kind, "notes.") {
		return filepath.Base(x.Target)
	}
	return x.Target
}

func (refinementsRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	list, err := v.Refinements(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(list, func(i, j int) bool {
		pi, pj := list[i].Status == "proposed", list[j].Status == "proposed"
		if pi != pj {
			return pi
		}
		return list[i].ID > list[j].ID
	})
	rows := make([]Row, 0, len(list))
	for _, x := range list {
		id := strconv.FormatInt(x.ID, 10)
		rows = append(rows, Row{
			ID:    id,
			Cells: []string{id, x.Status, x.Kind, refineTarget(x), age(clock().Sub(x.CreatedAt)), x.Rationale},
			Data:  x,
		})
	}
	return rows, nil
}

func refineContent(p *string) string {
	if p == nil {
		return "(no file)"
	}
	return *p
}

func (refinementsRes) Detail(r Row) string {
	x, ok := r.Data.(daemon.RefinementJSON)
	if !ok {
		return ""
	}
	return fmt.Sprintf("refinement %d — %s %s (%s)\nsession: %s  round: %s  trigger: %s\nwhy: %s\n\n--- before\n%s\n\n+++ after\n%s\n",
		x.ID, x.Kind, x.Target, x.Status, x.SessionID, x.RoundID, x.Trigger, x.Rationale,
		refineContent(x.Before), refineContent(x.After))
}

func refineStatus(status string) func(Row) bool {
	return func(r Row) bool {
		x, ok := r.Data.(daemon.RefinementJSON)
		return ok && x.Status == status
	}
}

func (refinementsRes) Actions() []Action {
	return []Action{
		{
			Key: "a", Label: "accept",
			Applies: refineStatus("proposed"),
			Run: func(ctx context.Context, v Views, r Row) error {
				return v.AcceptRefinement(ctx, r.Data.(daemon.RefinementJSON).ID)
			},
		},
		{
			Key: "r", Label: "reject",
			Applies: refineStatus("proposed"),
			Confirm: func(r Row) string { return "reject refinement " + r.ID + "?" },
			Run: func(ctx context.Context, v Views, r Row) error {
				return v.RejectRefinement(ctx, r.Data.(daemon.RefinementJSON).ID)
			},
		},
		{
			Key: "x", Label: "roll back round",
			Applies: refineStatus("applied"),
			Confirm: func(r Row) string {
				return "roll back every edit in round " + r.Data.(daemon.RefinementJSON).RoundID + "?"
			},
			Run: func(ctx context.Context, v Views, r Row) error {
				x := r.Data.(daemon.RefinementJSON)
				return v.RollbackRound(ctx, x.SessionID, x.RoundID)
			},
		},
	}
}
