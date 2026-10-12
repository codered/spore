package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// proposalChoices maps the alt keys to a proposal's three buttons.
var proposalChoices = map[string]string{"y": "yes", "n": "not-now", "x": "never"}

// proposalsMsg carries the companion's open proposals for session.
type proposalsMsg struct {
	session string
	list    []daemon.ProposalJSON
	err     error
}

// proposalAnsweredMsg is the daemon's answer to a proposal key.
type proposalAnsweredMsg struct {
	session, text string
	err           error
}

// fetchProposals asks for the open proposals when id is the companion
// session, and clears them otherwise: only the companion session has a bar.
func (m *Model) fetchProposals(id string) tea.Cmd {
	if m.cache.get(id).info.Source != "companion" {
		m.proposals = nil
		return nil
	}
	be, ctx := m.be, m.ctx
	return func() tea.Msg {
		list, err := be.Proposals(ctx)
		return proposalsMsg{session: id, list: list, err: err}
	}
}

// proposalOpen reports whether the session on screen has a proposal open.
func (m *Model) proposalOpen() bool {
	return len(m.proposals) > 0 && m.cache.get(m.selected).info.Source == "companion"
}

// proposalKey answers the newest open proposal for alt+y/n/x. ok is false
// when the key is not the proposal's and must fall through as before. An
// approval waiting in the session answers first: alt+y and alt+n fall
// through to it, and alt+x does nothing rather than reach "cancel agent".
func (m *Model) proposalKey(k string) (tea.Cmd, bool) {
	choice, isKey := proposalChoices[k]
	if !isKey || !m.proposalOpen() {
		return nil, false
	}
	if m.waiting() {
		if k == "x" {
			m.flash = "answer the approval first"
			return nil, true
		}
		return nil, false
	}
	p := m.proposals[0]
	m.proposals = m.proposals[1:]
	m.flash = "answering…"
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		text, err := be.AnswerProposal(ctx, p.ID, choice)
		return proposalAnsweredMsg{session: id, text: text, err: err}
	}, true
}

// proposalView is the open proposal docked above the input, or "" when
// none shows: not the companion, nothing open, a view or help on screen, or
// an approval waiting (it answers first). It is not drawn over the
// transcript as the approval card is, because a proposal can stay open for
// days and the conversation about it must stay readable.
func (m *Model) proposalView() string {
	if !m.proposalOpen() || m.table != nil || m.help || m.waiting() {
		return ""
	}
	w := max(10, m.mainWidth()-2)
	inner := max(4, w-2)
	text := strings.Join(strings.Fields(visible(m.proposals[0].Text)), " ")
	lines := strings.Split(lipgloss.NewStyle().Width(inner).Render(text), "\n")
	if len(lines) > 2 {
		lines = lines[:2]
		lines[1] = ansi.Truncate(strings.TrimRight(lines[1], " "), inner-1, "") + "…"
	}
	keys := styKey.Render("alt+y") + styMuted.Render(" yes   ") +
		styKey.Render("alt+n") + styMuted.Render(" not now   ") +
		styKey.Render("alt+x") + styMuted.Render(" never")
	if n := len(m.proposals) - 1; n > 0 {
		keys += styMuted.Render(fmt.Sprintf("   (+%d)", n))
	}
	rows := append([]string{styAccent.Bold(true).Render("spore proposes")}, lines...)
	rows = append(rows, clip(keys, inner))
	return styProposal.Width(w).Render(strings.Join(rows, "\n"))
}

// proposalHeight is how many rows the bar takes from the transcript.
func (m *Model) proposalHeight() int {
	if v := m.proposalView(); v != "" {
		return lipgloss.Height(v)
	}
	return 0
}

// companion runs /companion [args] and shows its text as a notice.
func (m *Model) companion(args []string) tea.Cmd {
	be, ctx, id := m.be, m.ctx, m.selected
	m.cache.get(id).add(kindNotice, "companion…")
	return func() tea.Msg {
		out, err := be.Companion(ctx, args)
		if err != nil {
			return noticeMsg{session: id, text: "companion failed: " + err.Error(), isErr: true}
		}
		return noticeMsg{session: id, text: strings.TrimRight(out, "\n")}
	}
}
