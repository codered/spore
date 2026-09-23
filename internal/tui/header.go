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
	return strings.Join(rows, "\n")
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
	title := "chat"
	if m.table != nil {
		title = m.table.title()
	}
	head := "─ " + title + " "
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
	return hint("i", "type") + "  " + hint("j/k", "session") + "  " + hint("n", "new") + "  " + hint("?", "help")
}
