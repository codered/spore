package companion

import (
	"fmt"
	"strings"

	"github.com/codered/spore/internal/store"
)

// Verdict is what a watch run decided: an alert for the user, or nothing to
// report. Kind is store.VerdictAlert or store.VerdictNothing.
type Verdict struct {
	Kind    string
	Message string
}

// VerdictSuffix is appended to a watch prompt so the run ends with a verdict
// line that ParseVerdict can read.
const VerdictSuffix = "\n\nEnd your work with a verdict. Begin your reply with exactly one of these lines:\n" +
	"ALERT: <what to tell the user, in a few lines>\n" +
	"NOTHING: <one line on what you checked>\n" +
	"Use ALERT only when what you found crosses the goal's bar."

// alertClip is how many runes of an alert reach the chat. The rest waits in
// the Jobs folder.
const alertClip = 1200

// alertFooter follows a clipped alert.
const alertFooter = "\n\n· the rest is in the Jobs folder"

// ParseVerdict reads a watch run's reply. A reply that does not start with a
// verdict marker is treated as an alert, so the user never loses a finding.
func ParseVerdict(reply string) Verdict {
	body := stripFence(strings.TrimSpace(reply))
	lines := strings.Split(body, "\n")
	first := -1
	for i, l := range lines {
		if strings.TrimSpace(l) != "" {
			first = i
			break
		}
	}
	if first < 0 {
		return Verdict{Kind: store.VerdictAlert, Message: ""}
	}
	line := strings.TrimLeft(lines[first], "*#_> \t")
	rest := strings.Join(lines[first+1:], "\n")
	switch {
	case hasMarker(line, "NOTHING:"):
		return Verdict{Kind: store.VerdictNothing, Message: markerText(line, "NOTHING:")}
	case hasMarker(line, "ALERT:"):
		msg := markerText(line, "ALERT:")
		if rest != "" {
			msg += "\n" + rest
		}
		return Verdict{Kind: store.VerdictAlert, Message: strings.TrimSpace(msg)}
	}
	return Verdict{Kind: store.VerdictAlert, Message: strings.TrimSpace(reply)}
}

// stripFence removes one surrounding ``` fence, with or without a language tag.
func stripFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	nl := strings.IndexByte(s, '\n')
	if nl < 0 {
		return s
	}
	s = s[nl+1:]
	if t := strings.TrimSpace(s); strings.HasSuffix(t, "```") {
		s = t[:len(t)-3]
	}
	return s
}

// hasMarker reports whether line starts with marker, ignoring case.
func hasMarker(line, marker string) bool {
	return len(line) >= len(marker) && strings.EqualFold(line[:len(marker)], marker)
}

// markerText returns the text after a marker on its line, without the
// emphasis marks that may wrap the colon.
func markerText(line, marker string) string {
	rest := strings.TrimLeft(line[len(marker):], "*_")
	return strings.TrimSpace(rest)
}

// ResearchPrompt is what a research run is told: the question, framed so the
// run knows it is unattended, read-only, and must list its sources.
func ResearchPrompt(label, question string) string {
	return fmt.Sprintf("You are running research for spore's companion, on %s. Question: %s\n\n"+
		"You run unattended, with read-only tools and your own notes; nobody will answer you. "+
		"Answer the question in a few short paragraphs and list your sources (URLs, files or tool results). "+
		"If you need a tool you do not have, say which and why.",
		label, strings.TrimSpace(question))
}

// AlertText is the text of an alert in the chat: at most 1200 runes, then a
// footer that points at the Jobs folder for the rest.
func AlertText(message string) string {
	r := []rune(message)
	if len(r) <= alertClip {
		return message
	}
	return string(r[:alertClip]) + alertFooter
}
