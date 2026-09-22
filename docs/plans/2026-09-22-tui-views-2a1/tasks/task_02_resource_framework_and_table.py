"""Task 02 — the resource framework and the table every view runs in.

Source plan: Task 2 Steps 2-5 (Step 1, views.go, moved to Task 01).
One component (internal/tui). It changes no seam: it consumes the Views
interface Task 01 already proved against a real daemon.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import gate

import plan_lib
from plan_lib import go, gotest

KIND = "scripted"
TIER = "T1"
TOUCHES = ["internal/tui"]
CROSSES = []

RESOURCE = """
package tui

import (
    "context"
    "time"
)

// Resource is one k9s-style view: what its table shows and what can be done
// to a row. The table component does the rest.
type Resource interface {
    Name() string   // "skills": the :command and the table title
    Hotkey() string // "S"
    Scoped() bool   // true: rows belong to the selected session
    Columns() []Column
    Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error)
    Detail(r Row) string // shown by enter / d
    Actions() []Action
}

// Column is one table column.
type Column struct {
    Title string
    Min   int  // minimum width in cells
    Flex  bool // takes the remaining width; at most one per resource
    Right bool // right-aligned (numbers)
}

// Row is one table row. ID is stable across refreshes, so the cursor can
// follow a row the refresh moved.
type Row struct {
    ID    string
    Cells []string
    Data  any // the typed value, for Detail and Actions
}

// Action is something a key does to the selected row.
type Action struct {
    Key     string
    Label   string
    Applies func(Row) bool   // nil: every row
    Confirm func(Row) string // nil: runs without asking
    Run     func(ctx context.Context, v Views, r Row) error
}

// opener is a Resource whose rows lead to a session: enter selects that
// session and returns to the chat screen instead of opening the detail pane.
type opener interface {
    Open(r Row) (sessionID string)
}

// clock is the resources' notion of now, for ages and relative times. Tests
// pin it.
var clock = time.Now
"""

TABLE_TEST = """
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
    lines := strings.Split(out, "\\n")
    if len(lines) > 10 {
        t.Fatalf("%d lines, want at most 10", len(lines))
    }
    for i, l := range lines {
        if w := ansi.StringWidth(l); w > 40 {
            t.Errorf("line %d is %d cells wide: %q", i, w, l)
        }
    }
    if !strings.Contains(out, "row29") {
        t.Fatalf("the cursor's row is off screen:\\n%s", out)
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
"""

TABLE = """
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
        return strings.Join(append(lines, styMuted.Render("no "+t.res.Name())), "\\n")
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
    return strings.Join(lines, "\\n")
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
"""

NEW_FILES = ["internal/tui/resource.go", "internal/tui/table.go", "internal/tui/table_test.go"]


def apply_tests():
    taskkit.create_file("internal/tui/resource.go", go(RESOURCE))
    taskkit.create_file("internal/tui/table_test.go", go(TABLE_TEST))


def apply_impl():
    taskkit.create_file("internal/tui/table.go", go(TABLE))


def apply():
    apply_tests()
    apply_impl()


RED = [
    ("the table tests need newTable",
     gotest("./internal/tui/", run="Cursor|Filter|Sort|FailedRefresh|OlderDaemon|RenderedTable|DetailPane", race=False),
     ["undefined: newTable"]),
]


def verify():
    for path in NEW_FILES:
        gate.structural(path + " exists", lambda p=path: os.path.exists(p))
    gate.structural("gofmt clean", lambda: plan_lib.gofmt_clean(*NEW_FILES))
    gate.component("the seven table tests pass",
                   gotest("./internal/tui/", run="Cursor|Filter|Sort|FailedRefresh|OlderDaemon|RenderedTable|DetailPane", count=1))
    gate.component("tui tests, race", gotest("./internal/tui/"))


if __name__ == "__main__":
    if "--red" in sys.argv:
        apply_tests()
        raise SystemExit(plan_lib.red(RED))
    raise SystemExit(gate.run(apply, verify))
