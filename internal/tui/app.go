package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

type mode int

const (
	modeNormal mode = iota
	modeInsert
	modeCommand
	modeFilter
	modeConfirm
)

func (m mode) String() string {
	return [...]string{"NORMAL", "INSERT", "COMMAND", "FILTER", "CONFIRM"}[m]
}

// pane is which half of the chat screen NORMAL-mode keys drive.
type pane int

const (
	paneChat pane = iota
	paneSidebar
)

const (
	sidebarWidth   = 30
	sidebarMinTerm = 90
	minWidth       = 60
	minHeight      = 15
	inputMaxHeight = 8
	// refreshEvery is how often an open view refetches.
	refreshEvery = 2 * time.Second
)

// Messages. Everything that changes the model arrives as one of these, so
// the model is single-threaded and testable by calling Update.
type (
	eventMsg      struct{ ev daemon.WireEvent }
	connectedMsg  struct{}
	streamLostMsg struct{ err error }
	sessionsMsg   struct {
		list []daemon.SessionJSON
		err  error
	}
	transcriptMsg struct {
		id  string
		t   daemon.TranscriptJSON
		err error
	}
	noticeMsg struct {
		session, text string
		isErr         bool
	}
	sendFailedMsg struct {
		session, text string
		err           error
	}
	approvalFailedMsg struct {
		session, root string
		ev            daemon.WireEvent
		err           error
	}
	newSessionMsg struct {
		id  string
		err error
	}
	viewRowsMsg struct {
		name, session string
		rows          []Row
		err           error
	}
	viewTickMsg   struct{ gen int }
	actionDoneMsg struct{ err error }
	deletedMsg    struct {
		res daemon.DeleteSessionsJSON
		err error
	}
)

// Options configure a Model.
type Options struct {
	ShowCost bool
	// Now is the clock; nil means time.Now. Tests pin it.
	Now func() time.Time
}

// commandNames are the `:` commands, for completion.
var commandNames = []string{"agents", "chat", "clear", "compact", "context", "delete", "jobs", "new", "q", "quit", "sessions", "skills", "usage"}

// confirmState is the modal on screen. yes and alt run on the model's
// goroutine when the key is pressed, so they may change the model before
// returning the command that does the work.
type confirmState struct {
	question string
	detail   []string
	// yesLabel names what y does; empty means "confirm".
	yesLabel string
	yes      func() tea.Cmd
	// alt, when set, is what D does: the delete prompt's "and on Discord".
	alt      func() tea.Cmd
	altLabel string
}

// do wraps a command that needs nothing from the model as a confirm action.
func do(cmd tea.Cmd) func() tea.Cmd { return func() tea.Cmd { return cmd } }

// Model is the Bubble Tea model behind `spore chat`.
type Model struct {
	ctx  context.Context
	be   Backend
	opts Options

	cache    *cache
	selected string
	mode     mode
	// focus is the pane NORMAL-mode keys drive; see focused.
	focus pane

	input textarea.Model
	line  textinput.Model
	vp    viewport.Model

	width, height int
	// sidebarPref overrides the automatic width rule once the user has
	// toggled the sidebar.
	sidebarPref *bool
	showAll     bool
	filter      string

	toolCursor   int
	toolLines    []int
	scrollToTool bool
	// follow pins the viewport to the bottom; unseen marks output that
	// arrived while the user was scrolled up.
	follow      bool
	unseen      bool
	lastContent string

	help         bool
	confirm      *confirmState
	reconnecting bool
	// views serves the resource views; nil when the backend cannot.
	views Views
	// table is the open view; nil means the chat screen. back holds the
	// views it was opened over, innermost last: esc returns to them.
	table *table
	back  []*table
	// sideCursor is the sidebar row under the cursor when it is not a
	// session: the jobs folder or a job's label (see row.key). Empty while
	// the cursor is on the selected session.
	sideCursor string
	// viewGen increments whenever a view opens or closes, so a tick from an
	// earlier view is ignored.
	viewGen  int
	viewErr  string
	viewTick func(gen int) tea.Cmd
	// flash is a one-line report in the status bar, such as what a delete
	// did; the next key clears it.
	flash string

	history []string
	histIdx int
	draft   string
	queued  map[string][]string

	md      *glamour.TermRenderer
	mdWidth int
}

