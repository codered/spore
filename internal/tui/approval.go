package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

const (
	// cardMaxWidth keeps the approval card's rows readable on a wide terminal.
	cardMaxWidth = 76
	// callBoxLines is the most rows the card gives to what the call does.
	callBoxLines = 8
)

// waiting reports whether an approval waits on the selected session, its
// own or one of its sub-agents'.
func (m *Model) waiting() bool {
	if m.selected == "" {
		return false
	}
	_, _, ok := m.cache.approvalFor(m.selected)
	return ok
}

// approvalCard is the selected session's approval as a card for a w x h
// area, or empty when nothing waits. When h is short the card compacts
// itself (see build) so the answers stay on screen.
func (m *Model) approvalCard(w, h int) string {
	_, ev, ok := m.cache.approvalFor(m.selected)
	if !ok || m.selected == "" {
		return ""
	}
	// Everything on the card comes from the model, a tool server or config,
	// and the person approves what the card says; none of it may drive the
	// terminal.
	ev.Tool, ev.Origin, ev.Profile = visible(ev.Tool), visible(ev.Origin), visible(ev.Profile)
	ev.Rule, ev.Pattern = visible(ev.Rule), visible(ev.Pattern)
	cw := min(cardMaxWidth, w-4)
	inner := max(10, cw-6)
	// While typing, the answers take alt so a letter in the draft is never
	// read as one.
	mod := ""
	if m.mode == modeInsert {
		mod = "alt+"
	}
	// build draws the card at a compaction level: 0 is the full card, 1
	// drops its spacing, 2 also drops the origin and pattern rows. The
	// title, rule, call, answers and deadline are never dropped.
	build := func(box []string, level int) string {
		gap := func(rows []string) []string {
			if level == 0 {
				return append(rows, "")
			}
			return rows
		}
		rows := []string{styApprovalTitle.Render(clip("spore wants to run "+ev.Tool, inner))}
		if o := originLine(ev); o != "" && level < 2 {
			rows = append(rows, clip(o, inner))
		}
		rows = gap(append(rows, clip(ruleLine(ev.Rule), inner)))
		rows = gap(append(rows, box...))
		rows = append(rows, clip(styKey.Render(mod+"y")+styMuted.Render(" allow once   ")+
			styKey.Render(mod+"n")+styMuted.Render(" deny   ")+
			styKey.Render(mod+"s")+styMuted.Render(" allow "+ev.Tool+" this session"), inner))
		if ev.Pattern != "" && level < 2 {
			rows = append(rows, clip(styKey.Render(mod+"p")+styMuted.Render(" allow once + propose ")+styAccent.Render(ev.Pattern), inner))
		}
		rows = append(gap(rows), styMuted.Render(m.deadline(ev.ExpiresAt)))
		sty := styApprovalCard
		if level > 0 {
			sty = sty.Padding(0, 2)
		}
		return sty.Width(cw - 2).Render(strings.Join(rows, "\n"))
	}
	// At each level the call box gives up rows first, down to one; past
	// the last level the card is taller than h, and the pane clips its foot.
	box := callBox(ev.Tool, ev.Args, callBoxLines, inner)
	var card string
	for level := 0; level <= 2; level++ {
		card = build(box, level)
		extra := lipgloss.Height(card) - h
		if extra <= 0 {
			return card
		}
		card = build(callBox(ev.Tool, ev.Args, max(1, len(box)-extra), inner), level)
		if lipgloss.Height(card) <= h {
			return card
		}
	}
	return card
}

// originLine says who asked and under which profile; empty when neither
// is known.
func originLine(ev daemon.WireEvent) string {
	var parts []string
	if ev.Origin != "" {
		parts = append(parts, styMuted.Render("from sub-agent ")+styTool.Render(short(ev.Origin)))
	}
	if ev.Profile != "" {
		parts = append(parts, styMuted.Render("profile "+ev.Profile))
	}
	return strings.Join(parts, styMuted.Render(" · "))
}

