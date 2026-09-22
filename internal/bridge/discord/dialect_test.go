package discord

import (
	"strings"
	"testing"
)

func convertAll(t *testing.T, s string) string {
	t.Helper()
	var d dialect
	return d.convert(s, true)
}

func TestDialectLeavesOrdinaryProseAlone(t *testing.T) {
	// The renderer's split test pins byte-for-byte passthrough. Anything
	// Discord already renders must survive untouched.
	in := "Hello, **world**.\n\n- a bullet\n- another\n\n# Heading\n## Deeper\n### Deepest\n\n`code` and a [link](http://x)\n"
	if got := convertAll(t, in); got != in {
		t.Fatalf("prose was rewritten:\n got %q\nwant %q", got, in)
	}
}

func TestDialectRewritesATableAsAnAlignedCodeBlock(t *testing.T) {
	in := "Week ahead:\n\n" +
		"| Day | High / Low | Conditions |\n" +
		"|---|---|---|\n" +
		"| Mon Sep 21 | 71° / 58° | Mostly sunny |\n" +
		"| Wed Sep 23 | 85° / 55° | Sunny |\n" +
		"\nA few notes:\n"
	got := convertAll(t, in)

	if strings.Contains(got, "|---|") {
		t.Fatalf("the delimiter row survived:\n%s", got)
	}
	if strings.Count(got, "```") != 2 {
		t.Fatalf("table is not wrapped in exactly one fenced block:\n%s", got)
	}
	if !strings.HasPrefix(got, "Week ahead:\n\n```\n") {
		t.Fatalf("prose before the table was disturbed:\n%s", got)
	}
	if !strings.HasSuffix(got, "```\n\nA few notes:\n") {
		t.Fatalf("prose after the table was disturbed:\n%q", got)
	}

	// Columns must line up: every body row's cells start at the same rune
	// offset, which is the whole point of the code block.
	var rows []string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "Day") || strings.HasPrefix(l, "Mon") || strings.HasPrefix(l, "Wed") {
			rows = append(rows, l)
		}
	}
	if len(rows) != 3 {
		t.Fatalf("want a header and two body rows, got %d:\n%s", len(rows), got)
	}
	for _, r := range rows[1:] {
		if idx := strings.Index(r, "71°"); idx >= 0 {
			if idx != strings.Index(rows[0], "High") {
				t.Fatalf("column 2 is not aligned:\n%s", got)
			}
		}
		if idx := strings.Index(r, "85°"); idx >= 0 {
			if idx != strings.Index(rows[0], "High") {
				t.Fatalf("column 2 is not aligned:\n%s", got)
			}
		}
	}
}

func TestDialectLeavesPipeTextThatIsNotATableAlone(t *testing.T) {
	// No delimiter row means it is not a table; rewriting it would mangle
	// ordinary prose that happens to contain pipes.
	in := "| this is just | a line\n| and another\n"
	if got := convertAll(t, in); got != in {
		t.Fatalf("non-table pipe text was rewritten:\n got %q\nwant %q", got, in)
	}
}

func TestDialectDemotesHeadingsDiscordCannotRender(t *testing.T) {
	in := "### kept\n#### demoted\n##### also demoted\n"
	want := "### kept\n**demoted**\n**also demoted**\n"
	if got := convertAll(t, in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDialectDropsHorizontalRules(t *testing.T) {
	in := "above\n\n---\n\nbelow\n***\n___\nend\n"
	want := "above\n\nbelow\nend\n"
	if got := convertAll(t, in); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDialectLeavesFencedContentAlone(t *testing.T) {
	// A markdown table inside a code block is a code sample, not a table,
	// and #### inside one is a comment.
	in := "```\n| a | b |\n|---|---|\n| 1 | 2 |\n#### not a heading\n---\n```\nafter\n"
	if got := convertAll(t, in); got != in {
		t.Fatalf("fenced content was rewritten:\n got %q\nwant %q", got, in)
	}
}

func TestDialectHoldsAnIncompleteTable(t *testing.T) {
	// Streaming: a table arrives across flushes. Rewriting half of one puts
	// a broken code block on screen that the next edit cannot repair, so the
	// rows must be withheld until the table ends.
	var d dialect
	const s = "Week ahead:\n| Day | Cond |\n|---|---|\n| Mon | Sunny |\n"
	h := d.hold(s)
	if h == 0 {
		t.Fatal("an unfinished table was not held back")
	}
	emitted := d.convert(s[:len(s)-h], false)
	if strings.Contains(emitted, "|") || strings.Contains(emitted, "```") {
		t.Fatalf("part of the table reached the screen: %q", emitted)
	}
	if emitted != "Week ahead:\n" {
		t.Fatalf("emitted %q, want only the prose above the table", emitted)
	}
}

func TestDialectDoesNotHoldOrdinaryProse(t *testing.T) {
	var d dialect
	for _, s := range []string{"Hello, ", "a paragraph with no newline at all", "done.\n"} {
		if h := d.hold(s); h != 0 {
			t.Fatalf("prose %q was held back (%d bytes); streaming would stall", s, h)
		}
	}
}

func TestDialectGivesUpOnAnEnormousHeldRun(t *testing.T) {
	// A hold that grows without bound would starve the screen and defeat
	// the message-limit split, so it must yield.
	var d dialect
	s := "| " + strings.Repeat("x", 4000) + " |\n"
	if h := d.hold(s); h != 0 {
		t.Fatalf("hold of %d bytes is unbounded", h)
	}
}