// New builds the model for sessionID. It starts in INSERT: `spore chat` is
// opened to type.
func New(ctx context.Context, be Backend, sessionID string, opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	in := textarea.New()
	in.Placeholder = "Ask spore something…"
	in.Prompt = ""
	in.ShowLineNumbers = false
	in.CharLimit = 0
	in.SetHeight(1)
	in.MaxHeight = inputMaxHeight
	in.FocusedStyle.CursorLine = lipgloss.NewStyle()
	in.FocusedStyle.Base = lipgloss.NewStyle()
	in.BlurredStyle.Base = lipgloss.NewStyle()
	// Enter sends; a newline is deliberate and needs its own key.
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("ctrl+j", "alt+enter"))

	line := textinput.New()
	line.Prompt = ""

	c := newCache(opts.Now)
	c.showCost = opts.ShowCost
	m := &Model{
		ctx: ctx, be: be, opts: opts, cache: c, selected: sessionID,
		input: in, line: line, vp: viewport.New(80, 20),
		width: 80, height: 24, toolCursor: -1, follow: true,
		queued: map[string][]string{},
	}
	if sessionID != "" {
		c.get(sessionID)
	}
	if v, ok := be.(Views); ok {
		m.views = v
	}
	m.viewTick = func(gen int) tea.Cmd {
		return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return viewTickMsg{gen: gen} })
	}
	m.mode = modeInsert
	m.input.Focus()
	return m
}

func (m *Model) Init() tea.Cmd { return textarea.Blink }

// Update applies one message, then re-lays-out the screen. Blocks cache
// their own rendering, so the re-layout is a join, not a re-render.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.sync()
	return m, cmd
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return nil

	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			m.vp.ScrollUp(3)
			m.follow = false
		case tea.MouseButtonWheelDown:
			m.vp.ScrollDown(3)
			m.follow = m.vp.AtBottom()
		}
		return nil

	case connectedMsg:
		m.reconnecting = false
		// The feed replays every live approval on connect.
		m.cache.ClearApprovals()
		cmds := []tea.Cmd{m.loadSessions()}
		if m.selected != "" {
			cmds = append(cmds, m.loadTranscript(m.selected))
		}
		return tea.Batch(cmds...)

	case streamLostMsg:
		m.reconnecting = true
		return nil

	case eventMsg:
		if msg.ev.Session == "" {
			return nil
		}
		if msg.ev.Type == daemon.WireSessionDeleted {
			// Before Apply, which would bring the session back to life.
			return m.removeSessions([]string{msg.ev.Session})
		}
		id := m.cache.Apply(msg.ev)
		switch msg.ev.Type {
		case daemon.WireTurnDone, daemon.WireStopped, daemon.WireError:
			var seen tea.Cmd
			if id == m.selected {
				seen = m.markSeen(id)
			}
			return tea.Batch(seen, m.drainQueue(id))
		}
		return nil

	case deletedMsg:
		if msg.err != nil {
			m.flash = "delete failed: " + msg.err.Error()
			return nil
		}
		noun := "sessions"
		if len(msg.res.Deleted) == 1 {
			noun = "session"
		}
		m.flash = strings.Join(append([]string{fmt.Sprintf("deleted %d %s", len(msg.res.Deleted), noun)}, msg.res.Notes...), " · ")
		return m.removeSessions(msg.res.Deleted)

	case sessionsMsg:
		if msg.err != nil {
			m.errorf("list sessions: %v", msg.err)
			return nil
		}
		m.cache.SetSessions(msg.list)
		if m.cache.get(m.selected).info.Source == "job" {
			// Started on a run: its folder must be open to show it. Closing
			// the folder moves the selection off runs, so this never
			// reopens one the user closed.
			m.cache.jobsOpen = true
		}
		if m.selected == "" {
			if ids := m.selectable(); len(ids) > 0 {
				return m.selectSession(ids[0])
			}
		}
		return nil

	case transcriptMsg:
		if msg.err != nil {
			m.cache.get(msg.id).add(kindError, "load transcript: "+msg.err.Error())
			return nil
		}
		m.cache.SetTranscript(msg.t)
		return nil

	case noticeMsg:
		kind := kindNotice
		if msg.isErr {
			kind = kindError
		}
		m.cache.get(msg.session).add(kind, msg.text)
		return nil

	case sendFailedMsg:
		sv := m.cache.get(msg.session)
		if strings.Contains(msg.err.Error(), "already has a turn running") {
			// A turn started elsewhere (Discord, a job) got there first.
			// Send this one when that turn ends.
			sv.dropLastUser(msg.text)
			sv.working = true
			m.queued[msg.session] = append([]string{msg.text}, m.queued[msg.session]...)
			sv.add(kindNotice, "queued: "+firstLine(msg.text))
			return nil
		}
		sv.working = false
		sv.add(kindError, "send failed: "+msg.err.Error())
		return nil

	case approvalFailedMsg:
		m.cache.restoreApproval(msg.root, msg.ev)
		m.flash = "could not answer the approval: " + msg.err.Error()
		return nil

	case newSessionMsg:
		if msg.err != nil {
			m.errorf("new session: %v", msg.err)
			return nil
		}
		m.cache.get(msg.id).loaded = true
		cmd := m.selectSession(msg.id)
		return tea.Batch(cmd, m.enterInsert(), m.loadSessions())

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

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	if m.mode == modeInsert {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		return cmd
	}
	return nil
}

