package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// Messages the stream goroutine and the network commands post into the
// program. Everything that touches model state arrives as one of these, so
// the model itself is single-threaded and testable by calling Update.
type (
	streamMsg    struct{ ev daemon.WireEvent }
	streamEndMsg struct{ err error }
	sendErrMsg   struct{ err error }
	resolveErr   struct{ err error }
)

type chatState int

const (
	stateIdle chatState = iota
	stateBusy
	stateApproving
)

const (
	// inputMaxHeight bounds the prompt box. Beyond this the textarea scrolls
	// rather than pushing the transcript off the screen.
	inputMaxHeight = 8
	// liveReserve is the number of rows the view keeps for the prompt, the
	// hint line and the box borders when it sizes the live region.
	liveReserve = 12
)

// chatUI is the Bubble Tea model behind `spore chat`. It owns no network
// code: sending a message and answering an approval are injected, so the
// model can be driven end to end in a test with no daemon.
type chatUI struct {
	sessionID string
	webURL    string
	showCost  bool

	send    func(text string) error
	resolve func(pendingID int64, ans policy.Answer) error
	// emit writes a finished block to the terminal scrollback above the live
	// view. Production passes tea.Println; tests capture instead.
	emit func(string) tea.Cmd

	ta   textarea.Model
	spin spinner.Model
	md   *glamour.TermRenderer

	width  int
	height int

	state chatState
	// live is the assistant text of the current run, held until a segment
	// boundary (a tool call, or the end of the turn) flushes it to
	// scrollback. Holding it is what keeps the transcript in order.
	live      strings.Builder
	turnStart time.Time

	// pending is the approval being answered; more wait in approvalQueue.
	pending       *daemon.WireEvent
	approvalQueue []daemon.WireEvent

	// queued holds messages typed while a turn was still running. They are
	// sent one at a time as the session becomes free.
	queued []string

	history []string
	// histIdx indexes history while browsing it; len(history) means "not
	// browsing", which is why draft is kept alongside.
	histIdx int
	draft   string

	// fatal is the error the program exits with, read by the caller once the
	// program has stopped.
	fatal error
	// quitting suppresses the final view so the prompt box does not linger.
	quitting bool
}

func newChatUI(sessionID, webURL string, showCost bool) *chatUI {
	ta := textarea.New()
	ta.Placeholder = "Ask spore something…"
	ta.Prompt = ""
	ta.ShowLineNumbers = false
	ta.CharLimit = 0
	ta.SetHeight(1)
	ta.MaxHeight = inputMaxHeight
	ta.FocusedStyle.CursorLine = lipgloss.NewStyle()
	ta.FocusedStyle.Base = lipgloss.NewStyle()
	ta.BlurredStyle.Base = lipgloss.NewStyle()
	// Enter sends the message; a newline is deliberate and needs its own key.
	ta.KeyMap.InsertNewline = key.NewBinding(
		key.WithKeys("ctrl+j", "alt+enter"),
		key.WithHelp("ctrl+j", "newline"),
	)
	ta.Focus()

	sp := spinner.New(spinner.WithSpinner(spinner.Dot), spinner.WithStyle(styAccent))

	return &chatUI{
		sessionID: sessionID,
		webURL:    webURL,
		showCost:  showCost,
		emit:      func(s string) tea.Cmd { return tea.Println(s) },
		ta:        ta,
		spin:      sp,
		width:     80,
		height:    24,
		md:        newMarkdown(80),
		state:     stateIdle,
	}
}

func (m *chatUI) Init() tea.Cmd {
	return tea.Batch(textarea.Blink, m.spin.Tick, m.emit(banner(m.sessionID, m.webURL)))
}

// flush writes finished lines to scrollback as one block, so their order is
// the order they happened in. Empty parts are dropped.
func (m *chatUI) flush(parts ...string) tea.Cmd {
	var kept []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return m.emit(strings.Join(kept, "\n"))
}

// flushLive drains the buffered assistant text into scrollback, rendered as
// markdown. It is called at every segment boundary.
func (m *chatUI) flushLive() tea.Cmd {
	text := m.live.String()
	m.live.Reset()
	return m.flush(renderMarkdown(m.md, text))
}

func (m *chatUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		// Two columns of border and two of padding are not available to text.
		m.ta.SetWidth(max(20, msg.Width-4))
		m.md = newMarkdown(max(20, msg.Width-2))
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd

	case streamMsg:
		return m, m.handleEvent(msg.ev)

	case streamEndMsg:
		m.fatal = msg.err
		m.quitting = true
		return m, tea.Quit

	case sendErrMsg:
		m.state = stateIdle
		return m, m.flush(styDanger.Render("  ✗ send failed: " + msg.err.Error()))

	case resolveErr:
		return m, m.flush(styDanger.Render("  ✗ could not answer the approval: " + msg.err.Error()))

	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}

	var cmd tea.Cmd
	m.ta, cmd = m.ta.Update(msg)
	return m, cmd
}

