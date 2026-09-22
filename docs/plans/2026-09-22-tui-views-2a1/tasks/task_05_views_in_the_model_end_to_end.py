"""Task 05 — open views by hotkey or :command, refresh them, act on rows; proven end to end.

Source plan: Task 5 (the model) plus Task 6 Step 1 (the TUI end-to-end test),
which moves here: this is the first task where a key press can travel
tui model -> Views -> adapter -> daemon HTTP -> store and back to the screen,
so it is the task that must prove it.
"""
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import GateFailure, gate

import plan_lib
from plan_lib import One, append_go, gotest, lines, offset, todo

KIND = "scripted"
TIER = "T2"
TOUCHES = ["internal/tui", "cmd/spore"]
CROSSES = ["tui model -> Views -> adapter -> daemon HTTP (keys to screen, and x/y cancelling a real job)"]

APP = "internal/tui/app.go"
VIEW = "internal/tui/view.go"
HEADER = "internal/tui/header.go"
APP_TEST = "internal/tui/app_test.go"
VIEW_TEST = "internal/tui/view_test.go"
E2E = "cmd/spore/tui_e2e_test.go"

# ------------------------------------------------------------------ tests --

NEW_TEST_MODEL_TICK = """
    // A real refresh tick sleeps two seconds; tests drive ticks by hand.
    m.viewTick = func(int) tea.Cmd { return nil }
"""

