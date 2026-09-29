// Package tui is the full-screen terminal interface behind `spore chat`: a
// herdr-style sidebar of sessions, a k9s-style modal keymap, and a transcript
// rebuilt from structured blocks so it can re-wrap, collapse and stream.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The palette is deliberately small and adaptive: spore runs in whatever
// terminal the operator already has, so every colour must stay legible on a
// light background and a dark one.
var (
	colAccent = lipgloss.AdaptiveColor{Light: "#0B7A6B", Dark: "#3DDC97"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8A9792"}
	colDanger = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FF6B6B"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FFB454"}
	colTool   = lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#C4A2FF"}
	colVisor  = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#67E8F9"}

	// Fills sit behind a whole row. They mark the row, not its content, so
	// the glyphs drawn on them keep their own colours.
	colFillSel    = lipgloss.AdaptiveColor{Light: "#DDEFE7", Dark: "#1D2A26"}
	colFillWarn   = lipgloss.AdaptiveColor{Light: "#FBEBD3", Dark: "#2A2418"}
	colFillCursor = lipgloss.AdaptiveColor{Light: "#ECF1EF", Dark: "#1A2320"}
)

var (
	styMuted    = lipgloss.NewStyle().Foreground(colMuted)
	styAccent   = lipgloss.NewStyle().Foreground(colAccent)
	styDanger   = lipgloss.NewStyle().Foreground(colDanger)
	styWarn     = lipgloss.NewStyle().Foreground(colWarn)
	styTool     = lipgloss.NewStyle().Foreground(colTool)
	styKey      = lipgloss.NewStyle().Foreground(colAccent).Bold(true)
	styHeader   = lipgloss.NewStyle().Foreground(colMuted).Bold(true)
	stySelected = lipgloss.NewStyle().Reverse(true)
	styBadge    = lipgloss.NewStyle().Foreground(colAccent).Reverse(true).Bold(true)

	// styMode is the vim-style mode badge at the left of the status bar.
	styMode = lipgloss.NewStyle().
		Foreground(lipgloss.Color("#0B1215")).
		Background(colAccent).
		Bold(true)

	// styModeWarn is the mode badge while an approval waits on the person.
	styModeWarn = styMode.Background(colWarn)

	styVisor = lipgloss.NewStyle().Foreground(colVisor)
	styFaint = lipgloss.NewStyle().Faint(true)

	// styTabOn is the lit tab in the top nav.
	styTabOn = lipgloss.NewStyle().Foreground(colAccent).Background(colFillSel).Bold(true)

	// styInputBox frames the prompt while typing; styInputIdle is the same
	// frame, dimmed, when the keys are driving navigation instead.
	styInputBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colAccent).
			Padding(0, 1)
	styInputIdle = styInputBox.BorderForeground(colMuted)

	styApprovalTitle = lipgloss.NewStyle().Foreground(colWarn).Bold(true)

	// styApprovalCard frames the approval drawn over the transcript.
	styApprovalCard = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colWarn).
			Padding(1, 2)

	// styModal frames a question that must be answered before anything
	// else: approvals, deletes, stops.
	styModal = lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(colWarn).
			Padding(1, 2)
)

// mdStyleName is resolved once. Asking the terminal for its background
// colour costs a round trip with a timeout attached, which is affordable at
// startup and not on every resize. It is a variable so tests can pin a style
// that does not depend on the terminal.
var mdStyleName = sync.OnceValue(func() string {
	if lipgloss.HasDarkBackground() {
		return "dark"
	}
	return "light"
})

// newMarkdown builds a glamour renderer for the given width. It returns nil
// when glamour cannot be configured, and every caller treats a nil renderer
// as "print the text as it came".
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

// renderMarkdown formats assistant prose, trimming the blank lines glamour
// adds around every document.
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
// with. The padding is wrapped in styling escapes, so the visible width has
// to be measured before trimming.
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

// prettyArgs renders a tool call's JSON arguments, clipped to maxLines.
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

// humanTokens keeps an estimate short: 38.0k reads better than 38004.
func humanTokens(n int) string {
	if n < 1000 {
		return strconv.Itoa(n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

// short is the four-character form of a session id the sidebar and status
// bar show.
func short(id string) string {
	if len(id) > 4 {
		return id[:4]
	}
	return id
}

// tildePath abbreviates the home directory.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}

// firstLine clips a message to one short line for a status note.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	return clip(s, 60)
}

// clip shortens s to at most n display cells, marking the cut.
func clip(s string, n int) string {
	if n < 1 {
		return ""
	}
	if ansi.StringWidth(s) <= n {
		return s
	}
	return ansi.Truncate(s, n, "…")
}

// oneLine collapses any run of whitespace, newlines included, to one space.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// fill draws line on a background of bg across width cells. Styled text
// inside line resets its colours as it ends, which would also end the
// background, so the background is put back after every reset.
func fill(line string, width int, bg lipgloss.TerminalColor) string {
	line += strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
	return refill(line, bgSeq(bg))
}

// refill opens s with seq and reopens it after every SGR reset in s.
func refill(s, seq string) string {
	if seq == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+seq)
	s = strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+seq)
	return seq + s + "\x1b[0m"
}

// bgSeq is the escape sequence that sets bg in this terminal, or empty
// when the terminal has no colour.
func bgSeq(bg lipgloss.TerminalColor) string {
	s := lipgloss.NewStyle().Background(bg).Render("\x00")
	if i := strings.IndexByte(s, 0); i > 0 {
		return s[:i]
	}
	return ""
}

// humanDur is a short elapsed time: 0.4s, 12s, 1m05s.
func humanDur(d time.Duration) string {
	d = max(0, d)
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
