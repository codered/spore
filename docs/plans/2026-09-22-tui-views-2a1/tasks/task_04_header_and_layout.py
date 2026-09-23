"""Task 04 — a k9s header with key hints, a title rule, and a status bar of keys.

Source plan: Task 4. One component (internal/tui). The red phase is the
golden diff: after the layout change and before -update, every golden screen
test must fail and every other test must pass.
"""
import os
import subprocess
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import GateFailure, gate

import plan_lib
from plan_lib import One, go, gotest, lines, offset, todo

KIND = "scripted"
TIER = "T1"
TOUCHES = ["internal/tui"]
CROSSES = []

HEADER = """
package tui

import (
    "fmt"
    "strings"

    "github.com/charmbracelet/lipgloss"
    "github.com/charmbracelet/x/ansi"
)

// headerRows is the header's height: three rows at 100 columns and wider,
// one row below.
func (m *Model) headerRows() int {
    if m.width >= 100 {
        return 3
    }
    return 1
}

// bodyHeight is the rows between the title rule and the status bar.
func (m *Model) bodyHeight() int { return max(1, m.height-m.headerRows()-2) }

// headerView is the k9s band: where you are on the left, the keys that work
// on the right.
func (m *Model) headerView() string {
    sv := m.cache.get(m.selected)
    title := sv.info.Title
    if title == "" {
        title = "untitled"
    }
    src := sv.info.Source
    if src == "" {
        src = "unknown"
    }
    who := styKey.Render("spore") + styMuted.Render(" · ") + short(m.selected) + " " + oneLine(title) +
        styMuted.Render(" · "+src+" · "+tildePath(sv.info.Workspace))
    facts := m.facts()
    if m.headerRows() == 1 {
        left := who
        if facts != "" {
            left += styMuted.Render(" · ") + facts
        }
        return fitRow(left, hint("?", "help")+"  "+hint(":", "cmd"), m.width)
    }
    grid := m.hintGrid()
    lefts := []string{who, facts, ""}
    rows := make([]string, 3)
    for i := range rows {
        rows[i] = fitRow(lefts[i], grid[i], m.width)
    }
    return strings.Join(rows, "\\n")
}

// facts is the selected session's numbers.
func (m *Model) facts() string {
    sv := m.cache.get(m.selected)
    var out []string
    if sv.model != "" {
        out = append(out, sv.model)
    }
    if sv.ctxTokens > 0 {
        out = append(out, "ctx "+humanTokens(sv.ctxTokens))
    }
    if m.opts.ShowCost && sv.cost > 0 {
        out = append(out, fmt.Sprintf("$%.2f", sv.cost))
    }
    if n := m.cache.BlockedCount(); n > 0 {
        out = append(out, styWarn.Render(fmt.Sprintf("%d blocked", n)))
    }
    if m.reconnecting {
        out = append(out, styDanger.Render("reconnecting…"))
    }
    return strings.Join(out, styMuted.Render(" · "))
}

func hint(key, label string) string {
    return styKey.Render("<"+key+">") + " " + styMuted.Render(label)
}

// hintGrid is the header's right block: the global keys, then every view's
// hotkey, spread over three rows.
func (m *Model) hintGrid() []string {
    cells := []string{hint("n", "new"), hint(":", "cmd"), hint("?", "help")}
    for _, r := range resources() {
        cells = append(cells, hint(r.Hotkey(), r.Name()))
    }
    cells = append(cells, hint("q", "quit"))
    rows := []string{"", "", ""}
    per := (len(cells) + 2) / 3
    for i, c := range cells {
        r := min(2, i/per)
        if rows[r] != "" {
            rows[r] += "  "
        }
        rows[r] += c
    }
    return rows
}

// fitRow puts left and right on one line of width cells, cutting left first.
func fitRow(left, right string, width int) string {
    rw := lipgloss.Width(right)
    room := width - rw - 1
    if room < 1 {
        return ansi.Truncate(right, width, "")
    }
    if lipgloss.Width(left) > room {
        left = ansi.Truncate(left, room, "…")
    }
    return left + strings.Repeat(" ", width-lipgloss.Width(left)-rw) + right
}

// ruleView is the line between header and body, naming what the body shows.
func (m *Model) ruleView() string {
    head := "─ chat "
    return styMuted.Render(ansi.Truncate(head+strings.Repeat("─", max(0, m.width-lipgloss.Width(head))), m.width, ""))
}

// keyHints is the status bar's left half: the keys that work in this mode.
func (m *Model) keyHints() string {
    switch m.mode {
    case modeInsert:
        return hint("enter", "send") + "  " + hint("ctrl+j", "newline") + "  " + hint("esc", "normal")
    case modeCommand:
        return hint("enter", "run") + "  " + hint("tab", "complete") + "  " + hint("esc", "cancel")
    case modeFilter:
        return hint("enter", "keep") + "  " + hint("esc", "clear")
    case modeConfirm:
        return ""
    }
    return hint("i", "type") + "  " + hint("j/k", "session") + "  " + hint("n", "new") + "  " + hint("?", "help")
}
"""