func (m *Model) errorf(format string, args ...any) {
	m.cache.get(m.selected).add(kindError, fmt.Sprintf(format, args...))
}

func (m *Model) loadSessions() tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		list, err := be.Sessions(ctx)
		return sessionsMsg{list: list, err: err}
	}
}

func (m *Model) loadTranscript(id string) tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		t, err := be.Transcript(ctx, id)
		return transcriptMsg{id: id, t: t, err: err}
	}
}

func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	m.flash = ""
	s := k.String()
	if s == "ctrl+c" {
		if m.mode == modeInsert && m.input.Value() != "" {
			m.input.Reset()
			m.resizeInput()
			return nil
		}
		return tea.Quit
	}
	if m.tooSmall() {
		if s == "q" {
			return tea.Quit
		}
		return nil
	}
	if m.help {
		m.help = false
		return nil
	}
	// A multiplexer that holds a lone Esc to see whether a sequence follows
	// (tmux's escape-time) delivers Esc and the next key as one read, which
	// arrives as alt+<key>. Read it as vim does: Esc, then the key. alt+enter
	// is not a rune key, so it stays the INSERT newline.
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
}

func (m *Model) keyNormal(k tea.KeyMsg) tea.Cmd {
	// An approval on screen takes y/n/s/p first. It is only live here, in
	// NORMAL: a y typed into the input must never be read as consent.
	if root, ev, ok := m.cache.approvalFor(m.selected); ok {
		if m.answer(root, ev, k.String()) {
			return nil
		}
	}
	if m.focused() == paneSidebar {
		switch k.String() {
		case "j", "down":
			return m.moveSelection(1)
		case "k", "up":
			return m.moveSelection(-1)
		case "enter":
			if m.sideCursor != "" {
				return m.enterRow()
			}
			return m.enterInsert()
		}
	}
	switch k.String() {
	case "tab", "shift+tab":
		if m.sidebarOn() {
			m.focus = 1 - m.focused()
		}
	case "j", "down":
		m.vp.ScrollDown(1)
		m.follow = m.vp.AtBottom()
	case "k", "up":
		m.vp.ScrollUp(1)
		m.follow = false
	case "enter":
		return m.enterInsert()
	case "i", "a":
		return m.enterInsert()
	case "ctrl+d":
		m.vp.HalfPageDown()
		m.follow = m.vp.AtBottom()
	case "ctrl+u":
		m.vp.HalfPageUp()
		m.follow = false
	case "g":
		m.vp.GotoTop()
		m.follow = false
	case "G":
		m.follow = true
	case "]":
		m.moveToolCursor(1)
	case "[":
		m.moveToolCursor(-1)
	case "o":
		m.toggleTool(false)
	case "O":
		m.toggleTool(true)
	case "/":
		m.mode = modeFilter
		m.line.SetValue(m.filter)
		m.line.CursorEnd()
		return m.line.Focus()
	case ":":
		m.mode = modeCommand
		m.line.SetValue("")
		return m.line.Focus()
	case "n":
		return m.newSession(m.cache.get(m.selected).info.Workspace)
	case "b":
		return m.nextBlocked()
	case "x":
		return m.confirmCancelAgent()
	case "d":
		return m.confirmDelete(false)
	case "z":
		return m.toggleJobs()
	case "esc":
		if m.cache.get(m.selected).info.ParentID != "" {
			return m.confirmCancelAgent()
		}
		return m.stop()
	case "?":
		m.help = true
	case "q":
		return tea.Quit
	case "ctrl+b":
		on := !m.sidebarOn()
		m.sidebarPref = &on
		if !on {
			m.focus = paneChat
		}
	default:
		if r, ok := resourceByHotkey(k.String()); ok {
			return m.openView(r)
		}
	}
	return nil
}