APP_TESTS = """
func TestAHotkeyOpensItsViewOnlyInNormal(t *testing.T) {
    fb := &fakeBackend{skills: daemon.SkillsJSON{Skills: []daemon.SkillJSON{{Name: "debug"}}}}
    m := newTestModel(t, fb, "s1")
    typeText(m, "S")
    if m.table != nil || m.input.Value() != "S" {
        t.Fatalf("S in INSERT opened a view (table=%v, input=%q)", m.table != nil, m.input.Value())
    }
    press(m, "esc", "S")
    if m.table == nil || m.table.res.Name() != "skills" {
        t.Fatal("S in NORMAL did not open skills")
    }
    if !strings.Contains(m.View(), "skills(1)") || !strings.Contains(m.View(), "debug") {
        t.Fatalf("view:\\n%s", m.View())
    }
}

func TestAltHotkeyFromInsertOpensTheView(t *testing.T) {
    m := newTestModel(t, &fakeBackend{}, "s1")
    run(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("J"), Alt: true})
    if m.table == nil || m.table.res.Name() != "jobs" {
        t.Fatal("alt+J from INSERT did not open jobs")
    }
}

func TestEscClosesTheInnermostLayerFirst(t *testing.T) {
    fb := &fakeBackend{skills: daemon.SkillsJSON{Skills: []daemon.SkillJSON{{Name: "debug"}, {Name: "deploy"}}}}
    m := newTestModel(t, fb, "s1")
    feed(m, wev("s1", daemon.WireTurnStarted))
    press(m, "esc", "S", "enter")
    if m.table.detail == nil {
        t.Fatal("enter did not open the detail pane")
    }
    press(m, "esc")
    if m.table == nil || m.table.detail != nil {
        t.Fatal("esc must close the detail pane and keep the view")
    }
    press(m, "/")
    typeText(m, "dep")
    press(m, "enter")
    if m.table.filter != "dep" {
        t.Fatalf("filter = %q", m.table.filter)
    }
    press(m, "esc")
    if m.table == nil || m.table.filter != "" {
        t.Fatal("esc must clear the filter and keep the view")
    }
    press(m, "esc")
    if m.table != nil {
        t.Fatal("esc must close the view")
    }
    if len(fb.stopped) != 0 {
        t.Fatalf("closing layers stopped the turn: %v", fb.stopped)
    }
    press(m, "esc")
    if len(fb.stopped) != 1 {
        t.Fatal("esc on the chat screen must still stop the running turn")
    }
}

func TestColonCommandsOpenViewsAndChatReturns(t *testing.T) {
    m := newTestModel(t, &fakeBackend{}, "s1")
    press(m, "esc", ":")
    typeText(m, "usage")
    press(m, "enter")
    if m.table == nil || m.table.res.Name() != "usage" {
        t.Fatal(":usage did not open the usage view")
    }
    press(m, ":")
    typeText(m, "chat")
    press(m, "enter")
    if m.table != nil {
        t.Fatal(":chat did not return to the chat screen")
    }
}

func TestAConfirmedActionRunsOnlyOnYesAndRefreshes(t *testing.T) {
    fb := &fakeBackend{jobs: []daemon.JobJSON{{ID: 7, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning", Enabled: true, NextRun: fixedNow().Add(time.Hour)}}}
    m := newTestModel(t, fb, "s1")
    press(m, "esc", "J", "x")
    if m.mode != modeConfirm || !strings.Contains(m.View(), "cancel job 7? y/n") {
        t.Fatalf("mode=%s, want the confirmation in the status bar:\\n%s", m.mode, m.View())
    }
    press(m, "n")
    if len(fb.cancelledJobs) != 0 {
        t.Fatal("n ran the action")
    }
    before := fb.fetches
    press(m, "x", "y")
    if len(fb.cancelledJobs) != 1 || fb.cancelledJobs[0] != 7 {
        t.Fatalf("cancelled = %v, want [7]", fb.cancelledJobs)
    }
    if fb.fetches <= before {
        t.Fatal("the view did not refresh after the action")
    }
}

func TestEnterOnAnAgentSelectsTheChildSession(t *testing.T) {
    fb := &fakeBackend{agents: daemon.AgentsJSON{Agents: []subagent.Status{{ID: "kid1", State: "done", Started: fixedNow()}}}}
    m := newTestModel(t, fb, "s1")
    press(m, "esc", "A", "enter")
    if m.table != nil || m.selected != "kid1" {
        t.Fatalf("table=%v selected=%q, want the chat screen on kid1", m.table != nil, m.selected)
    }
}

func TestAViewRefetchesOnItsTickAndIgnoresStaleTicks(t *testing.T) {
    fb := &fakeBackend{}
    m := newTestModel(t, fb, "s1")
    press(m, "esc", "J")
    gen, before := m.viewGen, fb.fetches
    run(m, viewTickMsg{gen: gen})
    if fb.fetches != before+1 {
        t.Fatalf("fetches = %d, want %d after a tick", fb.fetches, before+1)
    }
    press(m, "esc")
    run(m, viewTickMsg{gen: gen})
    if fb.fetches != before+1 {
        t.Fatal("a tick for a closed view fetched")
    }
}

func TestRowsForAViewThatIsNoLongerOpenAreDropped(t *testing.T) {
    m := newTestModel(t, &fakeBackend{}, "s1")
    press(m, "esc", "S")
    run(m, viewRowsMsg{name: "jobs", rows: things([]string{"x"})})
    if len(m.table.rows) != 0 {
        t.Fatal("rows for another view were installed")
    }
}

func TestSlashUsageInInsertStillPrintsText(t *testing.T) {
    m := newTestModel(t, &fakeBackend{}, "s1")
    typeText(m, "/usage")
    press(m, "enter")
    if m.table != nil || !strings.Contains(m.View(), "did usage") {
        t.Fatal("/usage typed into the input must print the report, not open the view")
    }
}
"""

