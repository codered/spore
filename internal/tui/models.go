package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/modelcmd"
)

// modelRows caps how many options the modal shows at once.
const modelRows = 12

// modelsState is the /model modal. tab 0 is the overview; tab i is
// view.Ops[i-1]. cursor is a row on the overview, an option on an op tab.
type modelsState struct {
	view   daemon.ModelsJSON
	loaded bool
	tab    int
	cursor int
	note   string
	isErr  bool
	back   mode
}

// modelsMsg carries a models view from the daemon: a fetch, or the answer
// to a choice (op set, with the confirmation to show).
type modelsMsg struct {
	view daemon.ModelsJSON
	op   string
	err  error
}

// openModels shows the modal and fetches what to put in it. It returns to
// INSERT if opened from there, so a draft survives a look at the models.
func (m *Model) openModels() tea.Cmd {
	back := modeNormal
	if m.mode == modeInsert {
		back = modeInsert
	}
	m.models = &modelsState{back: back}
	m.mode = modeModels
	m.input.Blur()
	return m.fetchModels(false)
}

func (m *Model) fetchModels(fresh bool) tea.Cmd {
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		v, err := be.Models(ctx, id, fresh)
		return modelsMsg{view: v, err: err}
	}
}

func (m *Model) modelsLoaded(msg modelsMsg) {
	s := m.models
	if s == nil || m.mode != modeModels {
		return // closed meanwhile
	}
	if msg.err != nil {
		s.note, s.isErr = msg.err.Error(), true
		if !s.loaded {
			s.note += " — press r to retry"
		}
		return
	}
	s.view, s.loaded = msg.view, true
	s.tab = min(s.tab, len(s.view.Ops)) // a refresh may have dropped the op
	s.note, s.isErr = "", false
	if msg.op != "" {
		s.note = strings.TrimSpace(modelcmd.Confirm(s.view, msg.op))
		s.cursor = 0 // the chosen model is listed first
	}
	s.cursor = min(s.cursor, max(0, m.modelRowCount()-1))
}

// modelRowCount is how many rows the current tab has.
func (m *Model) modelRowCount() int {
	s := m.models
	if s.tab == 0 {
		return len(s.view.Ops)
	}
	return len(modelcmd.Options(s.view, s.view.Ops[s.tab-1].Op))
}

func (m *Model) closeModels() tea.Cmd {
	back := m.models.back
	m.models = nil
	if back == modeInsert {
		return m.enterInsert()
	}
	m.mode = modeNormal
	return nil
}

func (m *Model) keyModels(k tea.KeyMsg) tea.Cmd {
	s := m.models
	tabs := len(s.view.Ops) + 1
	switch k.String() {
	case "esc", "q":
		return m.closeModels()
	case "tab", "l", "right":
		s.tab, s.cursor = (s.tab+1)%tabs, 0
	case "shift+tab", "h", "left":
		s.tab, s.cursor = (s.tab+tabs-1)%tabs, 0
	case "j", "down":
		s.cursor = min(s.cursor+1, max(0, m.modelRowCount()-1))
	case "k", "up":
		s.cursor = max(0, s.cursor-1)
	case "r":
		s.note, s.isErr = "refreshing…", false
		return m.fetchModels(true)
	case "enter":
		return m.chooseModel()
	}
	return nil
}

// chooseModel acts on the row under the cursor: on the overview it opens
// that op's tab; on an op tab it sends the choice.
func (m *Model) chooseModel() tea.Cmd {
	s := m.models
	if !s.loaded {
		return nil
	}
	if s.tab == 0 {
		if s.cursor < len(s.view.Ops) {
			s.tab, s.cursor = s.cursor+1, 0
		}
		return nil
	}
	op := s.view.Ops[s.tab-1].Op
	opts := modelcmd.Options(s.view, op)
	if s.cursor >= len(opts) {
		return nil
	}
	o := opts[s.cursor]
	if !o.Choosable() {
		s.note, s.isErr = o.Ref+" is not available right now", true
		return nil
	}
	s.note, s.isErr = "choosing "+o.Ref+"…", false
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		v, err := be.SetModel(ctx, id, op, o.Ref)
		return modelsMsg{view: v, op: op, err: err}
	}
}

// modelsView draws the modal: the tab bar, the tab's rows, provider
// errors, and the last confirmation or error.
func (m *Model) modelsView() string {
	s := m.models
	cw := max(30, min(90, m.width-10))
	names := []string{"overview"}
	for _, o := range s.view.Ops {
		names = append(names, o.Op)
	}
	var tabs []string
	for i, n := range names {
		if i == s.tab {
			tabs = append(tabs, styKey.Render("["+n+"]"))
		} else {
			tabs = append(tabs, styMuted.Render(" "+n+" "))
		}
	}
	lines := []string{styApprovalTitle.Render("Models") + styMuted.Render("  -> selected  * default"), strings.Join(tabs, "")}
	lines = append(lines, "")

	switch {
	case !s.loaded:
		lines = append(lines, styMuted.Render("loading…"))
	case s.tab == 0:
		for i, l := range strings.Split(strings.TrimRight(modelcmd.Overview(s.view), "\n"), "\n") {
			lines = append(lines, modelRow(clip(l, cw-2), i == s.cursor, true))
		}
	default:
		opts := modelcmd.Options(s.view, s.view.Ops[s.tab-1].Op)
		lo := max(0, min(s.cursor-modelRows/2, len(opts)-modelRows))
		hi := min(len(opts), lo+modelRows)
		for i := lo; i < hi; i++ {
			o := opts[i]
			mark := "  "
			if o.Selected {
				mark = "->"
			}
			text := mark + " " + o.Ref
			if o.Default {
				text += " *"
			}
			if !o.Available {
				text += " (unavailable)"
			}
			lines = append(lines, modelRow(clip(text, cw-2), i == s.cursor, o.Choosable()))
		}
	}
	for _, g := range s.view.Groups {
		if g.Error != "" {
			lines = append(lines, styMuted.Render(clip("! "+g.Provider+": "+g.Error, cw)))
		}
	}
	if s.note != "" {
		sty := styAccent
		if s.isErr {
			sty = styWarn
		}
		lines = append(lines, "", sty.Render(clip(s.note, cw)))
	}
	lines = append(lines, "", strings.Join(modelsParts(), "  "))
	w := 0
	for _, l := range lines {
		w = max(w, lipgloss.Width(l))
	}
	return styModal.Width(min(w, cw) + 4).Render(strings.Join(lines, "\n"))
}

// modelRow marks the cursor row and dims a row that cannot be chosen.
func modelRow(text string, cursor, choosable bool) string {
	switch {
	case cursor:
		return styAccent.Render("› " + text)
	case !choosable:
		return styMuted.Render("  " + text)
	}
	return "  " + text
}
