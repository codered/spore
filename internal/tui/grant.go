package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codered/spore/internal/daemon"
)

// grantPicker is the duration list that `a` opens over an approval, or that
// `:grant` opens with no argument. Its keys are not the approval's: picking a
// duration starts a grant, and esc goes back with nothing sent.
type grantPicker struct {
	root      string
	durations []string
	cursor    int
	// back is the mode the picker returns to: INSERT keeps the draft in the
	// input, NORMAL stays NORMAL.
	back mode
}

// grantDurationsMsg carries the durations the daemon offers.
type grantDurationsMsg struct {
	list []string
	err  error
}

// grantMsg reports a grant start or end. The daemon has applied it by the
// time this arrives, so the countdown is set from the answer as well as
// from the grant event.
type grantMsg struct {
	root, duration string
	end            bool
	at             time.Time
	err            error
}

func (m *Model) loadGrantDurations() tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		list, err := be.GrantDurations(ctx)
		return grantDurationsMsg{list: list, err: err}
	}
}

// openGrantPicker opens the picker for root, returning to back when it
// closes. With no durations configured there is nothing to pick.
func (m *Model) openGrantPicker(root string, back mode) tea.Cmd {
	if len(m.grantDurations) == 0 {
		m.flash = "timed grants are off"
		return nil
	}
	m.grantPick = &grantPicker{root: root, durations: m.grantDurations, back: back}
	m.mode = modeGrant
	m.input.Blur()
	return nil
}

// keyGrant is the picker's key handling. A number picks that duration at
// once; enter picks the one under the cursor.
func (m *Model) keyGrant(k tea.KeyMsg) tea.Cmd {
	p := m.grantPick
	switch s := k.String(); {
	case s == "esc":
		m.grantPick = nil
		return m.returnFromGrant(p.back)
	case s == "j" || s == "down":
		p.cursor = min(p.cursor+1, len(p.durations)-1)
	case s == "k" || s == "up":
		p.cursor = max(p.cursor-1, 0)
	case s == "enter":
		return m.chooseGrant(p.cursor)
	case len(s) == 1 && s[0] >= '1' && s[0] <= '9':
		if i := int(s[0] - '1'); i < len(p.durations) {
			return m.chooseGrant(i)
		}
	}
	return nil
}

// chooseGrant starts the grant for the picked duration and closes the picker.
func (m *Model) chooseGrant(i int) tea.Cmd {
	p := m.grantPick
	m.grantPick = nil
	return tea.Batch(m.startGrant(p.root, p.durations[i]), m.returnFromGrant(p.back))
}

// returnFromGrant puts the mode back. INSERT takes the input focus again.
func (m *Model) returnFromGrant(back mode) tea.Cmd {
	if back == modeInsert {
		return m.enterInsert()
	}
	m.mode = modeNormal
	return nil
}

func (m *Model) startGrant(root, duration string) tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		at, err := be.StartGrant(ctx, root, duration)
		return grantMsg{root: root, duration: duration, at: at, err: err}
	}
}

func (m *Model) endGrant(root string) tea.Cmd {
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		return grantMsg{root: root, end: true, err: be.EndGrant(ctx, root)}
	}
}

// grantDone reports a grant's answer and sets the countdown from it.
func (m *Model) grantDone(msg grantMsg) {
	switch {
	case msg.err != nil && msg.end:
		m.flash = "could not end the grant: " + msg.err.Error()
	case msg.err != nil:
		m.flash = "could not start the grant: " + msg.err.Error()
	case msg.end:
		m.cache.get(msg.root).grantUntil = time.Time{}
		m.flash = "grant ended"
	default:
		m.cache.get(msg.root).grantUntil = msg.at
		m.flash = "calls that would ask now run without asking, for " + msg.duration
	}
}

// grantCommand runs `:grant [duration|off]`. The grant goes to the root of
// the selected session, since a sub-agent's calls run under it.
func (m *Model) grantCommand(args []string) tea.Cmd {
	if m.selected == "" {
		return nil
	}
	root := m.cache.rootOf(m.selected)
	if len(args) == 0 {
		return m.openGrantPicker(root, modeNormal)
	}
	if args[0] == "off" {
		return m.endGrant(root)
	}
	if len(m.grantDurations) == 0 {
		m.flash = "timed grants are off"
		return nil
	}
	if !slices.Contains(m.grantDurations, args[0]) {
		m.flash = "unknown grant duration " + args[0] + ": use " + strings.Join(m.grantDurations, ", ")
		return nil
	}
	return m.startGrant(root, args[0])
}

// grantLeft is how long the grant covering the selected session has left;
// zero or less when none is live.
func (m *Model) grantLeft() time.Duration {
	if m.selected == "" {
		return 0
	}
	until := m.cache.grantOf(m.selected)
	if until.IsZero() {
		return 0
	}
	return until.Sub(m.opts.Now())
}

// grantCountdown is left as the header shows it: m:ss, or h:mm:ss from an hour.
func grantCountdown(left time.Duration) string {
	secs := int(left.Round(time.Second) / time.Second)
	h, mins, s := secs/3600, secs/60%60, secs%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, mins, s)
	}
	return fmt.Sprintf("%d:%02d", mins, s)
}

// grantRow is the approval card's line for the picker: it starts a grant
// the way y starts an allow, for the calls that would ask.
func grantRow(mod string) string {
	return styKey.Render(mod+"a") + styMuted.Render(" allow all for…")
}

// grantParts are the picker's keys.
func grantParts() []string {
	return []string{hint("1-9", "pick"), hint("j/k", "move"), hint("enter", "start"), hint("esc", "cancel")}
}

// grantView is the picker on screen: the durations, the cursor on one, and
// what a grant does not cover.
func (m *Model) grantView() string {
	p := m.grantPick
	lines := []string{
		styApprovalTitle.Render("Allow calls without asking for…"),
		styMuted.Render("calls policy would ask about run now; a deny rule still blocks"),
	}
	for i, d := range p.durations {
		row := fmt.Sprintf("%d  %s", i+1, d)
		if i == p.cursor {
			lines = append(lines, styAccent.Render("> "+row))
		} else {
			lines = append(lines, styMuted.Render("  "+row))
		}
	}
	lines = append(lines, "", strings.Join(grantParts(), "  "))
	cw := 0
	for _, l := range lines {
		cw = max(cw, lipgloss.Width(l))
	}
	cw = min(cw, max(10, m.width-10))
	return styModal.Width(cw + 4).Render(strings.Join(lines, "\n"))
}

// grantEvent sets a root session's countdown from a grant event. An empty
// expiry means the grant ended, so the zero time clears it.
func grantEvent(sv *sessionView, ev daemon.WireEvent) {
	sv.grantUntil = time.Time{}
	if at, err := time.Parse(time.RFC3339, ev.ExpiresAt); err == nil {
		sv.grantUntil = at
	}
}