func (m *Model) enterInsert() tea.Cmd {
	m.mode = modeInsert
	m.focus = paneChat
	return m.input.Focus()
}

// focused is the pane NORMAL-mode keys drive. Without a sidebar on screen
// there is only the chat.
func (m *Model) focused() pane {
	if !m.sidebarOn() || m.table != nil {
		return paneChat
	}
	return m.focus
}

func (m *Model) keyInsert(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		m.input.Blur()
		return nil
	case "enter":
		text := strings.TrimSpace(m.input.Value())
		if text == "" {
			return nil
		}
		m.input.Reset()
		m.resizeInput()
		m.history = append(m.history, text)
		m.histIdx = len(m.history)
		m.draft = ""
		if strings.HasPrefix(text, "/") {
			return m.slashLine(strings.TrimPrefix(text, "/"))
		}
		return m.submit(m.selected, text)
	case "up":
		if m.input.Line() == 0 && len(m.history) > 0 {
			if m.histIdx == len(m.history) {
				m.draft = m.input.Value()
			}
			if m.histIdx > 0 {
				m.histIdx--
				m.setInput(m.history[m.histIdx])
			}
			return nil
		}
	case "down":
		if m.histIdx < len(m.history) && m.input.Line() == m.input.LineCount()-1 {
			m.histIdx++
			if m.histIdx == len(m.history) {
				m.setInput(m.draft)
			} else {
				m.setInput(m.history[m.histIdx])
			}
			return nil
		}
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	m.resizeInput()
	return cmd
}

func (m *Model) setInput(s string) {
	m.input.SetValue(s)
	m.resizeInput()
	m.input.CursorEnd()
}

// resizeInput grows the prompt box with its text, up to the cap.
func (m *Model) resizeInput() {
	m.input.SetHeight(max(1, min(inputMaxHeight, m.input.LineCount())))
}

func (m *Model) keyCommand(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.mode = modeNormal
		m.line.Blur()
		return nil
	case "enter":
		v := m.line.Value()
		m.mode = modeNormal
		m.line.Blur()
		return m.command(v)
	case "tab":
		m.line.SetValue(complete(m.line.Value()))
		m.line.CursorEnd()
		return nil
	}
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	return cmd
}

// complete finishes a command name when exactly one matches.
func complete(v string) string {
	if strings.Contains(v, " ") {
		return v
	}
	var hits []string
	for _, c := range commandNames {
		if strings.HasPrefix(c, v) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0] + " "
	}
	return v
}

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

