package discord

import (
	"strings"
	"unicode/utf8"
)

// dialect rewrites the markdown a model writes into the markdown Discord
// actually renders. Discord's flavour has no tables, stops at three levels of
// heading, and has no horizontal rule, so text written for a full markdown
// renderer arrives as literal pipes, hashes and dashes.
//
// It is a stream, not a document: text arrives in deltas and is put on screen
// before the rest of it exists. A dialect therefore carries the two pieces of
// state that a line's meaning depends on — whether we are inside a fenced code
// block, and whether the last thing emitted ended mid-line — and offers hold
// so the renderer can withhold a construct it cannot yet rewrite.
type dialect struct {
	inFence bool
	// partial is the tail of the last emitted text when it did not end in a
	// newline. That line's opening bytes are already on screen, so it can no
	// longer become a table row or a heading however it continues.
	partial string
}

// holdLimit bounds how much text a dialect will withhold waiting for a
// construct to end. A model that writes a thousand-row table, or a lone '|'
// that never becomes a table, must not starve the screen or defeat the
// message-limit split; past this much the text goes out unrewritten.
const holdLimit = 1200

// hold reports how many trailing bytes of s must be withheld from the screen
// because they may be part of a construct that is not finished arriving.
// Rewriting half a table would put a broken code block on screen that no
// later edit can repair, since what is already sent has been converted.
func (d *dialect) hold(s string) int {
	if d.inFence || s == "" {
		return 0
	}
	// A line whose opening bytes are already on screen is beyond rewriting,
	// so it can never be the start of a held run.
	continuation := d.partial != ""

	lastNL := strings.LastIndexByte(s, '\n') + 1
	holdFrom := len(s)
	if tail := s[lastNL:]; tail != "" && !(lastNL == 0 && continuation) && mayOpenRewrite(tail) {
		holdFrom = lastNL
	}
	// Extend back over a trailing run of table rows: more rows may follow.
	// Anything other than a table row between the run and the end means the
	// table is already finished and needs no holding.
	if holdFrom == lastNL {
		for holdFrom > 0 {
			prev := strings.LastIndexByte(s[:holdFrom-1], '\n') + 1
			if prev == 0 && continuation {
				break
			}
			if !isTableLine(s[prev : holdFrom-1]) {
				break
			}
			holdFrom = prev
		}
	}
	if len(s)-holdFrom > holdLimit {
		return 0
	}
	return len(s) - holdFrom
}

// convert rewrites s into Discord's dialect and advances the stream state.
// final says s is the end of the text — a trailing line with no newline is
// then a whole line and is rewritten like any other, rather than being left
// for a continuation that will never come.
func (d *dialect) convert(s string, final bool) string {
	if s == "" {
		if final {
			d.partial = ""
		}
		return ""
	}
	var out strings.Builder
	rest := s
	// Finish the line already on screen verbatim before rewriting anything.
	if d.partial != "" {
		i := strings.IndexByte(rest, '\n')
		if i < 0 {
			d.partial += rest
			if final {
				d.partial = ""
			}
			return rest
		}
		head := rest[:i+1]
		out.WriteString(head)
		d.trackFence(head[:i])
		d.partial = ""
		rest = rest[i+1:]
	}

	// rest now begins at a line boundary.
	body, trailing := rest, ""
	if i := strings.LastIndexByte(rest, '\n'); i+1 < len(rest) {
		body, trailing = rest[:i+1], rest[i+1:]
	}
	if final && trailing != "" {
		body, trailing = rest, ""
	}
	if body != "" {
		lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
		for _, l := range d.rewrite(lines) {
			out.WriteString(l)
			out.WriteString("\n")
		}
		// A body that did not end in a newline (the final case) must not grow
		// one, or the seam between messages gains a line break.
		if !strings.HasSuffix(body, "\n") {
			s := out.String()
			out.Reset()
			out.WriteString(strings.TrimSuffix(s, "\n"))
		}
	}
	out.WriteString(trailing)
	d.partial = trailing
	if final {
		d.partial = ""
	}
	return out.String()
}