STATUS_VIEW = """
// statusView is the mode badge and the keys that work here, with the few
// signals that need the user now on the right.
func (m *Model) statusView() string {
    left := styMode.Render(" "+m.mode.String()+" ") + " " + m.keyHints()
    var right []string
    if m.unseen {
        right = append(right, styAccent.Render("↓ new"))
    }
    if m.mode == modeNormal && m.cache.State(m.selected) != daemon.SessionIdle {
        right = append(right, hint("esc", "stop"))
    }
    return fitRow(left, strings.Join(right, styMuted.Render(" · ")), m.width)
}
"""

HELP_VIEWS = """
        styKey.Render("VIEWS") + "  (normal mode)",
        "  S skills · A agents · U usage · J jobs · or :skills :agents :usage :jobs",
        "  in a view: j/k move · / filter · s sort · enter open · x act on the row · ctrl+r refresh · esc back",
        "",
"""

VIEW = "internal/tui/view.go"


def swap_line(path, old, new_lines, marker):
    if todo(path, marker):
        with One(path) as ed:
            ed.swap(ed.locate(old), new_lines)


def apply_layout():
    taskkit.create_file("internal/tui/header.go", go(HEADER))
    swap_line(VIEW, "\tm.vp.Height = max(1, m.height-1-lipgloss.Height(m.inputView())-m.overlayHeight())",
              ["\tm.vp.Height = max(1, m.bodyHeight()-lipgloss.Height(m.inputView())-m.overlayHeight())"],
              "m.vp.Height = max(1, m.bodyHeight()")
    swap_line(VIEW, "\treturn lipgloss.JoinVertical(lipgloss.Left, body, m.statusView())",
              ["\treturn lipgloss.JoinVertical(lipgloss.Left, m.headerView(), m.ruleView(), body, m.statusView())"],
              "m.headerView(), m.ruleView(), body")
    swap_line(VIEW, "\tw, h := m.mainWidth(), m.height-1", ["\tw, h := m.mainWidth(), m.bodyHeight()"],
              "\tw, h := m.mainWidth(), m.bodyHeight()")
    swap_line(VIEW, "\th := m.height - 1", ["\th := m.bodyHeight()"], "\th := m.bodyHeight()")
    if todo(VIEW, "// statusView is the mode badge"):
        with One(VIEW) as ed:
            head = ed.locate("func (m *Model) statusView() string {")
            ret = ed.locate('\treturn left + strings.Repeat(" ", gap) + r')
            ed.swap_range(head, offset(ed, ret, 1, "}"), lines(STATUS_VIEW))
    # fmt and ansi were used only by the old statusView.
    if taskkit.file_contains(VIEW, '\t"fmt"\n'):
        with One(VIEW) as ed:
            ed.delete(ed.locate('\t"fmt"'))
    if taskkit.file_contains(VIEW, '\t"github.com/charmbracelet/x/ansi"\n'):
        with One(VIEW) as ed:
            ed.delete(ed.locate('\t"github.com/charmbracelet/x/ansi"'))
    if todo(VIEW, 'styKey.Render("VIEWS")'):
        with One(VIEW) as ed:
            insert = ed.locate('\t\t"  enter send · ctrl+j newline · ↑↓ history · esc normal mode",')
            ed.insert_after(offset(ed, insert, 1, '\t\t"",'), lines(HELP_VIEWS))