VIEW_TESTS = """
func viewScene(t *testing.T) (*Model, *fakeBackend) {
    t.Helper()
    fb := &fakeBackend{
        skills: daemon.SkillsJSON{
            Skills: []daemon.SkillJSON{
                {Name: "debug", Description: "systematic debugging", BodyTokens: 1200, Loaded: true, Body: "# debug\\n\\nFind the root cause first."},
                {Name: "deploy", Description: "ship to fly.io", BodyTokens: 800},
            },
            Errors: []string{`gamma: unknown frontmatter key "bad"`},
        },
        usage: daemon.UsageJSON{
            Session: []store.UsageRow{{Model: "sonnet-5", Turns: 4, TokensIn: 1200, TokensOut: 300, TokensCacheRead: 36800, CostUSD: 0.12}},
            Days: []store.DailyUsageRow{
                {Day: "2026-09-22", UsageRow: store.UsageRow{Model: "sonnet-5", Turns: 40, TokensIn: 12000, TokensOut: 3000, TokensCacheRead: 300000, CostUSD: 1.4}},
                {Day: "2026-09-21", UsageRow: store.UsageRow{Model: "haiku-4-5", Turns: 12, TokensIn: 4000, TokensOut: 900, CostUSD: 0.03}},
            },
        },
        jobs: []daemon.JobJSON{{ID: 7, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing", Enabled: true, NextRun: fixedNow().Add(2 * time.Hour)}},
    }
    m := newTestModel(t, fb, "a1b2c3")
    run(m, tea.WindowSizeMsg{Width: 100, Height: 24})
    run(m, sessionsMsg{list: []daemon.SessionJSON{{ID: "a1b2c3", Title: "fix flaky test", Workspace: "/work/spore", Source: "chat"}}})
    return m, fb
}

func TestGoldenViews(t *testing.T) {
    m, _ := viewScene(t)
    press(m, "esc", "S")
    golden(t, "view-skills-100", m.View())
    press(m, "enter")
    golden(t, "view-skills-detail-100", m.View())

    m, _ = viewScene(t)
    press(m, "esc", "U")
    golden(t, "view-usage-100", m.View())

    m, _ = viewScene(t)
    press(m, "esc", "J", "x")
    golden(t, "view-jobs-confirm-100", m.View())

    m, fb := viewScene(t)
    press(m, "esc", "S")
    fb.viewErr = errors.New("connection refused")
    press(m, "ctrl+r")
    golden(t, "view-stale-100", m.View())
}
"""

E2E_TESTS = """
// press sends one key the way the driver sends runes.
func (d *driver) press(k string) {
    switch k {
    case "esc":
        d.key(tea.KeyEsc)
    case "enter":
        d.key(tea.KeyEnter)
    default:
        d.apply(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
    }
}

func TestTheTUIOpensViewsAgainstARealDaemonAndCancelsAJob(t *testing.T) {
    ws := t.TempDir()
    c := e2eDaemon(t, ws, provider.ScriptTurn{Text: "hi there", Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}})
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    sid, err := c.createSession(ctx, "chat", ws)
    if err != nil {
        t.Fatal(err)
    }
    var job struct {
        ID int64 `json:"id"`
    }
    if err := c.do(ctx, "POST", "/api/jobs", map[string]string{"spec": "0 9 * * *", "prompt": "morning briefing"}, &job); err != nil {
        t.Fatal(err)
    }

    be := tuiBackend{c: c}
    d := &driver{t: t, m: tui.New(ctx, be, sid, tui.Options{}), msgs: make(chan tea.Msg, 256)}
    d.apply(tea.WindowSizeMsg{Width: 120, Height: 40})
    go tui.Pump(ctx, be, func(msg tea.Msg) { d.msgs <- msg })
    d.apply(<-d.msgs) // connected

    d.typeLine("hello")
    d.until("hi there")

    d.press("esc")
    d.press("U")
    d.until("usage(")
    d.until("this session") // only the usage view renders this; the header already shows the model

    d.press("esc")
    d.until("─ chat")

    d.press("J")
    d.until("morning briefing")
    d.press("x")
    d.until("y/n")
    d.press("y")
    d.until("disabled")

    var jobs []struct {
        ID      int64 `json:"id"`
        Enabled bool  `json:"enabled"`
    }
    if err := c.do(ctx, "GET", "/api/jobs", nil, &jobs); err != nil {
        t.Fatal(err)
    }
    if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Enabled {
        t.Fatalf("jobs = %+v, want the job disabled", jobs)
    }
}
"""

# ------------------------------------------------------------ production --

MSG_TYPES = """
    viewRowsMsg struct {
        name, session string
        rows          []Row
        err           error
    }
    viewTickMsg   struct{ gen int }
    actionDoneMsg struct{ err error }
"""

REFRESH_CONST = """
    // refreshEvery is how often an open view refetches.
    refreshEvery = 2 * time.Second
"""

