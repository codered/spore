package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// fakeRes is a resource with fixed columns; its rows come from things().
type fakeRes struct{ actions []Action }

func (fakeRes) Name() string   { return "things" }
func (fakeRes) Hotkey() string { return "T" }
func (fakeRes) Scoped() bool   { return false }
func (fakeRes) Columns() []Column {
	return []Column{{Title: "NAME", Min: 6}, {Title: "SIZE", Min: 5, Right: true}, {Title: "NOTE", Min: 4, Flex: true}}
}
func (fakeRes) Fetch(context.Context, Views, string) ([]Row, error) { return nil, nil }
func (fakeRes) Detail(r Row) string                                 { return "detail of " + r.ID }
func (f fakeRes) Actions() []Action                                 { return f.actions }

// things makes one row per id; the size cell is sizes[i] when given.
func things(ids []string, sizes ...string) []Row {
	out := make([]Row, len(ids))
	for i, id := range ids {
		size := "1"
		if i < len(sizes) {
			size = sizes[i]
		}
		out[i] = Row{ID: id, Cells: []string{id, size, "note " + id}}
	}
	return out
}

func rowIDs(rs []Row) string {
	var s []string
	for _, r := range rs {
		s = append(s, r.ID)
	}
	return strings.Join(s, ",")
}

func TestTheCursorStaysOnItsRowWhenARefreshReorders(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(things([]string{"a", "b", "c"}), nil)
	tb.move(1)
	tb.setRows(things([]string{"c", "b", "a"}), nil)
	if r, ok := tb.selected(); !ok || r.ID != "b" {
		t.Fatalf("selected = %v %v, want b", r.ID, ok)
	}
}

func TestAFilterNarrowsAndMovesTheCursorOntoAMatch(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(things([]string{"alpha", "beta", "gamma"}), nil)
	tb.setFilter("MM")
	if got := rowIDs(tb.visible()); got != "gamma" {
		t.Fatalf("visible = %s, want gamma", got)
	}
	if r, _ := tb.selected(); r.ID != "gamma" {
		t.Fatalf("selected = %s, want the only match", r.ID)
	}
	tb.setFilter("")
	if got := rowIDs(tb.visible()); got != "alpha,beta,gamma" {
		t.Fatalf("cleared filter shows %s", got)
	}
}

func TestSortStepsThroughColumnsAscendingThenDescending(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(things([]string{"b", "a", "c"}, "1.2k", "800", "5"), nil)
	tb.cycleSort() // NAME ascending
	if got := rowIDs(tb.visible()); got != "a,b,c" {
		t.Fatalf("NAME asc = %s", got)
	}
	tb.cycleSort() // NAME descending
	if got := rowIDs(tb.visible()); got != "c,b,a" {
		t.Fatalf("NAME desc = %s", got)
	}
	tb.cycleSort() // SIZE ascending, numerically: 5 < 800 < 1.2k
	if got := rowIDs(tb.visible()); got != "c,a,b" {
		t.Fatalf("SIZE asc = %s, want numeric order", got)
	}
	for i := 0; i < 3; i++ { // SIZE desc, NOTE asc, NOTE desc
		tb.cycleSort()
	}
	tb.cycleSort() // back to fetch order
	if got := rowIDs(tb.visible()); got != "b,a,c" {
		t.Fatalf("after a full cycle = %s, want fetch order", got)
	}
}

func TestAFailedRefreshKeepsTheRowsAndMarksThemStale(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(things([]string{"a", "b"}), nil)
	tb.setRows(nil, errors.New("boom"))
	if got := rowIDs(tb.visible()); got != "a,b" {
		t.Fatalf("rows after a failed refresh = %s, want the old ones", got)
	}
	if !strings.Contains(tb.title(), "stale · boom") {
		t.Fatalf("title = %q, want it marked stale with the error", tb.title())
	}
	tb.setRows(things([]string{"a"}), nil)
	if strings.Contains(tb.title(), "stale") {
		t.Fatalf("title = %q after a good refresh, want the stale mark gone", tb.title())
	}
}

func TestAnOlderDaemonSaysSoInsteadOfShowingNothing(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(nil, fmt.Errorf("fetch: %w", ErrOlderDaemon))
	if out := tb.render(80, 10); !strings.Contains(out, "older than the TUI") {
		t.Fatalf("render = %q", out)
	}
}

func TestTheRenderedTableFitsItsWidthAndKeepsTheCursorVisible(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	var list []string
	for i := 0; i < 30; i++ {
		list = append(list, fmt.Sprintf("row%02d-with-a-long-name", i))
	}
	tb.setRows(things(list), nil)
	tb.moveTo(true)
	out := ansi.Strip(tb.render(40, 10))
	lines := strings.Split(out, "\n")
	if len(lines) > 10 {
		t.Fatalf("%d lines, want at most 10", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w > 40 {
			t.Errorf("line %d is %d cells wide: %q", i, w, l)
		}
	}
	if !strings.Contains(out, "row29") {
		t.Fatalf("the cursor's row is off screen:\n%s", out)
	}
}

func TestTheDetailPaneShowsTheSelectedRow(t *testing.T) {
	tb := newTable(fakeRes{}, "")
	tb.setRows(things([]string{"a", "b"}), nil)
	tb.move(1)
	tb.openDetail(40, 10)
	if out := tb.render(40, 10); !strings.Contains(out, "detail of b") {
		t.Fatalf("detail = %q", out)
	}
}
