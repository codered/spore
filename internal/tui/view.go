package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

func (m *Model) tooSmall() bool { return m.width < minWidth || m.height < minHeight }

func (m *Model) sidebarOn() bool {
	if m.sidebarPref != nil {
		return *m.sidebarPref
	}
	return m.width >= sidebarMinTerm
}

// sidebarOuter is the sidebar pane's width with its border.
const sidebarOuter = sidebarWidth + 2

// chatOuter is the chat pane's width with its border.
func (m *Model) chatOuter() int {
	if m.sidebarOn() {
		return max(24, m.width-sidebarOuter)
	}
	return max(24, m.width)
}

// mainWidth is the width inside the chat pane's border and padding.
func (m *Model) mainWidth() int { return m.chatOuter() - 4 }

// paneHeight is the rows inside a pane's border.
func (m *Model) paneHeight() int { return max(1, m.bodyHeight()-2) }

// sync re-lays-out after every update: input width, viewport size, and the
// transcript of the selected session.
func (m *Model) sync() {
	if m.mode == modeInsert {
		m.input.Placeholder = placeholderInsert
	} else {
		m.input.Placeholder = placeholderNormal
	}
	w := m.mainWidth()
	m.input.SetWidth(max(10, w-4))
	m.line.Width = max(10, w-6)
	if m.mdWidth != w {
		m.md, m.mdWidth = newMarkdown(w), w
	}
	m.vp.Width = w
	// One row inside the pane is the session's facts.
	m.vp.Height = max(1, m.paneHeight()-1-lipgloss.Height(m.inputView())-m.draftLineHeight())

	base := m.transcript(w)
	content := base
	if wl := m.workingLine(); wl != "" {
		if base != "" {
			content += "\n\n"
		}
		content += wl
	}
	if content != m.lastContent {
		m.vp.SetContent(content)
		// A spinner frame changes content but is not something new to read.
		if base != m.lastBase && !m.follow {
			m.unseen = true
		}
		m.lastContent, m.lastBase = content, base
	}
	if m.scrollToTool && m.toolCursor >= 0 && m.toolCursor < len(m.toolLines) {
		m.follow = false
		m.vp.SetYOffset(max(0, m.toolLines[m.toolCursor]-2))
	}
	m.scrollToTool = false
	if m.follow {
		m.vp.GotoBottom()
		m.unseen = false
	}
}

// transcript joins the selected session's blocks, recording the line each
// tool block starts on so the tool cursor can scroll to it.
func (m *Model) transcript(width int) string {
	m.toolLines = m.toolLines[:0]
	if m.selected == "" {
		return ""
	}
	var b strings.Builder
	line, tool := 0, 0
	var prev *block
	for _, blk := range m.cache.get(m.selected).blocks {
		if prev != nil {
			sep := "\n"
			if blk.kind == kindUser || prev.kind == kindFooter {
				sep = "\n\n"
			}
			b.WriteString(sep)
			line += strings.Count(sep, "\n")
		}
		selected := false
		if blk.kind == kindTool {
			selected = tool == m.toolCursor
			m.toolLines = append(m.toolLines, line)
			tool++
		}
		out := blk.render(width, m.md, selected)
		b.WriteString(out)
		line += strings.Count(out, "\n")
		prev = blk
	}
	return b.String()
}

func (m *Model) View() string {
	if m.tooSmall() {
		return "terminal too small"
	}
	body := m.mainView()
	if m.sidebarOn() && m.table == nil {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(), body)
	}
	if m.mode == modeConfirm && m.confirm != nil {
		body = placeOver(body, m.modalView(), m.width)
	}
	return lipgloss.JoinVertical(lipgloss.Left, m.headerView(), body, m.statusView())
}

func (m *Model) mainView() string {
	if m.table != nil && !m.help {
		w, h := m.width-2, m.paneHeight()
		body := lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h).Render(m.table.render(w, h))
		return paneBox(m.table.title(), body, m.width, m.bodyHeight(), true, 0, false)
	}
	on := m.focused() == paneChat
	if m.help {
		return paneBox("help", helpText(), m.chatOuter(), m.bodyHeight(), on, 1, false)
	}
	vpView := m.vp.View()
	if card := m.approvalCard(m.vp.Width, m.vp.Height); card != "" {
		vpView = placeOver(faint(vpView), card, m.vp.Width)
	}
	parts := []string{clip(m.sessionFacts(), m.mainWidth()), vpView, m.inputView()}
	if dl := m.draftLine(); dl != "" {
		parts = append(parts, dl)
	}
	return paneBox(m.paneTitle(), strings.Join(parts, "\n"), m.chatOuter(), m.bodyHeight(), on, 1, m.waiting())
}

