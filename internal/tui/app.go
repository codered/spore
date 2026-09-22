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

const (
	sidebarWidth   = 30
	sidebarMinTerm = 90
	minWidth       = 60
	minHeight      = 15
	inputMaxHeight = 8
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
)

// Options configure a Model.
type Options struct {
	ShowCost bool
	// Now is the clock; nil means time.Now. Tests pin it.
	Now func() time.Time
}

// commandNames are the `:` commands, for completion.
var commandNames = []string{"agents", "clear", "compact", "context", "new", "q", "quit", "sessions", "skills", "usage"}

type confirmState struct {
	prompt string
	yes    tea.Cmd
}

// Model is the Bubble Tea model behind `spore chat`.
type Model struct {
	ctx  context.Context
	be   Backend
	opts Options

	cache    *cache
	selected string
	mode     mode

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
		id := m.cache.Apply(msg.ev)
		switch msg.ev.Type {
		case daemon.WireTurnDone, daemon.WireStopped, daemon.WireError:
			return m.drainQueue(id)
		}
		return nil

	case sessionsMsg:
		if msg.err != nil {
			m.errorf("list sessions: %v", msg.err)
			return nil
		}
		m.cache.SetSessions(msg.list)
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
		m.cache.get(msg.session).add(kindError, "could not answer the approval: "+msg.err.Error())
		return nil

	case newSessionMsg:
		if msg.err != nil {
			m.errorf("new session: %v", msg.err)
			return nil
		}
		m.cache.get(msg.id).loaded = true
		cmd := m.selectSession(msg.id)
		return tea.Batch(cmd, m.enterInsert(), m.loadSessions())

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
	return m.keyNormal(k)
}

func (m *Model) keyNormal(k tea.KeyMsg) tea.Cmd {
	// An approval on screen takes y/n/s/p first. It is only live here, in
	// NORMAL: a y typed into the input must never be read as consent.
	if root, ev, ok := m.cache.approvalFor(m.selected); ok {
		if cmd, handled := m.answer(root, ev, k.String()); handled {
			return cmd
		}
	}
	switch k.String() {
	case "j", "down":
		return m.moveSelection(1)
	case "k", "up":
		return m.moveSelection(-1)
	case "i", "a", "enter":
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
	}
	return nil
}

func (m *Model) enterInsert() tea.Cmd {
	m.mode = modeInsert
	return m.input.Focus()
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
			return m.command(strings.TrimPrefix(text, "/"))
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
	switch k.String() {
	case "esc":
		m.filter = ""
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
	m.filter = m.line.Value()
	return cmd
}

func (m *Model) keyConfirm(k tea.KeyMsg) tea.Cmd {
	c := m.confirm
	m.confirm = nil
	m.mode = modeNormal
	if c != nil && k.String() == "y" {
		return c.yes
	}
	return nil
}

// command runs a `:` command, or a slash command typed into the input.
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
	case "clear", "compact", "context", "usage", "skills", "agents":
		return m.slash(name)
	}
	m.errorf("unknown command: %s", name)
	return nil
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

func (m *Model) answer(root string, ev daemon.WireEvent, k string) (tea.Cmd, bool) {
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
			return nil, true // the option was never offered
		}
		ans = policy.Answer{Allow: true, Scope: policy.ScopePattern}
	default:
		return nil, false
	}
	m.cache.markAnswered(root, ev.PendingID)
	sel := m.selected
	m.cache.get(sel).add(kindNotice, answerText(ev, ans))
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		if err := be.Resolve(ctx, root, ev.PendingID, ans); err != nil {
			return approvalFailedMsg{session: sel, root: root, ev: ev, err: err}
		}
		return nil
	}, true
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
		prompt: fmt.Sprintf("stop sub-agent %s? y/n", short(child)),
		yes: func() tea.Msg {
			if err := be.CancelAgent(ctx, parent, child); err != nil {
				return noticeMsg{session: child, text: err.Error(), isErr: true}
			}
			return nil
		},
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

func (m *Model) moveSelection(d int) tea.Cmd {
	ids := m.selectable()
	if len(ids) == 0 {
		return nil
	}
	j := indexOf(ids, m.selected) + d
	if indexOf(ids, m.selected) < 0 {
		j = 0
	}
	return m.selectSession(ids[max(0, min(len(ids)-1, j))])
}

func (m *Model) selectSession(id string) tea.Cmd {
	if id == m.selected {
		return nil
	}
	m.selected, m.toolCursor, m.follow, m.unseen = id, -1, true, false
	if !m.cache.get(id).loaded {
		return m.loadTranscript(id)
	}
	return nil
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
