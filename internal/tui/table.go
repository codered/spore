package tui

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// table is one open resource view: its rows, the cursor, the filter, the sort
// and the detail pane. It performs no I/O; the model fetches and hands it rows.
type table struct {
	res     Resource
	session string // the session a scoped resource was opened for

	rows   []Row
	loaded bool
	err    error // the last fetch's error; the rows are then stale
	gone   bool  // the daemon has no route for this view

	cursor  string // selected row ID; survives refreshes that reorder rows
	filter  string
	sortCol int // -1: fetch order
	desc    bool

	detail *viewport.Model // non-nil while the detail pane is open
}

func newTable(r Resource, session string) *table {
	return &table{res: r, session: session, sortCol: -1}
}

// setRows installs a fetch result. An error keeps the previous rows and marks
// them stale; ErrOlderDaemon marks the whole view gone.
func (t *table) setRows(rows []Row, err error) {
	switch {
	case errors.Is(err, ErrOlderDaemon):
		t.gone, t.err = true, nil
	case err != nil:
		t.err = err
	default:
		t.rows, t.err, t.gone, t.loaded = rows, nil, false, true
		if _, ok := t.selected(); !ok {
			t.moveTo(false)
		}
	}
}

// visible is the rows after the filter and the sort.
func (t *table) visible() []Row {
	f := strings.ToLower(t.filter)
	out := make([]Row, 0, len(t.rows))
	for _, r := range t.rows {
		if f == "" || strings.Contains(strings.ToLower(strings.Join(r.Cells, " ")), f) {
			out = append(out, r)
		}
	}
	if t.sortCol >= 0 {
		col, desc := t.sortCol, t.desc
		sort.SliceStable(out, func(i, j int) bool {
			if desc {
				return lessCell(cellAt(out[j], col), cellAt(out[i], col))
			}
			return lessCell(cellAt(out[i], col), cellAt(out[j], col))
		})
	}
	return out
}

func cellAt(r Row, i int) string {
	if i < len(r.Cells) {
		return r.Cells[i]
	}
	return ""
}

// lessCell compares numerically when both cells read as numbers ("1.2k",
// "$0.12", "45%"), and as text otherwise.
func lessCell(a, b string) bool {
	x, okA := numeric(a)
	y, okB := numeric(b)
	if okA && okB {
		return x < y
	}
	return a < b
}

func numeric(s string) (float64, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s, "%"), "$"))
	mult := 1.0
	if strings.HasSuffix(s, "k") {
		s, mult = strings.TrimSuffix(s, "k"), 1000
	}
	f, err := strconv.ParseFloat(s, 64)
	return f * mult, err == nil
}

func (t *table) selected() (Row, bool) {
	for _, r := range t.visible() {
		if r.ID == t.cursor {
			return r, true
		}
	}
	return Row{}, false
}

func (t *table) move(d int) {
	v := t.visible()
	if len(v) == 0 {
		return
	}
	i := 0
	for k, r := range v {
		if r.ID == t.cursor {
			i = k
		}
	}
	t.cursor = v[max(0, min(len(v)-1, i+d))].ID
}

func (t *table) moveTo(end bool) {
	v := t.visible()
	switch {
	case len(v) == 0:
		t.cursor = ""
	case end:
		t.cursor = v[len(v)-1].ID
	default:
		t.cursor = v[0].ID
	}
}

func (t *table) setFilter(f string) {
	t.filter = f
	if _, ok := t.selected(); !ok {
		t.moveTo(false)
	}
}

// cycleSort steps through each column ascending, then descending, then back
// to fetch order.
func (t *table) cycleSort() {
	switch {
	case t.sortCol < 0:
		t.sortCol, t.desc = 0, false
	case !t.desc:
		t.desc = true
	default:
		t.sortCol, t.desc = t.sortCol+1, false
		if t.sortCol >= len(t.res.Columns()) {
			t.sortCol = -1
		}
	}
}

func (t *table) title() string {
	s := fmt.Sprintf("%s(%d)", t.res.Name(), len(t.visible()))
	if t.filter != "" {
		s += " /" + t.filter
	}
	if t.err != nil {
		s += " · stale · " + t.err.Error()
	}
	return s
}

func (t *table) openDetail(width, height int) {
	r, ok := t.selected()
	if !ok {
		return
	}
	vp := viewport.New(width, max(1, height))
	vp.SetContent(lipgloss.NewStyle().Width(max(10, width)).Render(t.res.Detail(r)))
	t.detail = &vp
}

func (t *table) render(width, height int) string {
	switch {
	case t.detail != nil:
		return t.detail.View()
	case t.gone:
		return styWarn.Render(ErrOlderDaemon.Error())
	case !t.loaded:
		return styMuted.Render("loading…")
	}
	cols := t.res.Columns()
	widths := columnWidths(cols, width)
	lines := []string{ansi.Truncate(t.headerLine(cols, widths), width, "")}
	rows := t.visible()
	if len(rows) == 0 {
		return strings.Join(append(lines, styMuted.Render("no "+t.res.Name())), "\n")
	}
	body := max(1, height-1)
	sel := 0
	for i, r := range rows {
		if r.ID == t.cursor {
			sel = i
		}
	}
	start := 0
	if sel >= body {
		start = sel - body + 1
	}
	for i := start; i < len(rows) && i < start+body; i++ {
		line := ansi.Truncate(formatRow(rows[i].Cells, cols, widths), width, "")
		if rows[i].ID == t.cursor {
			line = stySelected.Render(line + strings.Repeat(" ", max(0, width-ansi.StringWidth(line))))
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (t *table) headerLine(cols []Column, widths []int) string {
	cells := make([]string, len(cols))
	for i, c := range cols {
		title := c.Title
		if i == t.sortCol {
			if t.desc {
				title += "↓"
			} else {
				title += "↑"
			}
		}
		cells[i] = pad(title, widths[i], c.Right)
	}
	return styHeader.Render(strings.Join(cells, "  "))
}

// columnWidths gives each column its minimum and the flex column the rest.
func columnWidths(cols []Column, width int) []int {
	w := make([]int, len(cols))
	used := 2 * max(0, len(cols)-1)
	flex := -1
	for i, c := range cols {
		w[i] = c.Min
		used += c.Min
		if c.Flex {
			flex = i
		}
	}
	if flex >= 0 && used < width {
		w[flex] += width - used
	}
	return w
}

func formatRow(cells []string, cols []Column, widths []int) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = pad(clip(oneLine(cellAt(Row{Cells: cells}, i)), widths[i]), widths[i], c.Right)
	}
	return strings.Join(out, "  ")
}

// pad fills s to w display cells, on the left when right-aligned.
func pad(s string, w int, right bool) string {
	gap := strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
	if right {
		return gap + s
	}
	return s + gap
}