func (m *Model) keyConfirm(k tea.KeyMsg) tea.Cmd {
	c := m.confirm
	m.confirm = nil
	m.mode = modeNormal
	switch {
	case c != nil && k.String() == "y":
		return c.yes()
	case c != nil && k.String() == "D" && c.alt != nil:
		return c.alt()
	}
	return nil
}

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
		m.leaveViews()
		return nil
	case "delete":
		return m.confirmDelete(len(args) > 0 && args[0] == "all")
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

func (m *Model) slash(name string) tea.Cmd {
	be, ctx, id := m.be, m.ctx, m.selected
	m.cache.get(id).add(kindNotice, name+"…")
	return func() tea.Msg {
		out, err := be.Slash(ctx, id, name)
		if err != nil {
			return noticeMsg{session: id, text: name + " failed: " + err.Error(), isErr: true}
		}
		return noticeMsg{session: id, text: out}
	}
}

// submit sends a message, or queues it while the session is busy.
func (m *Model) submit(id, text string) tea.Cmd {
	if id == "" {
		return nil
	}
	sv := m.cache.get(id)
	if m.cache.State(id) != daemon.SessionIdle {
		m.queued[id] = append(m.queued[id], text)
		sv.add(kindNotice, "queued: "+firstLine(text))
		return nil
	}
	sv.addUser(text)
	sv.working = true
	m.follow = true
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		if err := be.Send(ctx, id, text); err != nil {
			return sendFailedMsg{session: id, text: text, err: err}
		}
		return nil
	}
}

// drainQueue sends the next message typed while id was busy.
func (m *Model) drainQueue(id string) tea.Cmd {
	q := m.queued[id]
	if len(q) == 0 || m.cache.State(id) != daemon.SessionIdle {
		return nil
	}
	m.queued[id] = q[1:]
	return m.submit(id, q[0])
}

// answer opens the modal that confirms an approval key, reporting whether k
// was one.
func (m *Model) answer(root string, ev daemon.WireEvent, k string) bool {
	var ans policy.Answer
	switch k {
	case "y":
		ans = policy.Answer{Allow: true, Scope: policy.ScopeOnce}
	case "n":
		ans = policy.Answer{Allow: false, Scope: policy.ScopeOnce}
	case "s":
		ans = policy.Answer{Allow: true, Scope: policy.ScopeSession}
	case "p":
		if ev.Pattern == "" {
			return true // the option was never offered
		}
		ans = policy.Answer{Allow: true, Scope: policy.ScopePattern}
	default:
		return false
	}
	detail := []string{`matched policy rule "` + ev.Rule + `"`}
	if ev.Origin != "" {
		detail = append(detail, "asked by sub-agent "+short(ev.Origin))
	}
	m.confirm = &confirmState{
		question: answerQuestion(ev, ans),
		detail:   detail,
		yes:      func() tea.Cmd { return m.resolve(root, ev, ans) },
	}
	m.mode = modeConfirm
	return true
}

// resolve sends a confirmed answer. The approval may have been answered
// elsewhere while the modal was open; then there is nothing left to send.
func (m *Model) resolve(root string, ev daemon.WireEvent, ans policy.Answer) tea.Cmd {
	if !m.cache.pending(root, ev.PendingID) {
		m.flash = "that approval was already answered"
		return nil
	}
	m.cache.markAnswered(root, ev.PendingID)
	m.flash = answerText(ev, ans)
	sel, be, ctx := m.selected, m.be, m.ctx
	return func() tea.Msg {
		if err := be.Resolve(ctx, root, ev.PendingID, ans); err != nil {
			return approvalFailedMsg{session: sel, root: root, ev: ev, err: err}
		}
		return nil
	}
}

func (m *Model) stop() tea.Cmd {
	id := m.selected
	if id == "" || m.cache.State(id) == daemon.SessionIdle {
		return nil
	}
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		if err := be.Stop(ctx, id); err != nil {
			return noticeMsg{session: id, text: err.Error(), isErr: true}
		}
		return nil
	}
}