// ruleLine names the rule that asked. An approval is always an ask, so the
// event needs no decision field to say so.
func ruleLine(rule string) string {
	switch rule {
	case "policy.default":
		return styMuted.Render("no rule matched · default is ask")
	case "":
		return styMuted.Render("matched ") + styWarn.Render("ask")
	}
	return styMuted.Render("matched ") + styWarn.Render("ask") + styMuted.Render("  "+rule)
}

// callBox is what the call will do, in at most maxRows rows of width w on a
// filled background: a shell command as a prompt line, anything else as
// its arguments.
func callBox(tool, args string, maxRows, w int) []string {
	var lines []string
	if tool == "shell_exec" {
		var in struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if json.Unmarshal([]byte(args), &in) == nil && in.Command != "" {
			for i, l := range strings.Split(strings.TrimRight(in.Command, "\n"), "\n") {
				l = visible(l)
				if i == 0 {
					l = "$ " + l
				} else {
					l = "  " + l
				}
				lines = append(lines, l)
			}
			if in.TimeoutSeconds > 0 {
				lines = append(lines, styMuted.Render(fmt.Sprintf("timeout %ds", in.TimeoutSeconds)))
			}
		}
	}
	if lines == nil {
		// prettyArgs re-marshals valid JSON, escaping its controls, but
		// passes anything else through as it came.
		for _, l := range strings.Split(prettyArgs(args, 1<<30), "\n") {
			lines = append(lines, visible(l))
		}
	}
	if len(lines) > maxRows {
		if maxRows >= 2 {
			more := len(lines) - (maxRows - 1)
			lines = append(lines[:maxRows-1:maxRows-1], styMuted.Render(fmt.Sprintf("… %d more lines", more)))
		} else {
			lines = lines[:1]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fill(clip(" "+l, w), w, colFillCursor)
	}
	return out
}

// visible makes every control character in s show as a symbol instead of
// acting on the terminal: a carriage return or an escape sequence in a
// command must not repaint the row that asks the person to approve it.
// Tabs become spaces, so widths are counted as they are drawn.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\t':
			b.WriteString("    ")
		case r < 0x20:
			b.WriteRune(0x2400 + r) // ␀..␟: ␍ for \r, ␛ for ESC
		case r == 0x7f:
			b.WriteRune('␡')
		case r >= 0x80 && r < 0xa0:
			b.WriteRune('\uFFFD') // C1 controls, CSI among them
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// visibleLines is visible for text of several lines: each line's controls
// show as symbols and the newlines between them stay.
func visibleLines(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = visible(l)
	}
	return strings.Join(lines, "\n")
}

// deadline is when the daemon denies an unanswered approval. expires is
// empty after a daemon restart, when nothing will time the ask out.
func (m *Model) deadline(expires string) string {
	at, err := time.Parse(time.RFC3339, expires)
	if expires == "" || err != nil {
		return "waiting (no timeout)"
	}
	left := at.Sub(m.opts.Now()).Round(time.Second)
	if left <= 0 {
		return "auto-denying…"
	}
	return fmt.Sprintf("auto-deny in %d:%02d", int(left.Minutes()), int(left.Seconds())%60)
}

// faint dims every row of s, so what is behind the card reads as behind it.
func faint(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = styFaint.Render(ansi.Strip(l))
	}
	return strings.Join(lines, "\n")
}

// draftLine tells someone typing how to answer without losing the draft.
func (m *Model) draftLine() string {
	if m.mode != modeInsert || !m.waiting() {
		return ""
	}
	msg := "answer with alt+y/n/s/p, or esc then y/n/s/p"
	if strings.TrimSpace(m.input.Value()) != "" {
		msg = "draft kept · " + msg
	}
	return styWarn.Render(clip(msg, m.mainWidth()))
}

func (m *Model) draftLineHeight() int {
	if m.draftLine() != "" {
		return 1
	}
	return 0
}