MODEL_FIELDS = """
    // views serves the resource views; nil when the backend cannot.
    views Views
    // table is the open view; nil means the chat screen.
    table *table
    // viewGen increments whenever a view opens or closes, so a tick from an
    // earlier view is ignored.
    viewGen  int
    viewErr  string
    viewTick func(gen int) tea.Cmd
"""

NEW_VIEWS = """
    if v, ok := be.(Views); ok {
        m.views = v
    }
    m.viewTick = func(gen int) tea.Cmd {
        return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return viewTickMsg{gen: gen} })
    }
"""

UPDATE_CASES = """
    case viewRowsMsg:
        if m.table != nil && m.table.res.Name() == msg.name && m.table.session == msg.session {
            m.table.setRows(msg.rows, msg.err)
        }
        return nil

    case viewTickMsg:
        if m.table == nil || msg.gen != m.viewGen {
            return nil
        }
        return tea.Batch(m.fetchView(), m.viewTick(msg.gen))

    case actionDoneMsg:
        m.viewErr = ""
        if msg.err != nil {
            m.viewErr = msg.err.Error()
        }
        return m.fetchView()
"""

VIEW_METHODS = """
// openView replaces the body with a resource's table. A scoped view is for
// the selected session.
func (m *Model) openView(r Resource) tea.Cmd {
    if m.views == nil {
        m.errorf("views need the daemon")
        return nil
    }
    m.mode = modeNormal
    m.input.Blur()
    m.table = newTable(r, m.selected)
    m.viewErr = ""
    m.viewGen++
    return tea.Batch(m.fetchView(), m.viewTick(m.viewGen))
}

func (m *Model) closeView() {
    m.table = nil
    m.viewErr = ""
    m.viewGen++
}

func (m *Model) fetchView() tea.Cmd {
    t := m.table
    if t == nil || m.views == nil {
        return nil
    }
    res, session, v, ctx := t.res, t.session, m.views, m.ctx
    return func() tea.Msg {
        rows, err := res.Fetch(ctx, v, session)
        return viewRowsMsg{name: res.Name(), session: session, rows: rows, err: err}
    }
}

// keyView handles a key while a view is open, innermost layer first: the
// detail pane, then the view itself.
func (m *Model) keyView(k tea.KeyMsg) tea.Cmd {
    t := m.table
    s := k.String()
    if t.detail != nil {
        switch s {
        case "esc", "q", "enter", "d":
            t.detail = nil
            return nil
        }
        var cmd tea.Cmd
        *t.detail, cmd = t.detail.Update(k)
        return cmd
    }
    switch s {
    case "esc":
        if t.filter != "" {
            t.setFilter("")
            return nil
        }
        m.closeView()
    case "j", "down":
        t.move(1)
    case "k", "up":
        t.move(-1)
    case "g":
        t.moveTo(false)
    case "G":
        t.moveTo(true)
    case "ctrl+d":
        t.move(m.bodyHeight() / 2)
    case "ctrl+u":
        t.move(-m.bodyHeight() / 2)
    case "/":
        m.mode = modeFilter
        m.line.SetValue(t.filter)
        m.line.CursorEnd()
        return m.line.Focus()
    case "s":
        t.cycleSort()
    case "enter", "d":
        r, ok := t.selected()
        if !ok {
            return nil
        }
        if o, isOpener := t.res.(opener); isOpener && s == "enter" {
            if id := o.Open(r); id != "" {
                m.closeView()
                return m.selectSession(id)
            }
        }
        t.openDetail(m.width, m.bodyHeight())
    case "ctrl+r":
        return m.fetchView()
    case ":":
        m.mode = modeCommand
        m.line.SetValue("")
        return m.line.Focus()
    case "?":
        m.help = true
    case "q":
        return tea.Quit
    default:
        if r, ok := resourceByHotkey(s); ok {
            return m.openView(r)
        }
        return m.runAction(s)
    }
    return nil
}

// runAction runs the selected row's action for key, asking first when the
// action has a confirmation.
func (m *Model) runAction(key string) tea.Cmd {
    r, ok := m.table.selected()
    if !ok {
        return nil
    }
    for _, a := range m.table.res.Actions() {
        if a.Key != key || (a.Applies != nil && !a.Applies(r)) {
            continue
        }
        v, ctx := m.views, m.ctx
        run := func() tea.Msg { return actionDoneMsg{err: a.Run(ctx, v, r)} }
        if a.Confirm == nil {
            return run
        }
        m.confirm = &confirmState{prompt: a.Confirm(r) + " y/n", yes: run}
        m.mode = modeConfirm
        return nil
    }
    return nil
}
"""