// handleEvent folds one wire event into the model. Text accumulates; every
// other event is a segment boundary that flushes the text buffered so far,
// which is what keeps prose and tool calls in the order they occurred.
func (m *chatUI) handleEvent(ev daemon.WireEvent) tea.Cmd {
	switch ev.Type {
	case daemon.WireText:
		if m.state == stateIdle {
			m.state = stateBusy
		}
		m.live.WriteString(ev.Text)
		return nil

	case daemon.WireToolCall:
		return tea.Sequence(m.flushLive(), m.flush(toolCallLine(ev.Tool, ev.Args, m.width)))

	case daemon.WireToolResult:
		return m.flush(toolResultLine(ev.Content, ev.IsError, ev.Truncated))

	case daemon.WireApproval:
		m.approvalQueue = append(m.approvalQueue, ev)
		return tea.Sequence(m.flushLive(), m.nextApproval())

	case daemon.WireResolved:
		// Somebody else (the web UI, Discord) answered. Drop it so this
		// terminal does not keep asking a question that is already settled.
		return m.dropApproval(ev.PendingID)

	case daemon.WireTurnDone:
		took := ""
		if !m.turnStart.IsZero() {
			took = fmt.Sprintf(" · %.1fs", time.Since(m.turnStart).Seconds())
		}
		cmd := tea.Sequence(
			m.flushLive(),
			m.flush(turnFooter(ev.Model, ev.TokensIn, ev.TokensOut, ev.CostUSD, m.showCost)+styMuted.Render(took)),
		)
		return tea.Sequence(cmd, m.finishTurn())

	case daemon.WireError:
		cmd := tea.Sequence(m.flushLive(), m.flush(styDanger.Render("  ✗ turn failed: "+ev.Error)))
		return tea.Sequence(cmd, m.finishTurn())
	}
	return nil
}

// finishTurn returns the session to idle and starts whatever the user typed
// while it was busy.
func (m *chatUI) finishTurn() tea.Cmd {
	if m.pending != nil {
		m.state = stateApproving
	} else {
		m.state = stateIdle
	}
	if m.state == stateIdle && len(m.queued) > 0 {
		next := m.queued[0]
		m.queued = m.queued[1:]
		return m.submit(next)
	}
	return nil
}

// nextApproval promotes the head of the queue to the visible prompt when no
// approval is on screen already.
func (m *chatUI) nextApproval() tea.Cmd {
	if m.pending != nil || len(m.approvalQueue) == 0 {
		return nil
	}
	ev := m.approvalQueue[0]
	m.approvalQueue = m.approvalQueue[1:]
	m.pending = &ev
	m.state = stateApproving
	return nil
}

// dropApproval removes an approval answered elsewhere, whether it is the one
// on screen or still queued.
func (m *chatUI) dropApproval(pendingID int64) tea.Cmd {
	if m.pending != nil && m.pending.PendingID == pendingID {
		m.pending = nil
		m.state = stateBusy
		return tea.Sequence(
			m.flush(styMuted.Render("  · approval answered elsewhere")),
			m.nextApproval(),
		)
	}
	for i, ev := range m.approvalQueue {
		if ev.PendingID == pendingID {
			m.approvalQueue = append(m.approvalQueue[:i], m.approvalQueue[i+1:]...)
			break
		}
	}
	return nil
}