// rewrite converts whole lines, grouping the runs that must be read together.
func (d *dialect) rewrite(lines []string) []string {
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			d.inFence = !d.inFence
			out = append(out, l)
			continue
		}
		if d.inFence {
			out = append(out, l)
			continue
		}
		if isTableLine(l) {
			j := i
			for j < len(lines) && isTableLine(lines[j]) {
				j++
			}
			run := lines[i:j]
			out = append(out, renderTable(run)...)
			i = j - 1
			continue
		}
		if isRule(l) {
			// A rule usually arrives fenced by blank lines. Dropping only
			// the rule would leave those two stacked into a gap wider than
			// the paragraph break it replaces, so one of them goes with it.
			if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" &&
				i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				i++
			}
			continue
		}
		out = append(out, demoteHeading(l))
	}
	return out
}

// trackFence advances the fence state over one line emitted verbatim.
func (d *dialect) trackFence(line string) {
	if strings.HasPrefix(strings.TrimSpace(line), "```") {
		d.inFence = !d.inFence
	}
}

// mayOpenRewrite reports whether a line that has only partly arrived could
// still turn into something this package rewrites. Prose that cannot must
// never be held, or ordinary streaming stalls for a throttle interval.
func mayOpenRewrite(line string) bool {
	t := strings.TrimSpace(line)
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "|") || strings.HasPrefix(t, "#") {
		return true
	}
	// A rule's opening run: '-', '*' or '_' and nothing else yet.
	return strings.Trim(t, "-") == "" || strings.Trim(t, "*") == "" || strings.Trim(t, "_") == ""
}

func isTableLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

// isRule reports a horizontal rule: three or more of the same marker, spaces
// allowed between them. Discord renders none of the three, so they arrive as
// literal punctuation on a line of their own.
func isRule(line string) bool {
	t := strings.ReplaceAll(strings.TrimSpace(line), " ", "")
	if utf8.RuneCountInString(t) < 3 {
		return false
	}
	for _, m := range []string{"-", "*", "_"} {
		if strings.Trim(t, m) == "" {
			return true
		}
	}
	return false
}

// demoteHeading turns a heading deeper than Discord's three levels into a
// bold line, which is as close as the dialect gets to a fourth level.
func demoteHeading(line string) string {
	t := strings.TrimSpace(line)
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n < 4 {
		return line
	}
	text := strings.TrimSpace(t[n:])
	if text == "" {
		return line
	}
	return "**" + text + "**"
}

// renderTable turns a run of table rows into a fenced code block with padded
// columns. Discord has no table syntax at all, but it does render a fence in
// a monospaced font, which is the only way to make columns line up.
//
// A run without a delimiter row is not a table — it is prose that happens to
// start with a pipe — and is returned untouched.
func renderTable(run []string) []string {
	rows := make([][]string, 0, len(run))
	delim := false
	for i, l := range run {
		cells := tableCells(l)
		if i < 2 && isDelimiterRow(cells) {
			delim = true
			continue
		}
		rows = append(rows, cells)
	}
	if !delim || len(rows) == 0 {
		return run
	}

	width := []int{}
	for _, r := range rows {
		for i, c := range r {
			for len(width) <= i {
				width = append(width, 0)
			}
			if n := utf8.RuneCountInString(c); n > width[i] {
				width[i] = n
			}
		}
	}

	out := make([]string, 0, len(rows)+2)
	out = append(out, "```")
	for _, r := range rows {
		var b strings.Builder
		for i := range width {
			c := ""
			if i < len(r) {
				c = r[i]
			}
			b.WriteString(c)
			if i < len(width)-1 {
				b.WriteString(strings.Repeat(" ", width[i]-utf8.RuneCountInString(c)+2))
			}
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return append(out, "```")
}

// tableCells splits a row on its pipes, dropping the empty cells a leading
// and trailing pipe produce.
func tableCells(line string) []string {
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	cells := make([]string, len(parts))
	for i, p := range parts {
		cells[i] = strings.TrimSpace(p)
	}
	return cells
}

// isDelimiterRow reports the '|---|:--:|' row that makes a run of pipe lines
// a table rather than prose.
func isDelimiterRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimPrefix(strings.TrimSuffix(c, ":"), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}
