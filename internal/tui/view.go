package tui

import (
	"fmt"
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

// mainWidth is the width of the chat pane: the terminal minus the sidebar
// and its one-column border.
func (m *Model) mainWidth() int {
	if m.sidebarOn() {
		return max(20, m.width-sidebarWidth-1)
	}
	return max(20, m.width)
}

// sync re-lays-out after every update: input width, viewport size, and the
// transcript of the selected session.
func (m *Model) sync() {
	w := m.mainWidth()
	m.input.SetWidth(max(10, w-4))
	m.line.Width = max(10, w-6)
	if m.mdWidth != w {
		m.md, m.mdWidth = newMarkdown(w), w
	}
	m.vp.Width = w
	m.vp.Height = max(1, m.height-1-lipgloss.Height(m.inputView())-m.overlayHeight())

	content := m.transcript(w)
	if content != m.lastContent {
		m.lastContent = content
		m.vp.SetContent(content)
		if !m.follow {
			m.unseen = true
		}
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
	if m.sidebarOn() {
		body = lipgloss.JoinHorizontal(lipgloss.Top, m.sidebarView(), body)
	}
	return lipgloss.JoinVertical(lipgloss.Left, body, m.statusView())
}

func (m *Model) mainView() string {
	w, h := m.mainWidth(), m.height-1
	box := lipgloss.NewStyle().Width(w).Height(h).MaxHeight(h)
	if m.help {
		return box.Render(helpText())
	}
	parts := []string{m.vp.View()}
	if ov := m.overlayView(); ov != "" {
		parts = append(parts, ov)
	}
	parts = append(parts, m.inputView())
	return box.Render(strings.Join(parts, "\n"))
}

func (m *Model) sidebarView() string {
	h := m.height - 1
	body := renderSidebar(m.cache, m.cache.rows(m.showAll, m.filter, m.selected), m.selected, sidebarWidth, h)
	return stySidebar.Width(sidebarWidth).Height(h).MaxHeight(h).Render(body)
}

func (m *Model) inputView() string {
	w := max(10, m.mainWidth()-2)
	switch m.mode {
	case modeCommand:
		return styInputBox.Width(w).Render(":" + m.line.View())
	case modeFilter:
		return styInputBox.Width(w).Render("/" + m.line.View())
	case modeConfirm:
		prompt := ""
		if m.confirm != nil {
			prompt = m.confirm.prompt
		}
		return styApprovalBox.Width(w).Render(prompt)
	case modeInsert:
		return styInputBox.Width(w).Render(m.input.View())
	}
	return styInputIdle.Width(w).Render(m.input.View())
}

func (m *Model) overlayView() string {
	_, ev, ok := m.cache.approvalFor(m.selected)
	if !ok {
		return ""
	}
	var b strings.Builder
	title := "spore wants to run " + ev.Tool
	if ev.Origin != "" {
		title = "sub-agent " + short(ev.Origin) + ": " + title
	}
	b.WriteString(styApprovalTitle.Render(title) + "\n")
	b.WriteString(styMuted.Render(`matched policy rule "`+ev.Rule+`"`) + "\n")
	b.WriteString(prettyArgs(ev.Args, 8) + "\n")
	if m.mode != modeNormal {
		b.WriteString(styKey.Render("esc") + styMuted.Render(", then y/n/s/p"))
	} else {
		keys := []string{
			styKey.Render("y") + styMuted.Render(" allow once"),
			styKey.Render("n") + styMuted.Render(" deny"),
			styKey.Render("s") + styMuted.Render(" allow "+ev.Tool+" this session"),
		}
		if ev.Pattern != "" {
			keys = append(keys, styKey.Render("p")+styMuted.Render(" always allow "+ev.Pattern))
		}
		b.WriteString(strings.Join(keys, styMuted.Render("  ·  ")))
	}
	return styApprovalBox.Width(max(10, m.mainWidth()-2)).Render(b.String())
}

func (m *Model) overlayHeight() int {
	if ov := m.overlayView(); ov != "" {
		return lipgloss.Height(ov)
	}
	return 0
}

func (m *Model) statusView() string {
	sv := m.cache.get(m.selected)
	src := sv.info.Source
	if src == "" {
		src = "unknown"
	}
	left := styMode.Render(" "+m.mode.String()+" ") + " " + short(m.selected) +
		styMuted.Render(" · "+src+" · "+tildePath(sv.info.Workspace))

	var right []string
	if sv.model != "" {
		right = append(right, sv.model)
	}
	if sv.ctxTokens > 0 {
		right = append(right, "ctx "+humanTokens(sv.ctxTokens))
	}
	if m.opts.ShowCost && sv.cost > 0 {
		right = append(right, fmt.Sprintf("$%.2f", sv.cost))
	}
	if n := m.cache.BlockedCount(); n > 0 {
		right = append(right, styWarn.Render(fmt.Sprintf("%d blocked", n)))
	}
	if m.unseen {
		right = append(right, styAccent.Render("↓ new"))
	}
	if m.mode == modeNormal && m.cache.State(m.selected) != daemon.SessionIdle {
		right = append(right, styKey.Render("esc")+" stop")
	}
	if m.reconnecting {
		right = append(right, styDanger.Render("reconnecting…"))
	}
	r := strings.Join(right, styMuted.Render(" · "))
	gap := m.width - lipgloss.Width(left) - lipgloss.Width(r)
	if gap < 1 {
		return ansi.Truncate(left+" "+r, m.width, "…")
	}
	return left + strings.Repeat(" ", gap) + r
}

func helpText() string {
	return strings.Join([]string{
		styKey.Render("NORMAL"),
		"  j/k       move between sessions        i/a/enter  type into the session",
		"  ctrl+d/u  half page                    g/G        top / bottom",
		"  [ ]       previous / next tool call    o / O      expand one / all",
		"  /         filter sessions              :          command",
		"  n         new session here             b          next blocked session",
		"  x         stop the selected sub-agent  esc        stop the running turn",
		"  ctrl+b    toggle the sidebar           q          quit",
		"",
		styKey.Render("APPROVAL") + "  (normal mode, while one is showing)",
		"  y allow once · n deny · s allow the tool this session · p always allow the pattern",
		"  n answers the approval, not \"new session\", until it is answered",
		"",
		styKey.Render("INSERT"),
		"  enter send · ctrl+j newline · ↑↓ history · esc normal mode",
		"",
		styKey.Render("COMMANDS"),
		"  :new [dir]  :sessions [all]  :clear  :compact  :context  :usage  :skills  :agents  :q",
		"",
		styMuted.Render("any key closes this"),
	}, "\n")
}
