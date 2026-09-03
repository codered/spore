package recall

import "testing"

// FTS5 reads punctuation and bare keywords as query syntax, so raw user text
// is frequently a syntax error rather than an empty result. Every input must
// come out as a literal token conjunction.
func TestTokenizeQuotesEveryToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{`retry logic`, `"retry" "logic"`},
		{`what's the -v flag?`, `"what's" "the" "v" "flag"`},
		{`retry "logic`, `"retry" "logic"`},
		{`AND`, `"AND"`},
		{`a OR b`, `"a" "OR" "b"`},
		{`foo*`, `"foo"`},
		{`NEAR(x y)`, `"NEAR" "x" "y"`},
		{`naïve café`, `"naïve" "café"`},
		{`snake_case`, `"snake_case"`},
		{`  spaced   out  `, `"spaced" "out"`},
		{``, ``},
		{`   `, ``},
		{`!@#$%^&*()`, ``},
	}
	for _, c := range cases {
		if got := Tokenize(c.in); got != c.want {
			t.Errorf("Tokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The store repeats these strings because it cannot import this package.
func TestKindConstantsMatchTheStore(t *testing.T) {
	if KindMessage != "message" || KindSummary != "summary" || KindFact != "fact" {
		t.Fatal("kind constants drifted from the values internal/store writes")
	}
}