HANDLE_KEY_TAIL = """
    if k.Alt && k.Type == tea.KeyRunes && (m.mode == modeInsert || m.mode == modeNormal) {
        m.mode = modeNormal
        m.input.Blur()
        plain := tea.KeyMsg{Type: tea.KeyRunes, Runes: k.Runes}
        if m.table != nil {
            return m.keyView(plain)
        }
        return m.keyNormal(plain)
    }
    switch m.mode {
    case modeInsert:
        return m.keyInsert(k)
    case modeCommand:
        return m.keyCommand(k)
    case modeFilter:
        return m.keyFilter(k)
    case modeConfirm:
        return m.keyConfirm(k)
    }
    if m.table != nil {
        return m.keyView(k)
    }
    return m.keyNormal(k)
"""

KEY_NORMAL_DEFAULT = """
    default:
        if r, ok := resourceByHotkey(k.String()); ok {
            return m.openView(r)
        }
"""

KEY_FILTER = """
func (m *Model) keyFilter(k tea.KeyMsg) tea.Cmd {
    set := func(v string) {
        if m.table != nil {
            m.table.setFilter(v)
        } else {
            m.filter = v
        }
    }
    switch k.String() {
    case "esc":
        set("")
        m.mode = modeNormal
        m.line.Blur()
        return nil
    case "enter":
        m.mode = modeNormal
        m.line.Blur()
        return nil
    }
    var cmd tea.Cmd
    m.line, cmd = m.line.Update(k)
    set(m.line.Value())
    return cmd
}
"""

COMMAND = """
// command runs a `:` command. A view's name opens that view.
func (m *Model) command(line string) tea.Cmd {
    fields := strings.Fields(line)
    if len(fields) == 0 {
        return nil
    }
    name, args := fields[0], fields[1:]
    switch name {
    case "q", "quit":
        return tea.Quit
    case "new":
        ws := ""
        if len(args) > 0 {
            ws = args[0]
        }
        return m.newSession(ws)
    case "sessions":
        m.showAll = len(args) > 0 && args[0] == "all"
        return nil
    case "chat":
        m.closeView()
        return nil
    case "clear", "compact", "context":
        return m.slash(name)
    }
    if r, ok := resourceByName(name); ok {
        return m.openView(r)
    }
    m.errorf("unknown command: %s", name)
    return nil
}

// slashLine runs a /command typed into the input. /usage, /skills and
// /agents keep printing the text report they print on every other surface.
func (m *Model) slashLine(line string) tea.Cmd {
    fields := strings.Fields(line)
    if len(fields) == 0 {
        return nil
    }
    switch fields[0] {
    case "clear", "compact", "context", "usage", "skills", "agents":
        return m.slash(fields[0])
    }
    return m.command(line)
}
"""

MAIN_VIEW_TABLE = """
    if m.table != nil && !m.help {
        h := m.bodyHeight()
        return lipgloss.NewStyle().Width(m.width).Height(h).MaxHeight(h).Render(m.table.render(m.width, h))
    }
"""

STATUS_CONFIRM = """
    if m.mode == modeConfirm && m.table != nil && m.confirm != nil {
        return fitRow(styMode.Render(" "+m.mode.String()+" ")+" "+styWarn.Render(m.confirm.prompt), "", m.width)
    }
"""

STATUS_VIEW_ERR = """
    if m.viewErr != "" {
        right = append(right, styDanger.Render(m.viewErr))
    }
"""

