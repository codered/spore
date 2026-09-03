package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The palette is deliberately small and adaptive: spore runs in whatever
// terminal the operator already has, so every colour must stay legible on a
// light background and a dark one.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "#0B7A6B", Dark: "#3DDC97"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8A8F98"}
	colDanger = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FF6B6B"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FFB454"}
	colTool   = lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#C4A2FF"}
)

var (
	styMuted  = lipgloss.NewStyle().Foreground(colMuted)
	styAccent = lipgloss.NewStyle().Foreground(colAccent)
	styDanger = lipgloss.NewStyle().Foreground(colDanger)
	styTool   = lipgloss.NewStyle().Foreground(colTool)
	styKey    = lipgloss.NewStyle().Foreground(colAccent).Bold(true)

	// styBadge is the small capsule that labels the banner.
	styBadge = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#0B1215")).
			Background(colAccent).
			Bold(true).
			Padding(0, 1)

	// styInputBox frames the prompt. A border costs two columns and two rows
	// and buys the one thing a chat CLI most lacks: an obvious place to type.
	styInputBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colAccent).
			Padding(0, 1)

	styApprovalBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colWarn).
			Padding(0, 1)

	styApprovalTitle = lipgloss.NewStyle().Foreground(colWarn).Bold(true)
)

// banner renders the greeting printed once when a chat starts.
func banner(sessionID, webURL string) string {
	line1 := styBadge.Render("spore") + "  " + styMuted.Render("session "+sessionID)
	line2 := styMuted.Render("  web ") + styAccent.Render(webURL)
	return line1 + "\n" + line2
}

// toolCallLine renders one tool invocation. Arguments are collapsed onto a
// single line and clipped: the transcript is a record of what ran, not a
// place to read a large payload.
func toolCallLine(tool, args string, width int) string {
	one := strings.Join(strings.Fields(args), " ")
	budget := width - len(tool) - 8
	if budget < 20 {
		budget = 20
	}
	if len(one) > budget {
		one = one[:budget] + "…"
	}
	return styTool.Render("  ▸ "+tool) + " " + styMuted.Render(one)
}

// toolResultLine renders the outcome of a tool call as a size, or as an
// error marker when the tool failed.
func toolResultLine(content string, isError, truncated bool) string {
	if isError {
		return styDanger.Render(fmt.Sprintf("  ✗ failed · %d bytes", len(content)))
	}
	note := ""
	if truncated {
		note = " · truncated"
	}
	return styMuted.Render(fmt.Sprintf("  ✓ %d bytes%s", len(content), note))
}

// turnFooter renders the per-turn accounting line.
func turnFooter(model string, in, out int, cost float64, showCost bool) string {
	s := fmt.Sprintf("%s · %d in / %d out", model, in, out)
	if showCost {
		s += fmt.Sprintf(" · $%.4f", cost)
	}
	return styMuted.Render("  " + s)
}

// prettyArgs renders a tool call's JSON arguments for an approval prompt,
// clipped to maxLines so a large payload cannot push the answer keys off the
// screen.
func prettyArgs(args string, maxLines int) string {
	text := args
	if indented, err := json.MarshalIndent(json.RawMessage(args), "", "  "); err == nil {
		text = string(indented)
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > maxLines {
		lines = append(lines[:maxLines], fmt.Sprintf("… %d more lines", len(lines)-maxLines))
	}
	return strings.Join(lines, "\n")
}

// mdStyleName is resolved once. Asking the terminal for its background
// colour costs a round trip with a timeout attached, which is affordable at
// startup and is not affordable on every window resize -- and in a test or a
// pipe, where nothing answers, it is the whole runtime.
var mdStyleName = sync.OnceValue(func() string {
	if lipgloss.HasDarkBackground() {
		return "dark"
	}
	return "light"
})

// newMarkdown builds a glamour renderer for the given width. It returns nil
// when glamour cannot be configured, and every caller treats a nil renderer
// as "print the text as it came" -- markdown styling is a nicety, never a
// reason to lose a reply.
func newMarkdown(width int) *glamour.TermRenderer {
	if width < 20 {
		width = 20
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle(mdStyleName()),
		glamour.WithWordWrap(width),
	)
	if err != nil {
		return nil
	}
	return r
}

// renderMarkdown formats assistant prose. Glamour adds its own leading and
// trailing blank lines, which look wrong in a scrollback where every turn is
// already separated, so they are trimmed back off.
func renderMarkdown(r *glamour.TermRenderer, text string) string {
	trimmed := strings.TrimRight(text, "\n")
	if trimmed == "" {
		return ""
	}
	if r == nil {
		return trimmed
	}
	out, err := r.Render(trimmed)
	if err != nil {
		return trimmed
	}
	return trimTrailingCells(strings.Trim(out, "\n"))
}

// trimTrailingCells removes the run of blank cells glamour pads each line
// with. The padding is invisible on screen but is real whitespace in a copied
// selection, and it is wrapped in styling escapes, so plain string trimming
// cannot see it: the visible width has to be measured first.
func trimTrailingCells(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		visible := ansi.Strip(line)
		kept := strings.TrimRight(visible, " \t")
		if len(kept) == len(visible) {
			continue
		}
		if kept == "" {
			lines[i] = ""
			continue
		}
		lines[i] = ansi.Truncate(line, lipgloss.Width(kept), "")
	}
	return strings.Join(lines, "\n")
}

// tailLines clips a rendered block to its last n display lines, marking the
// clip. The live region of an in-flight turn is bounded this way so a long
// reply cannot grow the inline view past the height of the terminal.
func tailLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	kept := lines[len(lines)-n:]
	return styMuted.Render(fmt.Sprintf("  … %d earlier lines", len(lines)-n)) + "\n" +
		strings.Join(kept, "\n")
}