func (m *Model) sidebarView() string {
	h := m.paneHeight()
	body := renderSidebar(m.cache, m.cache.rows(m.showAll, m.filter, m.selected), m.selected, m.sideCursor, sidebarWidth, h)
	return paneBox("sessions", withLegend(body, h), sidebarOuter, m.bodyHeight(), m.focused() == paneSidebar, 0, false)
}

// withLegend puts the state legend on the last of h rows when body leaves a
// blank row above it. Otherwise the rows win and there is no legend.
func withLegend(body string, h int) string {
	if body == "" {
		return strings.Repeat("\n", max(0, h-1)) + legendLine()
	}
	n := strings.Count(body, "\n") + 1
	if n > h-2 {
		return body
	}
	return body + strings.Repeat("\n", h-n) + legendLine()
}

// legendLine says what the sidebar's state glyphs mean.
func legendLine() string {
	return styAccent.Render("●") + styMuted.Render(" working  ") +
		styWarn.Render("◐") + styMuted.Render(" blocked  ○ idle")
}

// paneBox frames body in w x h cells with title in the top border and pad
// blank columns inside each side. The focused pane gets a heavy accent
// border, the other a light muted one, so the difference survives a
// terminal without colour. warn outranks focus: a pane waiting on the person
// is heavy and amber whether or not it has focus.
func paneBox(title, body string, w, h int, focused bool, pad int, warn bool) string {
	iw, ih := max(1, w-2), max(1, h-2)
	cw := max(1, iw-2*pad)
	gap := strings.Repeat(" ", pad)
	b, sty, tsty := lipgloss.RoundedBorder(), styMuted, styMuted
	switch {
	case warn:
		b, sty, tsty = lipgloss.ThickBorder(), styWarn, styApprovalTitle
	case focused:
		b, sty, tsty = lipgloss.ThickBorder(), styAccent, styKey
	}
	head := " " + clip(oneLine(title), max(0, iw-3)) + " "
	top := sty.Render(b.TopLeft+b.Top) + tsty.Render(head) +
		sty.Render(strings.Repeat(b.Top, max(0, iw-1-lipgloss.Width(head)))+b.TopRight)
	inner := lipgloss.NewStyle().Width(cw).Height(ih).MaxHeight(ih).Render(body)
	lines := []string{top}
	for _, l := range strings.Split(inner, "\n") {
		l = ansi.Truncate(l, cw, "")
		l += strings.Repeat(" ", max(0, cw-lipgloss.Width(l)))
		lines = append(lines, sty.Render(b.Left)+gap+l+gap+sty.Render(b.Right))
	}
	lines = append(lines, sty.Render(b.BottomLeft+strings.Repeat(b.Bottom, iw)+b.BottomRight))
	return strings.Join(lines, "\n")
}

// modalView is the confirmation on screen: the question, what it concerns,
// and the keys that answer it.
func (m *Model) modalView() string {
	c := m.confirm
	lines := []string{styApprovalTitle.Render(c.question)}
	for _, d := range c.detail {
		lines = append(lines, styMuted.Render(d))
	}
	lines = append(lines, "", confirmKeys(c))
	cw := 0
	for _, l := range lines {
		cw = max(cw, lipgloss.Width(l))
	}
	// The border and padding take six columns; keep two spare each side.
	cw = min(cw, max(10, m.width-10))
	return styModal.Width(cw + 4).Render(strings.Join(lines, "\n"))
}

// placeOver draws fg centred on bg, which is width cells wide.
func placeOver(bg, fg string, width int) string {
	rows, over := strings.Split(bg, "\n"), strings.Split(fg, "\n")
	fw := lipgloss.Width(fg)
	x, y := max(0, (width-fw)/2), max(0, (len(rows)-len(over))/2)
	for i, f := range over {
		j := y + i
		if j >= len(rows) {
			break
		}
		left := ansi.Truncate(rows[j], x, "")
		left += strings.Repeat(" ", max(0, x-lipgloss.Width(left)))
		f += strings.Repeat(" ", max(0, fw-lipgloss.Width(f)))
		// Reset after each cut so a style open in bg does not bleed.
		rows[j] = left + "\x1b[m" + f + "\x1b[m" + ansi.TruncateLeft(rows[j], x+fw, "")
	}
	return strings.Join(rows, "\n")
}