RULE_VIEW = """
func (m *Model) ruleView() string {
    title := "chat"
    if m.table != nil {
        title = m.table.title()
    }
    head := "─ " + title + " "
    return styMuted.Render(ansi.Truncate(head+strings.Repeat("─", max(0, m.width-lipgloss.Width(head))), m.width, ""))
}
"""

KEY_HINTS_VIEW = """
    if m.table != nil {
        esc := "back"
        switch {
        case m.table.detail != nil:
            esc = "close"
        case m.table.filter != "":
            esc = "clear"
        }
        parts := []string{hint("enter", "open"), hint("/", "filter"), hint("s", "sort")}
        for _, a := range m.table.res.Actions() {
            parts = append(parts, hint(a.Key, a.Label))
        }
        return strings.Join(append(parts, hint("esc", esc)), "  ")
    }
"""


def ins_after(path, expected, new_lines, marker):
    if todo(path, marker):
        with One(path) as ed:
            ed.insert_after(ed.locate(expected), new_lines)


def ins_before(path, expected, new_lines, marker):
    if todo(path, marker):
        with One(path) as ed:
            ed.insert_before(ed.locate(expected), new_lines)


def swap_line(path, expected, new_lines, marker):
    if todo(path, marker):
        with One(path) as ed:
            ed.swap(ed.locate(expected), new_lines)


def apply_tests():
    # internal/tui/app_test.go
    ins_after(APP_TEST, "\tm.line.Cursor.SetMode(cursor.CursorStatic)", lines(NEW_TEST_MODEL_TICK),
              "m.viewTick = func(int) tea.Cmd { return nil }")
    ins_after(APP_TEST, '\t"testing"', ['\t"time"'], '\t"time"\n')
    ins_after(APP_TEST, '\t"github.com/codered/spore/internal/provider"', ['\t"github.com/codered/spore/internal/subagent"'],
              '"github.com/codered/spore/internal/subagent"')
    append_go(APP_TEST, APP_TESTS, "func TestAHotkeyOpensItsViewOnlyInNormal")
    # internal/tui/view_test.go
    ins_after(VIEW_TEST, '\t"encoding/json"', ['\t"errors"'], '\t"errors"\n')
    ins_after(VIEW_TEST, '\t"github.com/codered/spore/internal/provider"', ['\t"github.com/codered/spore/internal/store"'],
              '"github.com/codered/spore/internal/store"')
    append_go(VIEW_TEST, VIEW_TESTS, "func TestGoldenViews")
    # cmd/spore/tui_e2e_test.go
    append_go(E2E, E2E_TESTS, "func TestTheTUIOpensViewsAgainstARealDaemonAndCancelsAJob")


