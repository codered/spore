package tui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRefillReappliesTheBackgroundAfterEveryReset(t *testing.T) {
	got := refill("a\x1b[0mb\x1b[mc", "<bg>")
	want := "<bg>a\x1b[0m<bg>b\x1b[m<bg>c\x1b[0m"
	if got != want {
		t.Fatalf("refill = %q, want %q", got, want)
	}
	if refill("abc", "") != "abc" {
		t.Fatal("an empty sequence must leave the line alone")
	}
}

func TestFillPadsToWidth(t *testing.T) {
	got := ansi.Strip(fill("ab", 6, colFillSel))
	if got != "ab    " {
		t.Fatalf("fill = %q", got)
	}
}
