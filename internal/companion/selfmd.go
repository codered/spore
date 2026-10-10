package companion

import (
	"fmt"
	"slices"
	"strings"
)

// SelfHeadings are self.md's sections, in order, without the "## ".
var SelfHeadings = []string{
	"What I'm curious about",
	"Threads with you",
	"How you like to be talked to",
	"Opinions I've formed",
}

// SelfTemplate is an empty self.md: every heading, no notes.
func SelfTemplate() string {
	var b strings.Builder
	for i, h := range SelfHeadings {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## " + h + "\n")
	}
	return b.String()
}

// AppendSelfNote adds "- text" at the end of heading's section. An empty
// body starts from the template; a body without that heading -- the user
// edited the file by hand -- gets the heading appended rather than the note
// refused, because the file is theirs too. Newlines in text are collapsed so
// a note can never forge a heading.
func AppendSelfNote(body, heading, text string) (string, error) {
	if !slices.Contains(SelfHeadings, heading) {
		return "", fmt.Errorf("heading %q must be one of: %s", heading, strings.Join(SelfHeadings, "; "))
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return "", fmt.Errorf("an empty note would add a bare bullet")
	}
	if strings.TrimSpace(body) == "" {
		body = SelfTemplate()
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == "## "+heading {
			at = i
			break
		}
	}
	if at < 0 {
		return body + "\n## " + heading + "\n\n- " + text + "\n", nil
	}
	// The section ends at the next "## " heading or the end of the file.
	end := len(lines)
	for i := at + 1; i < len(lines); i++ {
		if strings.HasPrefix(lines[i], "## ") {
			end = i
			break
		}
	}
	// Insert after the section's last non-blank line, keeping the blank
	// line that separates it from the next heading.
	ins := end
	for ins > at+1 && strings.TrimSpace(lines[ins-1]) == "" {
		ins--
	}
	bullet := "- " + text
	if ins == at+1 {
		// Empty section: a blank line between heading and first bullet.
		bullet = "\n" + bullet
	}
	out := append([]string{}, lines[:ins]...)
	out = append(out, bullet)
	out = append(out, lines[ins:]...)
	return strings.Join(out, "\n") + "\n", nil
}