def apply_model():
    # internal/tui/app.go, one edit per session.
    if todo(APP, "\tviewTickMsg   struct{ gen int }"):
        with One(APP) as ed:
            head = ed.locate("\tnewSessionMsg struct {")
            ed.insert_after(offset(ed, head, 3, "\t}"), lines(MSG_TYPES))
    ins_after(APP, "\tinputMaxHeight = 8", lines(REFRESH_CONST), "refreshEvery = 2 * time.Second")
    swap_line(APP, 'var commandNames = []string{"agents", "clear", "compact", "context", "new", "q", "quit", "sessions", "skills", "usage"}',
              ['var commandNames = []string{"agents", "chat", "clear", "compact", "context", "jobs", "new", "q", "quit", "sessions", "skills", "usage"}'],
              '"agents", "chat", "clear"')
    ins_after(APP, "\treconnecting bool", lines(MODEL_FIELDS), "\tviewTick func(gen int) tea.Cmd")
    ins_before(APP, "\tm.mode = modeInsert", lines(NEW_VIEWS), "if v, ok := be.(Views); ok {")
    ins_before(APP, "\tcase tea.KeyMsg:", lines(UPDATE_CASES) + [""], "\tcase viewRowsMsg:")
    if todo(APP, "func (m *Model) openView(r Resource) tea.Cmd {"):
        with One(APP) as ed:
            ret = ed.locate("\t\treturn newSessionMsg{id: id, err: err}")
            ed.insert_after(offset(ed, ret, 2, "}"), [""] + lines(VIEW_METHODS))
    if todo(APP, "\t\tif m.table != nil {\n\t\t\treturn m.keyView(plain)"):
        with One(APP) as ed:
            first = ed.locate("\tif k.Alt && k.Type == tea.KeyRunes && (m.mode == modeInsert || m.mode == modeNormal) {")
            ed.swap_range(first, ed.locate("\treturn m.keyNormal(k)"), lines(HANDLE_KEY_TAIL))
    ins_after(APP, "\t\tm.sidebarPref = &on", lines(KEY_NORMAL_DEFAULT),
              "\tdefault:\n\t\tif r, ok := resourceByHotkey(k.String()); ok {")
    if todo(APP, "\tset := func(v string) {"):
        with One(APP) as ed:
            head = ed.locate("func (m *Model) keyFilter(k tea.KeyMsg) tea.Cmd {")
            tail = ed.locate("\tm.filter = m.line.Value()")
            offset(ed, tail, 1, "\treturn cmd")
            ed.swap_range(head, offset(ed, tail, 2, "}"), lines(KEY_FILTER))
    swap_line(APP, '\t\t\treturn m.command(strings.TrimPrefix(text, "/"))',
              ['\t\t\treturn m.slashLine(strings.TrimPrefix(text, "/"))'], "return m.slashLine(")
    if todo(APP, "func (m *Model) slashLine(line string) tea.Cmd {"):
        with One(APP) as ed:
            head = ed.locate("// command runs a `:` command, or a slash command typed into the input.")
            tail = ed.locate('\tm.errorf("unknown command: %s", name)')
            offset(ed, tail, 1, "\treturn nil")
            ed.swap_range(head, offset(ed, tail, 2, "}"), lines(COMMAND))

    # internal/tui/view.go
    if todo(VIEW, "\tif m.sidebarOn() && m.table == nil {"):
        with One(VIEW) as ed:
            body = ed.locate("\tbody := m.mainView()")
            ed.swap(offset(ed, body, 1, "\tif m.sidebarOn() {"), ["\tif m.sidebarOn() && m.table == nil {"])
    ins_before(VIEW, "\tw, h := m.mainWidth(), m.bodyHeight()", lines(MAIN_VIEW_TABLE), "\tif m.table != nil && !m.help {")
    ins_after(VIEW, "func (m *Model) statusView() string {", lines(STATUS_CONFIRM),
              "if m.mode == modeConfirm && m.table != nil && m.confirm != nil {")
    swap_line(VIEW, "\tif m.mode == modeNormal && m.cache.State(m.selected) != daemon.SessionIdle {",
              ["\tif m.mode == modeNormal && m.table == nil && m.cache.State(m.selected) != daemon.SessionIdle {"],
              "m.mode == modeNormal && m.table == nil")
    ins_before(VIEW, '\treturn fitRow(left, strings.Join(right, styMuted.Render(" · ")), m.width)', lines(STATUS_VIEW_ERR),
               'if m.viewErr != "" {')

    # internal/tui/header.go
    if todo(HEADER, "\t\ttitle = m.table.title()"):
        with One(HEADER) as ed:
            head = ed.locate("func (m *Model) ruleView() string {")
            offset(ed, head, 1, '\thead := "─ chat "')
            ed.swap_range(head, offset(ed, head, 3, "}"), lines(RULE_VIEW))
    ins_before(HEADER, '\treturn hint("i", "type") + "  " + hint("j/k", "session") + "  " + hint("n", "new") + "  " + hint("?", "help")',
               lines(KEY_HINTS_VIEW), 'esc := "back"')


VIEW_GOLDENS = ["view-skills-100", "view-skills-detail-100", "view-usage-100", "view-jobs-confirm-100", "view-stale-100"]


def apply():
    apply_tests()
    apply_model()
    if os.path.exists("internal/tui/testdata/view-skills-100.golden"):
        return
    upd = subprocess.run(gotest("./internal/tui/", run="TestGoldenViews", race=False, count=1) + ["-update"],
                         capture_output=True, text=True)
    if upd.returncode != 0:
        raise GateFailure("golden -update failed:\n" + (upd.stdout + upd.stderr)[-3000:])