func (m *Model) confirmCancelAgent() tea.Cmd {
	child := m.selected
	parent := m.cache.get(child).info.ParentID
	if parent == "" {
		return nil
	}
	be, ctx := m.be, m.ctx
	m.confirm = &confirmState{
		question: fmt.Sprintf("Stop sub-agent %s?", short(child)),
		yesLabel: "stop",
		yes: do(func() tea.Msg {
			if err := be.CancelAgent(ctx, parent, child); err != nil {
				return noticeMsg{session: child, text: err.Error(), isErr: true}
			}
			return nil
		}),
	}
	m.mode = modeConfirm
	return nil
}

func (m *Model) newSession(workspace string) tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		id, err := be.NewSession(ctx, workspace)
		return newSessionMsg{id: id, err: err}
	}
}

// openView replaces the body with a resource's table. A scoped view is for
// the selected session.
func (m *Model) openView(r Resource) tea.Cmd {
	if m.views == nil {
		m.errorf("views need the daemon")
		return nil
	}
	m.back = nil
	return m.showView(newTable(r, m.selected))
}

// pushView opens r over the open view; esc returns to it.
func (m *Model) pushView(r Resource) tea.Cmd {
	if m.table != nil {
		m.back = append(m.back, m.table)
	}
	return m.showView(newTable(r, m.selected))
}

func (m *Model) showView(t *table) tea.Cmd {
	m.mode = modeNormal
	m.input.Blur()
	m.table = t
	m.viewErr = ""
	m.viewGen++
	return tea.Batch(m.fetchView(), m.viewTick(m.viewGen))
}

// closeView returns to the view this one was opened over, or to the chat.
func (m *Model) closeView() tea.Cmd {
	if n := len(m.back); n > 0 {
		t := m.back[n-1]
		m.back = m.back[:n-1]
		return m.showView(t)
	}
	m.table = nil
	m.viewErr = ""
	m.viewGen++
	return nil
}

// leaveViews closes every view and returns to the chat.
func (m *Model) leaveViews() {
	m.back = nil
	m.closeView()
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
		return m.closeView()
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
		if d, ok := t.res.(driller); ok && s == "enter" {
			if next, ok := d.Drill(r); ok {
				return m.pushView(next)
			}
		}
		if o, isOpener := t.res.(opener); isOpener && s == "enter" {
			if id := o.Open(r); id != "" {
				m.leaveViews()
				return m.selectSession(id)
			}
		}
		t.openDetail(m.width-2, m.paneHeight())
	case "o":
		r, ok := t.selected()
		if !ok {
			return nil
		}
		if o, isOpener := t.res.(sessionOpener); isOpener {
			m.leaveViews()
			return m.selectSession(o.OpenSession(r))
		}
		return m.runAction(s)
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
		m.confirm = &confirmState{question: capitalize(a.Confirm(r)), yes: do(run)}
		m.mode = modeConfirm
		return nil
	}
	return nil
}

func (m *Model) selectable() []string {
	var ids []string
	for _, r := range m.cache.rows(m.showAll, m.filter, m.selected) {
		if r.id != "" {
			ids = append(ids, r.id)
		}
	}
	return ids
}

func indexOf(ids []string, id string) int {
	for i, v := range ids {
		if v == id {
			return i
		}
	}
	return -1
}

// navigable is every sidebar row the cursor can rest on, top to bottom:
// sessions, and the jobs folder and each job's label.
func (m *Model) navigable() []string {
	var keys []string
	for _, r := range m.cache.rows(m.showAll, m.filter, m.selected) {
		switch {
		case r.key != "":
			keys = append(keys, r.key)
		case r.id != "":
			keys = append(keys, r.id)
		}
	}
	return keys
}

