package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// row is one sidebar line: a workspace heading, a session, or a "+N more"
// marker. Only session rows are selectable.
type row struct {
	header string
	id     string
	depth  int
	more   int
}

// idleShownPerWorkspace bounds how many idle top-level sessions a workspace
// lists before the rest collapse into "+N more".
const idleShownPerWorkspace = 10

// rows lays out the sidebar. By default it shows chat sessions, their
// sub-agents, and anything working or blocked; showAll adds idle sessions
// from other sources; a filter matches title or id across everything. The
// selected session always shows, so the chat pane never displays a session
// the sidebar has hidden.
func (c *cache) rows(showAll bool, filter, selected string) []row {
	kids := map[string][]*sessionView{}
	var roots []*sessionView
	for _, sv := range c.sessions {
		if sv.info.ID == "" {
			continue
		}
		if p := sv.info.ParentID; p != "" && c.sessions[p] != nil {
			kids[p] = append(kids[p], sv)
		} else {
			roots = append(roots, sv)
		}
	}
	byRecent := func(s []*sessionView) {
		sort.SliceStable(s, func(i, j int) bool {
			if !s[i].info.UpdatedAt.Equal(s[j].info.UpdatedAt) {
				return s[i].info.UpdatedAt.After(s[j].info.UpdatedAt)
			}
			return s[i].info.ID < s[j].info.ID
		})
	}
	byRecent(roots)
	for k := range kids {
		byRecent(kids[k])
	}

	filter = strings.ToLower(filter)
	visible := func(sv *sessionView) bool {
		id := sv.info.ID
		if id == selected {
			return true
		}
		if filter != "" {
			return strings.Contains(strings.ToLower(sv.info.Title), filter) || strings.HasPrefix(id, filter)
		}
		if showAll {
			return true
		}
		st := c.State(id)
		return sv.info.Source == "chat" || st != daemon.SessionIdle
	}
	var subtree func(sv *sessionView) bool
	subtree = func(sv *sessionView) bool {
		if visible(sv) {
			return true
		}
		for _, k := range kids[sv.info.ID] {
			if subtree(k) {
				return true
			}
		}
		return false
	}

	var out []row
	var addKids func(parent *sessionView, depth int, all bool)
	addKids = func(parent *sessionView, depth int, all bool) {
		for _, k := range kids[parent.info.ID] {
			if all || subtree(k) {
				out = append(out, row{id: k.info.ID, depth: depth})
				addKids(k, depth+1, all)
			}
		}
	}

	groups := map[string][]*sessionView{}
	var order []string
	for _, r := range roots {
		if !subtree(r) {
			continue
		}
		ws := r.info.Workspace
		if _, seen := groups[ws]; !seen {
			order = append(order, ws)
		}
		groups[ws] = append(groups[ws], r)
	}
	for _, ws := range order {
		out = append(out, row{header: ws})
		idle, hidden := 0, 0
		for _, r := range groups[ws] {
			if filter == "" && r.info.ID != selected && c.State(r.info.ID) == daemon.SessionIdle {
				idle++
				if idle > idleShownPerWorkspace {
					hidden++
					continue
				}
			}
			out = append(out, row{id: r.info.ID})
			// A shown root lists all its children unless a filter is
			// narrowing the view.
			addKids(r, 1, filter == "" && visible(r))
		}
		if hidden > 0 {
			out = append(out, row{more: hidden})
		}
	}
	return out
}

// renderSidebar draws rows into width x height, scrolling to keep the
// selection on screen.
func renderSidebar(c *cache, rows []row, selected string, width, height int) string {
	lines := make([]string, 0, len(rows))
	sel := -1
	for _, r := range rows {
		switch {
		case r.header != "":
			lines = append(lines, styHeader.Render(clip(tildePath(r.header), width)))
		case r.more > 0:
			lines = append(lines, styMuted.Render(fmt.Sprintf("  +%d more", r.more)))
		default:
			if r.id == selected {
				sel = len(lines)
			}
			lines = append(lines, sessionRow(c, r, width, r.id == selected))
		}
	}
	if height > 0 && len(lines) > height {
		start := 0
		if sel >= height {
			start = sel - height + 1
		}
		lines = lines[start:min(len(lines), start+height)]
	}
	return strings.Join(lines, "\n")
}

func sessionRow(c *cache, r row, width int, selected bool) string {
	sv := c.sessions[r.id]
	glyph := styMuted.Render("○")
	switch c.State(r.id) {
	case daemon.SessionWorking:
		glyph = styAccent.Render("●")
	case daemon.SessionBlocked:
		glyph = styWarn.Render("◐")
	}
	lead := ""
	if r.depth > 0 {
		lead = strings.Repeat("  ", r.depth-1) + "└ "
	}
	title := sv.info.Title
	if title == "" {
		title = "untitled"
	}
	tag := sourceTag(sv.info.Source)
	prefix := lead + glyph + " " + short(r.id) + " "
	room := width - lipgloss.Width(prefix) - len(tag) - 1
	line := prefix + clip(oneLine(title), room)
	if tag != "" {
		pad := max(1, width-lipgloss.Width(line)-len(tag))
		line += strings.Repeat(" ", pad) + styMuted.Render(tag)
	}
	if selected {
		plain := ansi.Strip(line)
		return stySelected.Render(plain + strings.Repeat(" ", max(0, width-lipgloss.Width(plain))))
	}
	return line
}

// sourceTag is the short label at the right of a row. A sub-agent needs none:
// its indentation says what it is.
func sourceTag(src string) string {
	switch src {
	case "chat":
		return "chat"
	case "discord":
		return "disc"
	case "job":
		return "job"
	}
	return ""
}
