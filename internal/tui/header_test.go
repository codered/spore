package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

func statusLine(m *Model) string {
	lines := strings.Split(ansi.Strip(m.View()), "\n")
	return lines[len(lines)-1]
}

func TestFitHintsDropsFromTheEndButKeepsTheLast(t *testing.T) {
	parts := []string{"aa", "bb", "cc", "? help"}
	if got := ansi.Strip(fitHints(parts, 100)); got != "aa  bb  cc  ? help" {
		t.Fatalf("roomy: %q", got)
	}
	if got := ansi.Strip(fitHints(parts, 12)); got != "aa  ? help" {
		t.Fatalf("tight: %q", got)
	}
	if got := ansi.Strip(fitHints(parts, 1)); got != "? help" {
		t.Fatalf("no room: %q", got)
	}
}

func TestStatusHintsKeepHelpAtEveryWidth(t *testing.T) {
	m := scene(t, 100, 24)
	press(m, "esc")
	for w := 60; w <= 200; w++ {
		run(m, tea.WindowSizeMsg{Width: w, Height: 24})
		line := statusLine(m)
		if !strings.Contains(line, "? help") {
			t.Fatalf("width %d lost ? help: %q", w, line)
		}
		if ansi.StringWidth(line) > w {
			t.Fatalf("width %d: status is %d wide", w, ansi.StringWidth(line))
		}
	}
}

func TestHintsHaveNoBracketsAndOfferNextBlocked(t *testing.T) {
	m := scene(t, 200, 24)
	press(m, "esc")
	line := statusLine(m)
	for _, want := range []string{"i type", "j/k scroll", "[ ] tools", "o expand", "n new", "b next blocked", ": cmd", "? help"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if strings.Contains(line, "<") {
		t.Fatalf("brackets left in %q", line)
	}
}

func TestDaemonLabel(t *testing.T) {
	for addr, want := range map[string]string{
		"":               "",
		"127.0.0.1:7777": "daemon :7777",
		"localhost:7777": "daemon :7777",
		"[::1]:7777":     "daemon :7777",
		":7777":          "daemon :7777",
		"10.0.0.5:7777":  "daemon 10.0.0.5:7777",
		"not an address": "daemon not an address",
	} {
		if got := daemonLabel(addr); got != want {
			t.Errorf("daemonLabel(%q) = %q, want %q", addr, got, want)
		}
	}
}

func TestHeaderShowsBlockedGlyphAndDaemon(t *testing.T) {
	m := scene(t, 160, 24)
	m.opts.Daemon = "127.0.0.1:7777"
	head := strings.Split(ansi.Strip(m.View()), "\n")[0]
	if !strings.HasSuffix(head, "◐ 1 blocked · daemon :7777") {
		t.Fatalf("header = %q", head)
	}
	run(m, tea.WindowSizeMsg{Width: 60, Height: 24})
	head = strings.Split(ansi.Strip(m.View()), "\n")[0]
	if strings.Contains(head, "daemon") || !strings.Contains(head, "◐ 1 blocked") {
		t.Fatalf("narrow header = %q", head)
	}
}