// moveSelection moves the sidebar cursor. Landing on a session selects it;
// landing on the jobs folder or a job's label leaves the chat pane on the
// session it shows, and enter then acts on that row.
func (m *Model) moveSelection(d int) tea.Cmd {
	keys := m.navigable()
	if len(keys) == 0 {
		return nil
	}
	at := m.selected
	if m.sideCursor != "" {
		at = m.sideCursor
	}
	j := indexOf(keys, at) + d
	if indexOf(keys, at) < 0 {
		j = 0
	}
	next := keys[max(0, min(len(keys)-1, j))]
	if isRowKey(next) {
		m.sideCursor = next
		return nil
	}
	m.sideCursor = ""
	return m.selectSession(next)
}

// toggleJobs opens or closes the jobs folder. Closing it hides every run in
// it, so a selected run gives way to the first chat and the cursor rests on
// the folder: nothing of the folder's contents stays on screen.
func (m *Model) toggleJobs() tea.Cmd {
	m.cache.jobsOpen = !m.cache.jobsOpen
	if m.cache.jobsOpen {
		return nil
	}
	inside := m.sideCursor != "" && m.sideCursor != folderKey
	if m.cache.get(m.selected).info.Source == "job" {
		inside = true
		for _, key := range m.navigable() {
			if !isRowKey(key) && m.cache.get(key).info.Source != "job" {
				m.sideCursor = ""
				cmd := m.selectSession(key)
				m.sideCursor = folderKey
				return cmd
			}
		}
	}
	if inside {
		m.sideCursor = folderKey
	}
	return nil
}

// enterRow acts on the sidebar row under the cursor when it is not a
// session: enter opens or closes the jobs folder, and opens a job's runs.
func (m *Model) enterRow() tea.Cmd {
	if m.sideCursor == folderKey {
		return m.toggleJobs()
	}
	if id, ok := jobOfKey(m.sideCursor); ok {
		return m.openView(jobRunsRes{job: id})
	}
	return nil
}

func (m *Model) selectSession(id string) tea.Cmd {
	if id == m.selected {
		return nil
	}
	m.selected, m.toolCursor, m.follow, m.unseen = id, -1, true, false
	if m.cache.get(id).info.Source == "job" {
		// A run lives in the jobs folder; showing one opens it.
		m.cache.jobsOpen = true
	}
	seen := m.markSeen(id)
	if !m.cache.get(id).loaded {
		return tea.Batch(seen, m.loadTranscript(id))
	}
	return seen
}

// markSeen clears a job run's unread mark here and on the daemon, so the
// jobs folder's badge stays right across restarts. Only job runs are
// counted, so only they are marked.
func (m *Model) markSeen(id string) tea.Cmd {
	sv := m.cache.get(id)
	if !sv.info.Unread || sv.info.Source != "job" {
		return nil
	}
	sv.info.Unread = false
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		if err := be.MarkSeen(ctx, id); err != nil {
			return noticeMsg{session: id, text: "could not mark the run seen: " + err.Error(), isErr: true}
		}
		return nil
	}
}

// confirmDelete asks before deleting the selected session (and its
// sub-agents), or every session. y deletes here; D deletes the Discord copy
// too.
func (m *Model) confirmDelete(all bool) tea.Cmd {
	id := m.selected
	var what string
	var ids []string
	switch {
	case all:
		n := 0
		for sid := range m.cache.sessions {
			if sid != "" {
				n++
			}
		}
		what = fmt.Sprintf("EVERY session (%d)", n)
	case id == "":
		return nil
	default:
		ids = []string{id}
		title := m.cache.get(id).info.Title
		if title == "" {
			title = short(id)
		}
		what = fmt.Sprintf("%q", oneLine(title))
		if n := m.cache.descendants(id); n > 0 {
			what += fmt.Sprintf(" and its %d sub-agent(s)", n)
		}
	}
	be, ctx := m.be, m.ctx
	del := func(discord bool) tea.Cmd {
		return func() tea.Msg {
			res, err := be.DeleteSessions(ctx, ids, all, discord)
			return deletedMsg{res: res, err: err}
		}
	}
	m.confirm = &confirmState{
		question: "Delete " + what + "?",
		yesLabel: "delete",
		yes:      do(del(false)),
		alt:      do(del(true)),
		altLabel: "also on Discord",
	}
	m.mode = modeConfirm
	m.input.Blur()
	return nil
}

