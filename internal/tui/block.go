package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/lipgloss"
)

type blockKind int

const (
	kindUser blockKind = iota
	kindText
	kindTool
	kindNotice
	kindError
	kindFooter
)

// block is one entry in a session's transcript. Everything the chat pane
// shows is a block, so a resize or an expand re-renders from data rather than
// from text that has already been printed.
type block struct {
	kind blockKind
	text string

	// kindTool
	toolID    string
	tool      string
	args      string
	result    string
	isError   bool
	truncated bool
	done      bool
	expanded  bool

	// kindText still receiving deltas.
	streaming bool

	// Render cache. cacheKey covers everything render's output depends on
	// except the block's own data, which touch() invalidates.
	cacheKey string
	cacheOut string

	// The stable part of a streaming reply, rendered once per change.
	stableSrc   string
	stableWidth int
	stableOut   string
}

// touch invalidates the render cache after the block's data changed.
func (b *block) touch() { b.cacheKey = "" }

// render draws the block at width. md may be nil. selected marks the tool
// cursor.
func (b *block) render(width int, md *glamour.TermRenderer, selected bool) string {
	key := fmt.Sprintf("%d/%t/%t", width, selected, b.expanded)
	if key == b.cacheKey {
		return b.cacheOut
	}
	out := b.draw(width, md, selected)
	b.cacheKey, b.cacheOut = key, out
	return out
}

func (b *block) draw(width int, md *glamour.TermRenderer, selected bool) string {
	wrap := lipgloss.NewStyle().Width(max(10, width))
	switch b.kind {
	case kindUser:
		return wrap.Render(styAccent.Render("› ") + b.text)
	case kindText:
		return b.drawText(width, md)
	case kindTool:
		return b.drawTool(width, selected)
	case kindNotice:
		return wrap.Render(styMuted.Render("· " + b.text))
	case kindError:
		return wrap.Render(styDanger.Render("✗ " + b.text))
	case kindFooter:
		return styMuted.Render(clip(b.text, width))
	}
	return ""
}

// drawText renders assistant prose. A finished block is markdown throughout.
// A streaming one renders only the paragraphs that are complete and shows
// the growing tail as plain wrapped text, so a half-written code fence or
// list never makes the whole reply reflow on every delta.
func (b *block) drawText(width int, md *glamour.TermRenderer) string {
	if !b.streaming {
		return renderMarkdown(md, b.text)
	}
	stable, tail := splitStable(b.text)
	if stable != b.stableSrc || width != b.stableWidth {
		b.stableSrc, b.stableWidth, b.stableOut = stable, width, ""
		if stable != "" {
			b.stableOut = renderMarkdown(md, stable)
		}
	}
	var parts []string
	if b.stableOut != "" {
		parts = append(parts, b.stableOut)
	}
	if tail != "" {
		parts = append(parts, lipgloss.NewStyle().Width(max(10, width)).Render(tail))
	}
	return strings.Join(parts, "\n")
}

// splitStable cuts text after the last blank line that is not inside a code
// fence. Everything before the cut is complete markdown.
func splitStable(text string) (stable, tail string) {
	cut := -1
	inFence := false
	pos := 0
	for _, line := range strings.SplitAfter(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		pos += len(line)
		if !inFence && trimmed == "" && strings.HasSuffix(line, "\n") {
			cut = pos
		}
	}
	if cut <= 0 {
		return "", text
	}
	return text[:cut], text[cut:]
}

// drawTool is one line collapsed -- `▸ bash go test ./…  ✗ exit 1` -- and
// the full arguments and result expanded.
func (b *block) drawTool(width int, selected bool) string {
	marker := "▸ "
	if selected {
		marker = "▶ "
	}
	status := styMuted.Render("…")
	if b.done {
		if b.isError {
			status = styDanger.Render("✗ " + clip(oneLine(firstLineOf(b.result)), 40))
		} else {
			status = styAccent.Render("✓ ") + styMuted.Render(summarise(b.result, b.truncated))
		}
	}
	head := styTool.Render(marker + b.tool)
	budget := width - lipgloss.Width(head) - lipgloss.Width(status) - 3
	line := head + " " + styMuted.Render(clip(oneLine(b.args), budget)) + "  " + status
	if !b.expanded {
		return line
	}
	var sb strings.Builder
	sb.WriteString(line)
	sb.WriteString("\n")
	sb.WriteString(indent(prettyArgs(b.args, 40), "    "))
	if b.done {
		sb.WriteString("\n")
		sb.WriteString(indent(clipLines(b.result, 200), "    "))
	}
	return lipgloss.NewStyle().Width(max(10, width)).Render(sb.String())
}

// summarise describes a successful result in a few words.
func summarise(result string, truncated bool) string {
	s := "empty"
	if trimmed := strings.TrimRight(result, "\n"); trimmed != "" {
		n := strings.Count(trimmed, "\n") + 1
		if n == 1 {
			s = clip(oneLine(trimmed), 40)
		} else {
			s = fmt.Sprintf("%d lines", n)
		}
	}
	if truncated {
		s += " · truncated"
	}
	return s
}

func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func clipLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… %d more lines", len(lines)-n)
}

func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = pad + l
	}
	return strings.Join(lines, "\n")
}