GOLDENS = ["screen-60", "screen-100", "screen-160", "sessions-all-100", "tool-expanded-100",
           "approval-insert-100", "approval-normal-100", "too-small"]


def goldens_regenerated():
    return "─ chat" in "\n".join(plan_lib.golden("screen-100"))


def apply():
    apply_layout()
    if goldens_regenerated():
        return
    # Red: every golden screen test fails on the new header; nothing else may.
    proc = subprocess.run(gotest("./internal/tui/", race=False, count=1), capture_output=True, text=True)
    out = proc.stdout + proc.stderr
    failed = sorted({l.split()[2] for l in out.splitlines() if l.startswith("--- FAIL: ")})
    want = ["TestGoldenAllSessions", "TestGoldenApprovalOverlay", "TestGoldenExpandedTool", "TestGoldenScreens"]
    if failed != want:
        print(out[-4000:], file=sys.stderr)
        raise GateFailure("before -update the failing tests must be exactly %s, got %s" % (want, failed))
    print("RED as expected: " + ", ".join(failed), file=sys.stderr)
    upd = subprocess.run(gotest("./internal/tui/", run="Golden", race=False, count=1) + ["-update"],
                         capture_output=True, text=True)
    if upd.returncode != 0:
        raise GateFailure("golden -update failed:\n" + (upd.stdout + upd.stderr)[-3000:])


def _wide_header(name):
    g = [r.rstrip() for r in plan_lib.golden(name)]
    return (g[0].startswith("spore · a1b2 fix flaky test · chat · /work/spore")
            and g[0].endswith("<n> new  <:> cmd  <?> help")
            and g[1].startswith("sonnet-5 · ctx 38.0k · 1 blocked")
            and g[1].endswith("<S> skills  <A> agents  <U> usage")
            and g[2].endswith("<J> jobs  <q> quit")
            and g[3].startswith("─ chat ───")
            and g[-1].startswith(" INSERT  <enter> send")
            and "sonnet-5" not in g[-1] and "ctx" not in g[-1])


def _narrow_header(name="screen-60"):
    g = [r.rstrip() for r in plan_lib.golden(name)]
    return (g[0].startswith("spore · a1b2") and g[0].endswith("<?> help  <:> cmd")
            and g[1].startswith("─ chat ───") and g[-1].startswith(" INSERT "))


def verify():
    gate.structural("View joins header, rule, body and status",
                    lambda: taskkit.file_contains(VIEW, "m.headerView(), m.ruleView(), body, m.statusView())"))
    gate.structural("help lists the views", lambda: taskkit.file_contains(VIEW, 'styKey.Render("VIEWS")'))
    gate.structural("gofmt clean", lambda: plan_lib.gofmt_clean("internal/tui/header.go", VIEW))
    gate.structural("goldens regenerated", goldens_regenerated)
    # Source plan, Task 4 Step 4: what each regenerated screen must show.
    for name, width in (("screen-100", 100), ("screen-160", 160)):
        gate.structural(name + ": three header rows, the rule, keys in the status bar", lambda n=name: _wide_header(n))
    gate.structural("screen-60: one header row, then the rule", _narrow_header)
    for name, width, height in (("screen-60", 60, 24), ("screen-100", 100, 24), ("screen-160", 160, 24),
                                ("sessions-all-100", 100, 24), ("tool-expanded-100", 100, 30),
                                ("approval-insert-100", 100, 30), ("approval-normal-100", 100, 30)):
        gate.structural("%s fits %dx%d" % (name, width, height), lambda n=name, w=width, h=height: plan_lib.golden_fits(n, w, h))
    gate.structural("too-small is still exactly 'terminal too small'",
                    lambda: plan_lib.golden("too-small") == ["terminal too small"])
    gate.component("tui tests, race", gotest("./internal/tui/"))


if __name__ == "__main__":
    raise SystemExit(gate.run(apply, verify))