// removeSessions drops deleted sessions, moving the selection to the next
// session still listed when the selected one went.
func (m *Model) removeSessions(ids []string) tea.Cmd {
	gone := map[string]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	next := ""
	if gone[m.selected] {
		list := m.selectable()
		at := indexOf(list, m.selected)
		for d := 1; d < len(list) && next == ""; d++ {
			for _, j := range []int{at + d, at - d} {
				if j >= 0 && j < len(list) && !gone[list[j]] {
					next = list[j]
					break
				}
			}
		}
	}
	for id := range gone {
		m.cache.remove(id)
		delete(m.queued, id)
	}
	if !gone[m.selected] {
		return nil
	}
	m.selected = ""
	if next == "" {
		return nil
	}
	return m.selectSession(next)
}

func (m *Model) nextBlocked() tea.Cmd {
	ids := m.selectable()
	start := indexOf(ids, m.selected)
	for k := 1; k <= len(ids); k++ {
		id := ids[(start+k+len(ids))%len(ids)]
		if m.cache.State(id) == daemon.SessionBlocked {
			return m.selectSession(id)
		}
	}
	return nil
}

func (m *Model) moveToolCursor(d int) {
	tools := m.cache.get(m.selected).tools()
	if len(tools) == 0 {
		return
	}
	if m.toolCursor < 0 {
		m.toolCursor = len(tools) - 1
	} else {
		m.toolCursor = max(0, min(len(tools)-1, m.toolCursor+d))
	}
	m.scrollToTool = true
}

func (m *Model) toggleTool(all bool) {
	tools := m.cache.get(m.selected).tools()
	if len(tools) == 0 {
		return
	}
	if all {
		expand := false
		for _, t := range tools {
			if !t.expanded {
				expand = true
			}
		}
		for _, t := range tools {
			t.expanded = expand
		}
		return
	}
	if m.toolCursor < 0 || m.toolCursor >= len(tools) {
		m.toolCursor = len(tools) - 1
	}
	tools[m.toolCursor].expanded = !tools[m.toolCursor].expanded
	m.scrollToTool = true
}

// ExitSummary is what `spore chat` prints to ordinary scrollback after the
// alternate screen closes: the last exchange, and how to come back.
func (m *Model) ExitSummary() string {
	sv := m.cache.get(m.selected)
	var user string
	var reply []string
	for _, b := range sv.blocks {
		switch b.kind {
		case kindUser:
			user, reply = b.text, nil
		case kindText:
			reply = append(reply, b.text)
		}
	}
	var out []string
	if user != "" {
		out = append(out, "› "+user)
	}
	if len(reply) > 0 {
		out = append(out, strings.Join(reply, "\n"))
	}
	if m.selected != "" {
		out = append(out, "resume: spore chat "+m.selected)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n\n") + "\n"
}

// answerQuestion is the modal's question for an approval key.
func answerQuestion(ev daemon.WireEvent, ans policy.Answer) string {
	switch {
	case !ans.Allow:
		return "Deny " + ev.Tool + "?"
	case ans.Scope == policy.ScopeSession:
		return "Allow " + ev.Tool + " for this session?"
	case ans.Scope == policy.ScopePattern:
		return "Always allow " + ev.Pattern + "?"
	}
	return "Allow " + ev.Tool + " once?"
}

// capitalize upper-cases a sentence's first letter.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func answerText(ev daemon.WireEvent, ans policy.Answer) string {
	verb := "denied"
	if ans.Allow {
		verb = "allowed"
	}
	scope := ""
	switch ans.Scope {
	case policy.ScopeSession:
		scope = " for this session"
	case policy.ScopePattern:
		scope = " from now on"
	}
	return verb + " " + ev.Tool + scope
}