RED = [
    ("the model tests need the view state",
     gotest("./internal/tui/", run="Hotkey|AltHotkey|EscCloses|ColonCommands|ConfirmedAction|EnterOnAnAgent|Refetches|NoLongerOpen|SlashUsage", race=False),
     ["m.table undefined", "m.viewTick undefined", "undefined: viewTickMsg"]),
    ("the TUI never shows a view against a real daemon",
     gotest("./cmd/spore/", run="OpensViews", race=False, count=1),
     ['the screen never showed "usage("']),
]


def _golden_has(name, *needles):
    text = "\n".join(r.rstrip() for r in plan_lib.golden(name))
    return all(n in text for n in needles)


def _status(name):
    return plan_lib.golden(name)[-1]


def _rule(name):
    return plan_lib.golden(name)[3]


def verify():
    gate.structural("New picks Views up from the backend", lambda: taskkit.file_contains(APP, "be.(Views)"))
    gate.structural("keys reach resourceByHotkey", lambda: taskkit.file_contains(APP, "resourceByHotkey(k.String())"))
    gate.structural("/commands go through slashLine", lambda: taskkit.file_contains(APP, 'return m.slashLine(strings.TrimPrefix(text, "/"))'))
    gate.structural("gofmt clean", lambda: plan_lib.gofmt_clean(APP, VIEW, HEADER, APP_TEST, VIEW_TEST, E2E))
    for name in VIEW_GOLDENS:
        gate.structural(name + " fits 100x24", lambda n=name: plan_lib.golden_fits(n, 100, 24))
    # Source plan, Task 5 Step 6: what each view golden must show.
    gate.structural("view-skills-100: rule, columns, the selected row, the error row, no sidebar, view keys",
                    lambda: _rule("view-skills-100").startswith("─ skills(3) ──")
                    and _golden_has("view-skills-100", "NAME", "LOADED", "TOKENS", "DESCRIPTION", "systematic debugging", "gamma: unknown frontmatter key")
                    and not _golden_has("view-skills-100", "│")
                    and "<enter> open  </> filter  <s> sort  <esc> back" in _status("view-skills-100"))
    gate.structural("view-skills-detail-100: the body, and esc closes",
                    lambda: _golden_has("view-skills-detail-100", "Find the root cause first.")
                    and "<esc> close" in _status("view-skills-detail-100"))
    gate.structural("view-usage-100: this session first at 96%, then two days, haiku with no cache",
                    lambda: _usage_rows())
    gate.structural("view-jobs-confirm-100: the confirmation in the status bar",
                    lambda: _status("view-jobs-confirm-100").startswith(" CONFIRM  cancel job 7? y/n"))
    gate.structural("view-stale-100: marked stale, rows kept",
                    lambda: "skills(3) · stale · connection refused" in _rule("view-stale-100")
                    and _golden_has("view-stale-100", "systematic debugging"))
    gate.component("tui tests, race", gotest("./internal/tui/"))
    gate.crossing("keys open views, and x/y cancels a real job, through the adapter and a real daemon",
                  cmd=gotest("./cmd/spore/", run="OpensViews|TheAdapterReads|WithoutTheRoute|TUIDrives", count=3))
    gate.component("cmd/spore tests, race", gotest("./cmd/spore/"))


def _usage_rows():
    body = [r for r in plan_lib.golden("view-usage-100")[5:-1] if r.strip()]
    return (len(body) >= 3 and body[0].startswith("this session") and "sonnet-5" in body[0]
            and "96%" in body[0] and "$0.1200" in body[0]
            and body[1].startswith("2026-09-22") and body[2].startswith("2026-09-21")
            and "haiku-4-5" in body[2] and " - " in body[2])


if __name__ == "__main__":
    if "--red" in sys.argv:
        apply_tests()
        raise SystemExit(plan_lib.red(RED))
    raise SystemExit(gate.run(apply, verify))
