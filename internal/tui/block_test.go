package tui

import (
	"os"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	// Pin a markdown style that does not ask the terminal anything: in a
	// test nothing answers, and the query would wait out its timeout.
	mdStyleName = func() string { return "notty" }
	os.Exit(m.Run())
}

func TestSplitStableCutsAtTheLastBlankLineOutsideAFence(t *testing.T) {
	cases := []struct{ in, stable, tail string }{
		{"one line, still typing", "", "one line, still typing"},
		{"para one\n\nhalf a", "para one\n\n", "half a"},
		{"a\n\nb\n\nc", "a\n\nb\n\n", "c"},
		// A blank line inside an unclosed fence is not a boundary.
		{"intro\n\n```go\nfunc a() {\n\n", "intro\n\n", "```go\nfunc a() {\n\n"},
		// Once the fence closes, the blank line after it is.
		{"```\ncode\n```\n\nafter", "```\ncode\n```\n\n", "after"},
	}
	for _, c := range cases {
		stable, tail := splitStable(c.in)
		if stable != c.stable || tail != c.tail {
			t.Errorf("splitStable(%q) = (%q, %q), want (%q, %q)", c.in, stable, tail, c.stable, c.tail)
		}
	}
}

func TestACollapsedToolIsOneLine(t *testing.T) {
	b := &block{kind: kindTool, tool: "bash", args: `{"cmd":"go test ./..."}`,
		result: "exit 1\nFAIL cmd/spore", isError: true, done: true}
	out := b.render(80, nil, false)
	if strings.Contains(out, "\n") {
		t.Fatalf("collapsed tool spans lines:\n%s", out)
	}
	for _, want := range []string{"bash", "go test", "✗", "exit 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("collapsed tool %q is missing %q", out, want)
		}
	}
}

func TestAnExpandedToolShowsItsArgumentsAndResult(t *testing.T) {
	b := &block{kind: kindTool, tool: "bash", args: `{"cmd":"go test ./..."}`,
		result: "exit 1\nFAIL cmd/spore", isError: true, done: true, expanded: true}
	out := b.render(80, nil, false)
	for _, want := range []string{`"cmd"`, "FAIL cmd/spore"} {
		if !strings.Contains(out, want) {
			t.Errorf("expanded tool is missing %q:\n%s", want, out)
		}
	}
}

func TestARunningToolShowsNoResult(t *testing.T) {
	b := &block{kind: kindTool, tool: "read", args: `{"path":"a.go"}`}
	if out := b.render(80, nil, false); !strings.Contains(out, "…") || strings.Contains(out, "✓") {
		t.Fatalf("running tool = %q, want a pending marker and no result", out)
	}
}

func TestRenderIsCachedUntilTouched(t *testing.T) {
	b := &block{kind: kindNotice, text: "first"}
	one := b.render(80, nil, false)
	b.text = "second" // changed without touch: the cache must still answer
	if two := b.render(80, nil, false); two != one {
		t.Fatalf("render without touch = %q, want the cached %q", two, one)
	}
	b.touch()
	if three := b.render(80, nil, false); !strings.Contains(three, "second") {
		t.Fatalf("render after touch = %q, want the new text", three)
	}
	if wide := b.render(120, nil, false); !strings.Contains(wide, "second") {
		t.Fatalf("a new width must re-render, got %q", wide)
	}
}

func TestStreamingTextShowsItsTail(t *testing.T) {
	b := &block{kind: kindText, text: "para one\n\nhalf a", streaming: true}
	out := b.render(80, newMarkdown(80), false)
	for _, want := range []string{"para one", "half a"} {
		if !strings.Contains(out, want) {
			t.Errorf("streaming render is missing %q:\n%s", want, out)
		}
	}
}
