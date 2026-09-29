package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// bodyHeight is the rows between the tab bar and the status bar.
func (m *Model) bodyHeight() int { return max(1, m.height-2) }

// headerView is the top nav: the brand, a tab per screen with the one on
// show lit, and on the right the signals that concern every session.
func (m *Model) headerView() string {
	right := m.globalFacts()
	left := m.tabBar(true)
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.width {
		// Narrow: the names matter more than the letters, which ? lists.
		left = m.tabBar(false)
	}
	return fitRow(left, right, m.width)
}

// tabBar is the brand and a tab per screen, with or without hotkeys.
func (m *Model) tabBar(hotkeys bool) string {
	active := m.activeTab()
	tabs := []string{tab("chat", "", active == "chat")}
	for _, r := range resources() {
		key := ""
		if hotkeys {
			key = r.Hotkey()
		}
		tabs = append(tabs, tab(r.Name(), key, active == r.Name()))
	}
	return styMode.Render(" spore ") + " " + strings.Join(tabs, "")
}

// tab is one entry of the top nav: its name and hotkey, reversed when lit.
func tab(name, hotkey string, on bool) string {
	if on {
		label := " " + name + " "
		if hotkey != "" {
			label += hotkey + " "
		}
		return styTabOn.Render(label)
	}
	out := " " + styMuted.Render(name)
	if hotkey != "" {
		out += " " + styKey.Render(hotkey)
	}
	return out + " "
}

// tabber is a view that belongs under another view's tab, such as a job's
// runs under jobs.
type tabber interface{ Tab() string }

// activeTab names the tab for what the body shows.
func (m *Model) activeTab() string {
	if m.table == nil {
		return "chat"
	}
	if t, ok := m.table.res.(tabber); ok {
		return t.Tab()
	}
	return m.table.res.Name()
}

// globalFacts is what the top nav reports about every session at once.
func (m *Model) globalFacts() string {
	var out []string
	if n := m.cache.BlockedCount(); n > 0 {
		out = append(out, styWarn.Render(fmt.Sprintf("%d blocked", n)))
	}
	if m.reconnecting {
		out = append(out, styDanger.Render("reconnecting…"))
	}
	return strings.Join(out, styMuted.Render(" · "))
}

// sessionFacts is the chat pane's first line: where the selected session
// runs and what it costs.
func (m *Model) sessionFacts() string {
	sv := m.cache.get(m.selected)
	src := sv.info.Source
	if src == "" {
		src = "unknown"
	}
	out := []string{src}
	if ws := sv.info.Workspace; ws != "" {
		out = append(out, tildePath(ws))
	}
	if sv.model != "" {
		out = append(out, sv.model)
	}
	if sv.ctxTokens > 0 {
		out = append(out, "ctx "+humanTokens(sv.ctxTokens))
	}
	if m.opts.ShowCost && sv.cost > 0 {
		out = append(out, fmt.Sprintf("$%.2f", sv.cost))
	}
	return styMuted.Render(strings.Join(out, " · "))
}

// paneTitle is the chat pane's border title: the session's id and title.
func (m *Model) paneTitle() string {
	if m.selected == "" {
		return "no session"
	}
	title := m.cache.get(m.selected).info.Title
	if title == "" {
		title = "untitled"
	}
	return short(m.selected) + " " + oneLine(title)
}

func hint(key, label string) string {
	return styKey.Render(key) + " " + styMuted.Render(label)
}

// fitHints joins hints two spaces apart, dropping them from the end until
// they fit in room cells. The last hint always stays: it is the way out, or
// the way to the rest of the keys.
func fitHints(parts []string, room int) string {
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	head := parts[:len(parts)-1]
	for n := len(head); n > 0; n-- {
		s := strings.Join(append(append([]string{}, head[:n]...), last), "  ")
		if lipgloss.Width(s) <= room {
			return s
		}
	}
	return last
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

// keyHints is the status bar's left half: the keys that work in this mode,
// fitted to room cells.
func (m *Model) keyHints(room int) string {
	var parts []string
	switch m.mode {
	case modeInsert:
		parts = []string{hint("enter", "send"), hint("ctrl+j", "newline"), hint("esc", "normal")}
	case modeCommand:
		parts = []string{hint("enter", "run"), hint("tab", "complete"), hint("esc", "cancel")}
	case modeFilter:
		parts = []string{hint("enter", "keep"), hint("esc", "clear")}
	case modeConfirm:
		if m.confirm == nil {
			return ""
		}
		parts = confirmParts(m.confirm)
	default:
		parts = m.normalHints()
	}
	return fitHints(parts, room)
}

// normalHints are the NORMAL-mode keys for what is on screen.
func (m *Model) normalHints() []string {
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
		return append(parts, hint("esc", esc))
	}
	if m.focused() == paneSidebar {
		return []string{hint("tab", "chat"), hint("j/k", "session"), hint("enter", "open"), hint("i", "type"), hint("n", "new"), hint("?", "help")}
	}
	var parts []string
	if m.sidebarOn() {
		parts = append(parts, hint("tab", "sessions"))
	}
	parts = append(parts, hint("i", "type"), hint("j/k", "scroll"), hint("[ ]", "tools"), hint("o", "expand"), hint("n", "new"))
	if m.nextBlockedID() != "" {
		parts = append(parts, hint("b", "next blocked"))
	}
	return append(parts, hint(":", "cmd"), hint("?", "help"))
}

// confirmParts are the modal's answers: y, D when it applies, and esc.
func confirmParts(c *confirmState) []string {
	yes := c.yesLabel
	if yes == "" {
		yes = "confirm"
	}
	parts := []string{hint("y", yes)}
	if c.alt != nil {
		parts = append(parts, hint("D", c.altLabel))
	}
	return append(parts, hint("esc", "cancel"))
}

// confirmKeys is the modal's answer line.
func confirmKeys(c *confirmState) string { return strings.Join(confirmParts(c), "  ") }