func (m *Model) inputView() string {
	w := max(10, m.mainWidth()-2)
	switch m.mode {
	case modeCommand:
		return styInputBox.Width(w).Render(":" + m.line.View())
	case modeFilter:
		return styInputBox.Width(w).Render("/" + m.line.View())
	case modeInsert:
		return styInputBox.Width(w).Render(m.input.View())
	}
	return styInputIdle.Width(w).Render(m.input.View())
}

// waitingTool names the latest call in the transcript's trailing run of
// tool calls that has no result yet.
func waitingTool(sv *sessionView) string {
	for i := len(sv.blocks) - 1; i >= 0 && sv.blocks[i].kind == kindTool; i-- {
		if !sv.blocks[i].done {
			return sv.blocks[i].tool
		}
	}
	return ""
}

// spinnerFrames turn once per frameEvery while a turn runs.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// workingLine ends the transcript while the selected session's turn runs:
// a spinner, what spore is doing, and for how long. It hides while an
// approval waits: then spore is waiting on the person, and the card says so.
func (m *Model) workingLine() string {
	if m.selected == "" || m.cache.State(m.selected) != daemon.SessionWorking {
		return ""
	}
	sv := m.cache.get(m.selected)
	label := "thinking"
	if b := sv.last(); b != nil && b.kind == kindText && b.streaming {
		label = "writing"
	} else if tool := waitingTool(sv); tool != "" {
		label = "running " + tool
	}
	now := m.opts.Now()
	if !sv.started.IsZero() {
		label += " · " + humanDur(now.Sub(sv.started))
	}
	frame := spinnerFrames[int(now.UnixMilli()/frameEvery.Milliseconds())%len(spinnerFrames)]
	return styAccent.Render(frame) + " " + styMuted.Render(label)
}

// statusView is the mode badge and the keys that work here, with the few
// signals that need the user now on the right.
func (m *Model) statusView() string {
	badge := styMode.Render(" " + m.mode.String() + " ")
	if m.approvalBar() {
		badge = styModeWarn.Render(" APPROVAL ")
	}
	var right []string
	if m.unseen {
		right = append(right, styAccent.Render("↓ new"))
	}
	if m.mode == modeNormal && m.table == nil && m.cache.State(m.selected) != daemon.SessionIdle {
		right = append(right, styWarn.Bold(true).Render("esc stop"))
	}
	if m.viewErr != "" {
		right = append(right, styDanger.Render(m.viewErr))
	}
	if m.flash != "" {
		right = append(right, styAccent.Render(m.flash))
	}
	r := strings.Join(right, styMuted.Render(" · "))
	room := m.width - lipgloss.Width(badge) - 1 - lipgloss.Width(r) - 1
	return fitRow(badge+" "+m.keyHints(max(0, room)), r, m.width)
}

func helpText() string {
	return strings.Join([]string{
		styKey.Render("NORMAL"),
		"  tab       sidebar / chat focus         i/a/enter  type into the session",
		"  j/k       sessions in the sidebar, scroll in the chat",
		"  ctrl+d/u  half page                    g/G        top / bottom",
		"  [ ]       previous / next tool call    o / O      expand one / all",
		"  /         filter sessions              :          command",
		"  n         new session here             b          next blocked session",
		"  x         stop the selected sub-agent  esc        stop the running turn",
		"  ctrl+b    toggle the sidebar           q          quit",
		"  d         delete the session (y here, D also on Discord)   :delete all  delete every session",
		"  z         open / close the jobs folder     enter on the folder opens / closes it; on a job, lists its runs",
		"",
		styKey.Render("APPROVAL") + "  (normal mode, while one is showing)",
		"  y allow once · n deny · s allow the tool this session · p always allow the pattern",
		"  each asks first: y confirms, esc cancels",
		"  n answers the approval, not \"new session\", until it is answered",
		"",
		styKey.Render("INSERT"),
		"  enter send · ctrl+j newline · ↑↓ history · esc normal mode",
		"",
		styKey.Render("VIEWS") + "  (normal mode)",
		"  S skills · A agents · U usage · J jobs · or :skills :agents :usage :jobs",
		"  in a view: j/k move · / filter · s sort · enter open · x act on the row · ctrl+r refresh · esc back",
		"  jobs: enter lists a job's runs · runs: enter shows a run's output · o opens the run in the chat",
		"",
		styKey.Render("COMMANDS"),
		"  :new [dir]  :sessions [all]  :clear  :compact  :context  :usage  :skills  :agents  :q",
		"",
		styMuted.Render("any key closes this"),
	}, "\n")
}
