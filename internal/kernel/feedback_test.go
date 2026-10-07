package kernel

import (
	"context"
	"strings"
	"testing"
)

// yaegi reports a panic at the last call the function made, not at the
// failing expression, so the error must say what went wrong in words. A
// local model retried the same out-of-range program three times on the bare
// "reflect: slice index out of range".
func TestPanicsExplainThemselves(t *testing.T) {
	cases := map[string]struct{ body, want string }{
		"index": {`func main() {
	var xs []int
	fmt.Println(xs[0])
}`, "past the end"},
		"slice bounds": {`func main() {
	s := "abc"
	n := 10
	fmt.Println(s[:n])
}`, "past the end"},
		"nil map": {`func main() {
	var m map[string]int
	m["a"] = 1
	fmt.Println(m)
}`, "make("},
		"nil pointer": {`type T struct{ X int }

func main() {
	var p *T
	fmt.Println(p.X)
}`, "nil pointer"},
		"type assertion": {`func main() {
	var v any = "s"
	fmt.Println(v.(int))
}`, "comma-ok"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), prog(c.body, "fmt"), &fakeRunner{}, opts())
			if err == nil {
				t.Fatal("the program did not fail")
			}
			msg := err.Error()
			if !strings.HasPrefix(msg, "panic: ") {
				t.Errorf("err = %q, want it to start with panic:", msg)
			}
			if !strings.Contains(msg, c.want) {
				t.Errorf("err = %q, want a hint containing %q", msg, c.want)
			}
			if !strings.Contains(msg, "approximate") {
				t.Errorf("err = %q, want it to warn that the position is approximate", msg)
			}
		})
	}
}

func TestAPanicTheProgramRaisesKeepsItsValue(t *testing.T) {
	_, err := Run(context.Background(), prog(`func main() { panic("no rows") }`), &fakeRunner{}, opts())
	if err == nil || !strings.HasPrefix(err.Error(), "panic: no rows") {
		t.Errorf("err = %v, want it to start with panic: no rows", err)
	}
}

// yaegi v0.16.1 has no min or max. Models use them anyway, prompt warning
// or not, so the kernel supplies them.
func TestMinAndMaxAreSupplied(t *testing.T) {
	res, err := Run(context.Background(), prog(`func main() {
	s := "hello world"
	var a, b float64 = 3.5, 1.2
	var n int64 = 7
	fmt.Println(s[:min(len(s), 5)], min(a, b, 2.0), max(a, b), max(n, 3), min(2, 1), max("a", "b"))
}`, "fmt"), &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := "hello 1.2 3.5 7 1 b\n"; res.Output != want {
		t.Errorf("Output = %q, want %q", res.Output, want)
	}
}

func TestOwnMinAndMaxAreLeftAlone(t *testing.T) {
	res, err := Run(context.Background(), prog(`func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func main() {
	max := 9
	fmt.Println(min(4, 2), max)
}`, "fmt"), &fakeRunner{}, opts())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Output != "2 9\n" {
		t.Errorf("Output = %q", res.Output)
	}
}

// The supplied min is generic, and yaegi cannot infer it from untyped float
// constants alone. The error must say how to fix that, not name a type the
// model never wrote.
func TestSuppliedMinExplainsUntypedFloats(t *testing.T) {
	_, err := Run(context.Background(), prog(`func main() { fmt.Println(min(1.5, 2.5)) }`, "fmt"), &fakeRunner{}, opts())
	if err == nil {
		t.Skip("yaegi now infers untyped floats; the hint is no longer needed")
	}
	if !strings.Contains(err.Error(), "float64(") {
		t.Errorf("err = %q, want a hint to type one argument", err)
	}
}

// Small models count lines badly. A compile or parse error quotes the line
// it names, with a caret under the column.
func TestErrorsQuoteTheLine(t *testing.T) {
	cases := map[string]struct{ src, line string }{
		"compile": {"package main\n\nfunc main() {\n\tx := 1\n\tx = x + undefinedThing\n}\n", "\tx = x + undefinedThing"},
		"parse":   {"package main\n\nfunc main() {\n\tvar v struct {\n\t\tA int `json:\"a\"\n\t}\n\t_ = v\n}\n", "\t\tA int `json:\"a\""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Run(context.Background(), c.src, &fakeRunner{}, opts())
			if err == nil {
				t.Fatal("the program was accepted")
			}
			if !strings.Contains(err.Error(), "| "+c.line+"\n") {
				t.Errorf("err does not quote %q:\n%s", c.line, err)
			}
			if !strings.Contains(err.Error(), "^") {
				t.Errorf("err has no caret:\n%s", err)
			}
		})
	}
}

func TestQuoteLine(t *testing.T) {
	src := "package main\n\nfunc main() {\n\tx = 1\n}\n"
	got := quoteLine(src, "4:2: undefined: x")
	want := "4:2: undefined: x\n    4 | \tx = 1\n      | \t^"
	if got != want {
		t.Errorf("quoteLine =\n%s\nwant\n%s", got, want)
	}
	for _, msg := range []string{"no position here", "99:1: past the end", "main.go:0:1: line zero"} {
		if got := quoteLine(src, msg); got != msg {
			t.Errorf("quoteLine(%q) = %q, want it unchanged", msg, got)
		}
	}
	if got := quoteLine(src, "main.go:4:2: x"); !strings.Contains(got, "| \tx = 1") {
		t.Errorf("a main.go: prefix is not understood: %q", got)
	}
}