func (m *chatUI) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "ctrl+c":
		m.quitting = true
		return tea.Quit
	case "ctrl+d":
		if m.ta.Value() == "" {
			m.quitting = true
			return tea.Quit
		}
	}

	if m.state == stateApproving {
		return m.handleApprovalKey(msg)
	}

	switch msg.String() {
	case "enter":
		text := strings.TrimSpace(m.ta.Value())
		if text == "" {
			return nil
		}
		m.ta.Reset()
		m.ta.SetHeight(1)
		m.history = append(m.history, text)
		m.histIdx = len(m.history)
		m.draft = ""
		if m.state != stateIdle {
			m.queued = append(m.queued, text)
			return m.flush(styMuted.Render("  · queued: " + firstLine(text)))
		}
		return m.submit(text)

	case "up":
		if m.ta.Line() == 0 && len(m.history) > 0 {
			if m.histIdx == len(m.history) {
				m.draft = m.ta.Value()
			}
			if m.histIdx > 0 {
				m.histIdx--
				m.setInput(m.history[m.histIdx])
			}
			return nil
		}

	case "down":
		if m.histIdx < len(m.history) && m.ta.Line() == m.ta.LineCount()-1 {
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
	m.ta, cmd = m.ta.Update(msg)
	m.resizeInput()
	return cmd
}

// handleApprovalKey answers the approval on screen. Only the offered keys do
// anything: a stray keystroke must never be read as consent.
func (m *chatUI) handleApprovalKey(msg tea.KeyMsg) tea.Cmd {
	ev := m.pending
	if ev == nil {
		return nil
	}
	var ans policy.Answer
	switch msg.String() {
	case "y":
		ans = policy.Answer{Allow: true, Scope: policy.ScopeOnce}
	case "n":
		ans = policy.Answer{Allow: false, Scope: policy.ScopeOnce}
	case "s":
		ans = policy.Answer{Allow: true, Scope: policy.ScopeSession}
	case "p":
		if ev.Pattern == "" {
			return nil // the option was never offered
		}
		ans = policy.Answer{Allow: true, Scope: policy.ScopePattern}
	default:
		return nil
	}

	answered := *ev
	m.pending = nil
	m.state = stateBusy
	resolve := m.resolve
	cmd := func() tea.Msg {
		if resolve == nil {
			return nil
		}
		if err := resolve(answered.PendingID, ans); err != nil {
			return resolveErr{err}
		}
		return nil
	}
	return tea.Sequence(m.flush(answerLine(answered.Tool, ans)), cmd, m.nextApproval())
}

// answerLine records an answered approval in the scrollback, so the decision
// is visible later next to the call it governed.
func answerLine(tool string, ans policy.Answer) string {
	verb := "denied"
	sty := styDanger
	if ans.Allow {
		verb, sty = "allowed", styAccent
	}
	scope := ""
	switch ans.Scope {
	case policy.ScopeSession:
		scope = " for this session"
	case policy.ScopePattern:
		scope = " from now on"
	}
	return sty.Render("  · " + verb + " " + tool + scope)
}

// submit posts a message. The network call runs in the command so a slow or
// stuck daemon cannot freeze the interface.
func (m *chatUI) submit(text string) tea.Cmd {
	m.state = stateBusy
	m.turnStart = time.Now()
	send := m.send
	return tea.Sequence(
		m.flush(styAccent.Render("› ")+text),
		func() tea.Msg {
			if send == nil {
				return nil
			}
			if err := send(text); err != nil {
				return sendErrMsg{err}
			}
			return nil
		},
	)
}

func (m *chatUI) setInput(s string) {
	m.ta.SetValue(s)
	m.resizeInput()
	m.ta.CursorEnd()
}

// resizeInput grows the prompt box with the text in it, up to the cap.
func (m *chatUI) resizeInput() {
	h := m.ta.LineCount()
	if h < 1 {
		h = 1
	}
	if h > inputMaxHeight {
		h = inputMaxHeight
	}
	m.ta.SetHeight(h)
}

func (m *chatUI) View() string {
	if m.quitting {
		return ""
	}
	var b strings.Builder

	if live := m.live.String(); live != "" {
		wrapped := lipgloss.NewStyle().Width(max(20, m.width-2)).Render(live)
		b.WriteString(tailLines(wrapped, max(3, m.height-liveReserve)))
		b.WriteString("\n")
	}

	if m.state == stateApproving && m.pending != nil {
		b.WriteString(m.approvalView())
		return b.String()
	}

	if m.state == stateBusy {
		b.WriteString(m.spin.View() + styMuted.Render("working…") + "\n")
	}

	b.WriteString(styInputBox.Width(max(24, m.width-2)).Render(m.ta.View()))
	b.WriteString("\n")
	b.WriteString(m.hint())
	return b.String()
}

func (m *chatUI) hint() string {
	parts := []string{
		styKey.Render("enter") + styMuted.Render(" send"),
		styKey.Render("ctrl+j") + styMuted.Render(" newline"),
		styKey.Render("↑↓") + styMuted.Render(" history"),
		styKey.Render("ctrl+c") + styMuted.Render(" quit"),
	}
	line := "  " + strings.Join(parts, styMuted.Render("  ·  "))
	if n := len(m.queued); n > 0 {
		line += styMuted.Render(fmt.Sprintf("  ·  %d queued", n))
	}
	return line
}

func (m *chatUI) approvalView() string {
	ev := m.pending
	var b strings.Builder
	b.WriteString(styApprovalTitle.Render("spore wants to run "+ev.Tool) + "\n")
	b.WriteString(styMuted.Render("matched policy rule "+quote(ev.Rule)) + "\n\n")
	b.WriteString(prettyArgs(ev.Args, 10) + "\n\n")

	keys := []string{
		styKey.Render("y") + styMuted.Render(" allow once"),
		styKey.Render("n") + styMuted.Render(" deny"),
		styKey.Render("s") + styMuted.Render(" allow all "+ev.Tool+" this session"),
	}
	if ev.Pattern != "" {
		keys = append(keys, styKey.Render("p")+styMuted.Render(" always allow "+ev.Pattern))
	}
	b.WriteString(strings.Join(keys, "\n"))
	return styApprovalBox.Width(max(24, m.width-2)).Render(b.String())
}

func quote(s string) string { return "\"" + s + "\"" }

// firstLine clips a multi-line message to one short line for a status note.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 60 {
		s = s[:60] + "…"
	}
	return s
}
