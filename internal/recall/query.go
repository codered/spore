package recall

import "strings"

// Tokenize turns arbitrary text into an FTS5 MATCH expression that cannot be
// a syntax error. FTS5 reads `"`, `*`, `-`, `AND`, `OR` and `NEAR` as syntax,
// so a natural-language question fails to parse rather than returning nothing:
// splitting on non-word runes and quoting each token makes every query a
// literal conjunction of terms.
//
// An input with no word characters returns "", and the caller MUST treat that
// as "no hits" rather than passing it to MATCH, which is itself an error.
func Tokenize(q string) string {
	var b strings.Builder
	var cur []rune
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte('"')
		// A double quote cannot survive inside a quoted FTS5 string, and
		// tokenising already dropped it; nothing here can reintroduce one.
		b.WriteString(string(cur))
		b.WriteByte('"')
		cur = cur[:0]
	}
	for _, r := range q {
		if isWordRune(r) {
			cur = append(cur, r)
			continue
		}
		flush()
	}
	flush()
	return b.String()
}

// isWordRune keeps letters, digits, underscores, apostrophes and everything
// above ASCII, so accented words survive as single tokens.
func isWordRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '_', r == '\'':
		return true
	case r > 127:
		return true
	}
	return false
}
