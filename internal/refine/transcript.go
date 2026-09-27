package refine

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// maxTranscriptChars bounds the refiner's input. The newest part is kept:
// a correction near the end of a long session is the likeliest lesson.
const maxTranscriptChars = 120_000

// Transcript renders rows for the refiner. Tool results are reduced to their
// size, because what a web page or a file said must never reach a pass that
// writes persistent context; only what the user and spore said does. Note
// rows are spore talking to the reader, not conversation, and are skipped.
func Transcript(rows []store.Message) (string, error) {
	var b strings.Builder
	for _, r := range rows {
		if r.Role == store.RoleNote {
			continue
		}
		var blocks []provider.Block
		if err := json.Unmarshal(r.BlocksJSON, &blocks); err != nil {
			return "", fmt.Errorf("message %d: %w", r.Seq, err)
		}
		b.WriteString(r.Role)
		b.WriteString(": ")
		for _, bl := range blocks {
			switch bl.Type {
			case provider.BlockText:
				b.WriteString(bl.Text)
			case provider.BlockToolUse:
				fmt.Fprintf(&b, "[called %s]", bl.Name)
			case provider.BlockToolResult:
				fmt.Fprintf(&b, "[tool result: %d bytes]", len(bl.Content))
			}
		}
		b.WriteString("\n")
	}
	s := b.String()
	if len(s) > maxTranscriptChars {
		s = "[earlier conversation omitted]\n" + strings.ToValidUTF8(s[len(s)-maxTranscriptChars:], "")
	}
	return s, nil
}
