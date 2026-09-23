package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// row is one sidebar line: the jobs folder, a job's label inside it, a
// workspace heading, a session, or a "+N more" marker. Only session rows are
// selectable.
type row struct {
	header string
	id     string
	depth  int
	more   int
	// folder marks the jobs folder row: runs is how many job runs it holds,
	// unread how many of them nobody has opened.
	folder       bool
	runs, unread int
	busy         bool // a run is in progress
	// label names a job inside the open folder.
	label string
}

// idleShownPerWorkspace bounds how many idle top-level sessions a workspace
// lists before the rest collapse into "+N more".
const idleShownPerWorkspace = 10

// rows lays out the sidebar. By default it shows chat sessions, sessions of
// unknown source, their sub-agents, and anything working or blocked; showAll
// adds idle sessions from other sources; a filter matches title or id across
// everything. The
// selected session always shows, so the chat pane never displays a session
// the sidebar has hidden.
func (c *cache) rows(showAll bool, filter, selected string) []row {
	kids := map[string][]*sessionView{}
	var roots, jobRuns []*sessionView
	for _, sv := range c.sessions {
		if sv.info.ID == "" {
			continue
		}
		switch p := sv.info.ParentID; {
		case p != "" && c.sessions[p] != nil:
			kids[p] = append(kids[p], sv)
		case sv.info.Source == "job":
			// A job run lives in the jobs folder, not under its workspace:
			// its workspace is a throwaway session directory.
			jobRuns = append(jobRuns, sv)
		default:
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
	byRecent(jobRuns)
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
		// "unknown" is every session from before sources were recorded: the
		// user's whole history, which the default view must not hide.
		switch sv.info.Source {
		case "chat", "unknown":
			return true
		}
		return c.State(id) != daemon.SessionIdle
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

	out = append(out, c.jobFolder(jobRuns, filter, selected, visible, subtree, addKids)...)

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
		// Only show header if workspace is non-empty
		if ws != "" {
			out = append(out, row{header: ws})
		}
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
		case r.folder:
			lines = append(lines, folderLine(r, c.jobsOpen, width))
		case r.label != "":
			lines = append(lines, styMuted.Render(clip("  "+oneLine(r.label), width)))
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
	// Ensure the line before the tag fits within width constraints
	budget := width
	if tag != "" {
		budget = width - len(tag) - 1
	}
	if lipgloss.Width(line) > budget {
		line = ansi.Truncate(line, budget, "…")
	}
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

// jobFolder is the jobs folder: its row, always, and inside it, while it is
// open or a filter is searching it, each job's runs under the job's label,
// newest job first. A selected run always shows, open or not, so the chat
// pane never displays a session the sidebar has hidden.
func (c *cache) jobFolder(runs []*sessionView, filter, selected string,
	visible, subtree func(*sessionView) bool, addKids func(*sessionView, int, bool)) []row {
	folder := row{folder: true, runs: len(runs)}
	for _, r := range runs {
		if r.info.Unread {
			folder.unread++
		}
		if r.working {
			folder.busy = true
		}
	}
	out := []row{folder}
	open := c.jobsOpen || filter != ""

	byJob := map[int64][]*sessionView{}
	var order []int64
	for _, r := range runs {
		shown := r.info.ID == selected || (open && (filter == "" || subtree(r)))
		if !shown {
			continue
		}
		if _, seen := byJob[r.info.JobID]; !seen {
			order = append(order, r.info.JobID)
		}
		byJob[r.info.JobID] = append(byJob[r.info.JobID], r)
	}
	for _, job := range order {
		group := byJob[job]
		label := "earlier runs"
		if job > 0 {
			label = fmt.Sprintf("job %d · %s", job, group[0].info.Title)
		}
		out = append(out, row{label: label})
		hidden := 0
		for i, r := range group {
			if filter == "" && i >= idleShownPerWorkspace && r.info.ID != selected {
				hidden++
				continue
			}
			out = append(out, row{id: r.info.ID, depth: 1})
			addKids(r, 2, filter == "" && visible(r))
		}
		if hidden > 0 {
			out = append(out, row{more: hidden})
		}
	}
	return out
}

// folderLine draws the jobs folder: muted while it has never held a run,
// the accent colour while a run is going, and with a count badge while
// finished runs wait unopened.
func folderLine(r row, open bool, width int) string {
	arrow := "▸"
	if open {
		arrow = "▾"
	}
	text := arrow + " jobs"
	switch {
	case r.unread > 0:
		badge := styBadge.Render(fmt.Sprintf(" %d ", r.unread))
		return clip(styAccent.Bold(true).Render(text)+" "+badge, width)
	case r.busy:
		return styAccent.Render(clip(text, width))
	case r.runs == 0:
		return styMuted.Render(clip(text, width))
	}
	return styHeader.Render(clip(text, width))
}
