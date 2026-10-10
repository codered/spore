package companion

import (
	"strings"
	"testing"
)

func TestAppendSelfNoteStartsFromTheTemplate(t *testing.T) {
	got, err := AppendSelfNote("", "Threads with you", "deciding whether to sell ZS before earnings")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range SelfHeadings {
		if !strings.Contains(got, "## "+h+"\n") {
			t.Errorf("missing heading %q in:\n%s", h, got)
		}
	}
	threads := strings.Index(got, "## Threads with you")
	note := strings.Index(got, "- deciding whether to sell ZS before earnings\n")
	next := strings.Index(got, "## How you like to be talked to")
	if threads >= note || note >= next {
		t.Fatalf("note not under its heading:\n%s", got)
	}
}

func TestAppendSelfNoteAppendsAtTheEndOfItsSection(t *testing.T) {
	body, _ := AppendSelfNote("", "What I'm curious about", "first")
	body, _ = AppendSelfNote(body, "What I'm curious about", "second")
	first := strings.Index(body, "- first")
	second := strings.Index(body, "- second")
	next := strings.Index(body, "## Threads with you")
	if first >= second || second >= next {
		t.Fatalf("order wrong:\n%s", body)
	}
}

func TestAppendSelfNoteAddsAMissingHeading(t *testing.T) {
	hand := "Some notes the user wrote by hand.\n"
	got, err := AppendSelfNote(hand, "Opinions I've formed", "tabs are fine")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, hand) || !strings.HasSuffix(got, "## Opinions I've formed\n\n- tabs are fine\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestAppendSelfNoteRejectsUnknownHeadingAndEmptyText(t *testing.T) {
	if _, err := AppendSelfNote("", "Secrets", "x"); err == nil {
		t.Error("accepted an unknown heading")
	}
	if _, err := AppendSelfNote("", "Threads with you", "  \n "); err == nil {
		t.Error("accepted empty text")
	}
}

func TestAppendSelfNoteCollapsesNewlines(t *testing.T) {
	got, _ := AppendSelfNote("", "Threads with you", "line one\n## Fake heading")
	if strings.Contains(got, "\n## Fake heading") {
		t.Fatalf("a note injected a heading:\n%s", got)
	}
}
