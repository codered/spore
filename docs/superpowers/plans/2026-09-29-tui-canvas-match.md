# TUI Canvas Match Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `spore chat`'s chat and approval screens match the design canvas's TUI boards (spec items 1–11).

**Architecture:** Every change is to rendering in `internal/tui`. The model's state and message flow stay as they are. The only exceptions are the tick interval, and one new `Options.Daemon` string that `cmd/spore/chat.go` fills in. The approval moves from a box stacked above the input to a card drawn over a dimmed viewport. The working line moves from a pinned row into the end of the transcript.

**Tech Stack:** Go, Bubble Tea v1.3, lipgloss v1.1, bubbles v1.0, `charmbracelet/x/ansi`.

**Spec:** `docs/superpowers/specs/2026-09-29-tui-canvas-match-design.md`

## Global Constraints

- **Files:** only `internal/tui/**` and `cmd/spore/chat.go` change. No daemon, wire or route changes.
- **Dependencies:** `go.mod` and `go.sum` must not change. Do not import `termenv` or anything new.
- **Colours** (dark / light), exactly:
  - `colMuted` `#8A9792` / `#6B7280`
  - `colFillSel` `#1D2A26` / `#DDEFE7`
  - `colFillWarn` `#2A2418` / `#FBEBD3`
  - `colFillCursor` `#1A2320` / `#ECF1EF`
  - `colVisor` `#67E8F9` / `#0E7490`
- **Key hints** have no angle brackets: `i type`, not `<i> type`. Hints are joined with two spaces.
- **Copy:**
  - the placeholder says `: for commands`, never `/ for commands`;
  - the approval footer never says `also shown on web and Discord`;
  - the status bar never says `other keys do nothing`.
- **Build tags:** vet and test use `-tags sqlite_fts5`. Tests in `internal/tui` alone run without it.
- **Goldens:** after `-update`, read `git diff internal/tui/testdata` and confirm every changed line is one the current task makes. Never accept a golden diff you cannot explain.
- **Commits:** end every commit message with:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp
  ```

## Review Focus

1. **A 60-column terminal (no sidebar) with a long shell command:** every card row must stay inside the pane. Test in Task 7.
2. **The sub-agent selected instead of its root** while the sub-agent's approval waits: the card, the amber pane and `waiting on you` must still show. Test in Task 8.
3. **A turn stopped while a tool call has no result:** the working line must go away. The orphaned tool row keeps `…` and shows no duration. Test in Task 5.
4. **Scrolled up while a turn runs:** a spinner frame is not new output, so `↓ new` must not light up until real text arrives. Test in Task 5.
5. **The selected session is the only blocked one:** `b next blocked` must not be offered. Test in Task 8.

---

### Task 1: Palette, fills and key hints

**Files:**
- Modify: `internal/tui/style.go` (palette vars, styles, new `fill`/`refill`/`bgSeq`)
- Modify: `internal/tui/header.go` (`hint`, `keyHints`, `confirmKeys`, new `fitHints`)
- Modify: `internal/tui/view.go` (`statusView`)
- Modify: `internal/tui/app.go` (new `nextBlockedID`, `nextBlocked` uses it)
- Modify: `internal/tui/block.go` (`›` in `colVisor`)
- Test: `internal/tui/style_test.go` (create), `internal/tui/header_test.go` (create)

**Interfaces:**
- Produces:
  - `fill(line string, width int, bg lipgloss.TerminalColor) string`
  - `refill(s, seq string) string`
  - `fitHints(parts []string, room int) string`
  - `(m *Model) keyHints(room int) string`
  - `confirmParts(c *confirmState) []string`
  - `(m *Model) nextBlockedID() string`
  - colours `colFillSel`, `colFillWarn`, `colFillCursor`, `colVisor`
  - styles `styVisor`, `styModeWarn`, `styApprovalCard`, `styFaint`

- [ ] **Step 1: Write the failing tests**

Create `internal/tui/style_test.go`:

```go
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
```

Create `internal/tui/header_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'Refill|FillPads|FitHints|StatusHints|HintsHaveNo'`
Expected: build failure, `undefined: refill`, `undefined: fill` and `undefined: fitHints`.

- [ ] **Step 3: Implement the palette and fills in `style.go`**

Replace the `colMuted` line and add the new colours in the first `var (...)` block:

```go
	colAccent = lipgloss.AdaptiveColor{Light: "#0B7A6B", Dark: "#3DDC97"}
	colMuted  = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#8A9792"}
	colDanger = lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#FF6B6B"}
	colWarn   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FFB454"}
	colTool   = lipgloss.AdaptiveColor{Light: "#5B21B6", Dark: "#C4A2FF"}
	colVisor  = lipgloss.AdaptiveColor{Light: "#0E7490", Dark: "#67E8F9"}

	// Fills sit behind a whole row. They mark the row, not its content, so
	// the glyphs drawn on them keep their own colours.
	colFillSel    = lipgloss.AdaptiveColor{Light: "#DDEFE7", Dark: "#1D2A26"}
	colFillWarn   = lipgloss.AdaptiveColor{Light: "#FBEBD3", Dark: "#2A2418"}
	colFillCursor = lipgloss.AdaptiveColor{Light: "#ECF1EF", Dark: "#1A2320"}
```

In the second `var (...)` block:

- change `styTabOn` to
  `styTabOn = lipgloss.NewStyle().Foreground(colAccent).Background(colFillSel).Bold(true)`;
- add, after `styMode`:

```go
	// styModeWarn is the mode badge while an approval waits on the person.
	styModeWarn = styMode.Background(colWarn)

	styVisor = lipgloss.NewStyle().Foreground(colVisor)
	styFaint = lipgloss.NewStyle().Faint(true)
```

- add, after `styApprovalTitle`:

```go
	// styApprovalCard frames the approval drawn over the transcript.
	styApprovalCard = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colWarn).
			Padding(1, 2)
```

Append to the end of `style.go`:

```go
// fill draws line on a background of bg across width cells. Styled text
// inside line resets its colours as it ends, which would also end the
// background, so the background is put back after every reset.
func fill(line string, width int, bg lipgloss.TerminalColor) string {
	line += strings.Repeat(" ", max(0, width-ansi.StringWidth(line)))
	return refill(line, bgSeq(bg))
}

// refill opens s with seq and reopens it after every SGR reset in s.
func refill(s, seq string) string {
	if seq == "" {
		return s
	}
	s = strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m"+seq)
	s = strings.ReplaceAll(s, "\x1b[m", "\x1b[m"+seq)
	return seq + s + "\x1b[0m"
}

// bgSeq is the escape sequence that sets bg in this terminal, or empty
// when the terminal has no colour.
func bgSeq(bg lipgloss.TerminalColor) string {
	s := lipgloss.NewStyle().Background(bg).Render("\x00")
	if i := strings.IndexByte(s, 0); i > 0 {
		return s[:i]
	}
	return ""
}
```

- [ ] **Step 4: Implement the hints in `header.go`**

Replace `hint`, `keyHints` and `confirmKeys` with:

```go
func hint(key, label string) string {
	return styKey.Render(key) + " " + styMuted.Render(label)
}

// fitHints joins hints two spaces apart, dropping them from the end until
// they fit in room cells. The last hint always stays: it is the way out, or
// the way to the rest of the keys.
func fitHints(parts []string, room int) string {
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	head := parts[:len(parts)-1]
	for n := len(head); n > 0; n-- {
		s := strings.Join(append(append([]string{}, head[:n]...), last), "  ")
		if lipgloss.Width(s) <= room {
			return s
		}
	}
	return last
}

// keyHints is the status bar's left half: the keys that work in this mode,
// fitted to room cells.
func (m *Model) keyHints(room int) string {
	var parts []string
	switch m.mode {
	case modeInsert:
		parts = []string{hint("enter", "send"), hint("ctrl+j", "newline"), hint("esc", "normal")}
	case modeCommand:
		parts = []string{hint("enter", "run"), hint("tab", "complete"), hint("esc", "cancel")}
	case modeFilter:
		parts = []string{hint("enter", "keep"), hint("esc", "clear")}
	case modeConfirm:
		if m.confirm == nil {
			return ""
		}
		parts = confirmParts(m.confirm)
	default:
		parts = m.normalHints()
	}
	return fitHints(parts, room)
}

// normalHints are the NORMAL-mode keys for what is on screen.
func (m *Model) normalHints() []string {
	if m.table != nil {
		esc := "back"
		switch {
		case m.table.detail != nil:
			esc = "close"
		case m.table.filter != "":
			esc = "clear"
		}
		parts := []string{hint("enter", "open"), hint("/", "filter"), hint("s", "sort")}
		for _, a := range m.table.res.Actions() {
			parts = append(parts, hint(a.Key, a.Label))
		}
		return append(parts, hint("esc", esc))
	}
	if m.focused() == paneSidebar {
		return []string{hint("tab", "chat"), hint("j/k", "session"), hint("enter", "open"), hint("i", "type"), hint("n", "new"), hint("?", "help")}
	}
	var parts []string
	if m.sidebarOn() {
		parts = append(parts, hint("tab", "sessions"))
	}
	parts = append(parts, hint("i", "type"), hint("j/k", "scroll"), hint("[ ]", "tools"), hint("o", "expand"), hint("n", "new"))
	if m.nextBlockedID() != "" {
		parts = append(parts, hint("b", "next blocked"))
	}
	return append(parts, hint(":", "cmd"), hint("?", "help"))
}

// confirmParts are the modal's answers: y, D when it applies, and esc.
func confirmParts(c *confirmState) []string {
	yes := c.yesLabel
	if yes == "" {
		yes = "confirm"
	}
	parts := []string{hint("y", yes)}
	if c.alt != nil {
		parts = append(parts, hint("D", c.altLabel))
	}
	return append(parts, hint("esc", "cancel"))
}

// confirmKeys is the modal's answer line.
func confirmKeys(c *confirmState) string { return strings.Join(confirmParts(c), "  ") }
```

- [ ] **Step 5: Fit the status bar in `view.go`**

Replace `statusView` with:

```go
// statusView is the mode badge and the keys that work here, with the few
// signals that need the user now on the right.
func (m *Model) statusView() string {
	badge := styMode.Render(" " + m.mode.String() + " ")
	var right []string
	if m.unseen {
		right = append(right, styAccent.Render("↓ new"))
	}
	if m.mode == modeNormal && m.table == nil && m.cache.State(m.selected) != daemon.SessionIdle {
		right = append(right, styWarn.Bold(true).Render("esc stop"))
	}
	if m.viewErr != "" {
		right = append(right, styDanger.Render(m.viewErr))
	}
	if m.flash != "" {
		right = append(right, styAccent.Render(m.flash))
	}
	r := strings.Join(right, styMuted.Render(" · "))
	room := m.width - lipgloss.Width(badge) - 1 - lipgloss.Width(r) - 1
	return fitRow(badge+" "+m.keyHints(max(0, room)), r, m.width)
}
```

- [ ] **Step 6: Add `nextBlockedID` in `app.go`**

Replace `nextBlocked` with:

```go
// nextBlockedID is the next blocked session after the selected one, in
// sidebar order, or empty when no other session is blocked.
func (m *Model) nextBlockedID() string {
	ids := m.selectable()
	start := indexOf(ids, m.selected)
	for k := 1; k <= len(ids); k++ {
		id := ids[(start+k+len(ids))%len(ids)]
		if id != m.selected && m.cache.State(id) == daemon.SessionBlocked {
			return id
		}
	}
	return ""
}

func (m *Model) nextBlocked() tea.Cmd {
	if id := m.nextBlockedID(); id != "" {
		return m.selectSession(id)
	}
	return nil
}
```

- [ ] **Step 7: Colour the user marker in `block.go`**

In `draw`, change the `kindUser` case to:

```go
	case kindUser:
		return wrap.Render(styVisor.Render("› ") + b.text)
```

- [ ] **Step 8: Run the new tests and the package**

Run: `go test ./internal/tui -run 'Refill|FillPads|FitHints|StatusHints|HintsHaveNo'`
Expected: PASS.

Run: `go test ./internal/tui`
Expected: only the golden tests fail, because of the status bar text.

- [ ] **Step 9: Regenerate the goldens and read the diff**

Run: `go test ./internal/tui -run TestGolden -update && git diff --stat internal/tui/testdata && git diff internal/tui/testdata | head -80`
Expected: every changed line is a status bar line with brackets gone or hints added or dropped. Nothing else changes, because the colours are stripped from goldens.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add internal/tui
git commit -m "tui: canvas palette, row fills, and bracketless fitted key hints

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 2: Header facts and the daemon address

**Files:**
- Modify: `internal/tui/app.go` (`Options.Daemon`)
- Modify: `internal/tui/header.go` (`headerView`, `globalFacts`, new `daemonLabel`)
- Modify: `cmd/spore/chat.go:63`
- Test: `internal/tui/header_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `Options.Daemon string`
  - `daemonLabel(addr string) string`
  - `(m *Model) globalFacts(withDaemon bool) string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/header_test.go`:

```go
func TestDaemonLabel(t *testing.T) {
	for addr, want := range map[string]string{
		"":                "",
		"127.0.0.1:7777":  "daemon :7777",
		"localhost:7777":  "daemon :7777",
		"[::1]:7777":      "daemon :7777",
		":7777":           "daemon :7777",
		"10.0.0.5:7777":   "daemon 10.0.0.5:7777",
		"not an address":  "daemon not an address",
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'DaemonLabel|HeaderShows'`
Expected: build failure, `undefined: daemonLabel` and `m.opts.Daemon undefined`.

- [ ] **Step 3: Add `Options.Daemon` in `app.go`**

```go
// Options configure a Model.
type Options struct {
	ShowCost bool
	// Daemon is the daemon's address, for the header; empty hides it.
	Daemon string
	// Now is the clock; nil means time.Now. Tests pin it.
	Now func() time.Time
}
```

- [ ] **Step 4: Implement the header in `header.go`**

Add `"net"` to the imports. Replace `headerView` and `globalFacts` with:

```go
// headerView is the top nav: the brand, a tab per screen with the one on
// show lit, and on the right the signals that concern every session.
func (m *Model) headerView() string {
	right := m.globalFacts(true)
	left := m.tabBar(true)
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.width {
		// Narrow: the names matter more than the letters, which ? lists.
		left = m.tabBar(false)
	}
	if lipgloss.Width(left)+lipgloss.Width(right)+1 > m.width {
		right = m.globalFacts(false)
	}
	return fitRow(left, right, m.width)
}

// globalFacts is what the top nav reports about every session at once, and
// where the daemon is when withDaemon is set.
func (m *Model) globalFacts(withDaemon bool) string {
	var out []string
	if n := m.cache.BlockedCount(); n > 0 {
		out = append(out, styWarn.Render(fmt.Sprintf("◐ %d blocked", n)))
	}
	if m.reconnecting {
		out = append(out, styDanger.Render("reconnecting…"))
	}
	if d := daemonLabel(m.opts.Daemon); withDaemon && d != "" {
		out = append(out, styMuted.Render(d))
	}
	return strings.Join(out, styMuted.Render(" · "))
}

// daemonLabel names the daemon for the header. A loopback host goes without
// saying, so 127.0.0.1:7777 shows as :7777.
func daemonLabel(addr string) string {
	if addr == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "daemon " + addr
	}
	switch host {
	case "", "127.0.0.1", "localhost", "::1":
		return "daemon :" + port
	}
	return "daemon " + addr
}
```

- [ ] **Step 5: Wire the address in `cmd/spore/chat.go`**

Change line 63 to:

```go
	summary, err := tui.Run(ctx, tuiBackend{c: c, showCost: cfg.ShowCost}, sessionID, tui.Options{ShowCost: cfg.ShowCost, Daemon: cfg.Daemon.Addr})
```

- [ ] **Step 6: Run the tests, regenerate the goldens, and read the diff**

Run: `go test ./internal/tui -run 'DaemonLabel|HeaderShows'`
Expected: PASS.

Run: `go test ./internal/tui -run TestGolden -update && git diff internal/tui/testdata | head -60`
Expected: only header lines change, `N blocked` becoming `◐ N blocked`. The goldens have no daemon address, because `newTestModel` leaves `Daemon` empty.

Run: `go test ./internal/tui && go build -tags sqlite_fts5 ./cmd/spore`
Expected: PASS, and the binary builds.

- [ ] **Step 7: Commit**

```bash
git add internal/tui cmd/spore/chat.go
git commit -m "tui: header shows the blocked glyph and the daemon address

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 3: Sidebar fills and legend

**Files:**
- Modify: `internal/tui/sidebar.go` (`sessionRow`, `highlight`)
- Modify: `internal/tui/view.go` (`sidebarView`, new `withLegend`, `legendLine`)
- Test: `internal/tui/sidebar_test.go`

**Interfaces:**
- Consumes: `fill`, `colFillSel`, `colFillWarn` (Task 1).
- Produces: `withLegend(body string, h int) string` and `legendLine() string`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/sidebar_test.go` (add `"strings"` and `"github.com/charmbracelet/x/ansi"` to its imports if missing):

```go
func TestWithLegendPutsItOnTheLastRow(t *testing.T) {
	legend := ansi.Strip(legendLine())
	if legend != "● working  ◐ blocked  ○ idle" {
		t.Fatalf("legend = %q", legend)
	}
	got := strings.Split(ansi.Strip(withLegend("a\nb", 5)), "\n")
	if len(got) != 5 || got[0] != "a" || got[1] != "b" || got[4] != legend {
		t.Fatalf("two rows in five: %q", got)
	}
	if got := strings.Split(ansi.Strip(withLegend("", 3)), "\n"); len(got) != 3 || got[2] != legend {
		t.Fatalf("empty body: %q", got)
	}
}

func TestLegendGivesWayToRows(t *testing.T) {
	body := "a\nb\nc\nd"
	if got := withLegend(body, 5); got != body {
		t.Fatalf("no blank row left, legend still drawn: %q", got)
	}
}

func TestSidebarPaneEndsInTheLegend(t *testing.T) {
	m := scene(t, 100, 24)
	lines := strings.Split(ansi.Strip(m.sidebarView()), "\n")
	if !strings.Contains(lines[len(lines)-2], "● working  ◐ blocked  ○ idle") {
		t.Fatalf("row above the border = %q", lines[len(lines)-2])
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'Legend'`
Expected: build failure, `undefined: legendLine` and `undefined: withLegend`.

- [ ] **Step 3: Implement the fills in `sidebar.go`**

Replace `highlight` with:

```go
// highlight draws line as the cursor row when on is set.
func highlight(line string, width int, on bool) string {
	if !on {
		return line
	}
	return fill(line, width, colFillSel)
}
```

In `sessionRow`, replace the final `if selected { ... }` block with:

```go
	if selected {
		bg := colFillSel
		if c.State(r.id) == daemon.SessionBlocked {
			bg = colFillWarn
		}
		return fill(line, width, bg)
	}
	return line
```

- [ ] **Step 4: Implement the legend in `view.go`**

Replace `sidebarView` with the code below, and add the two functions after it:

```go
func (m *Model) sidebarView() string {
	h := m.paneHeight()
	body := renderSidebar(m.cache, m.cache.rows(m.showAll, m.filter, m.selected), m.selected, m.sideCursor, sidebarWidth, h)
	return paneBox("sessions", withLegend(body, h), sidebarOuter, m.bodyHeight(), m.focused() == paneSidebar, 0)
}

// withLegend puts the state legend on the last of h rows when body leaves a
// blank row above it. Otherwise the rows win and there is no legend.
func withLegend(body string, h int) string {
	if body == "" {
		return strings.Repeat("\n", max(0, h-1)) + legendLine()
	}
	n := strings.Count(body, "\n") + 1
	if n > h-2 {
		return body
	}
	return body + strings.Repeat("\n", h-n) + legendLine()
}

// legendLine says what the sidebar's state glyphs mean.
func legendLine() string {
	return styAccent.Render("●") + styMuted.Render(" working  ") +
		styWarn.Render("◐") + styMuted.Render(" blocked  ○ idle")
}
```

- [ ] **Step 5: Run the tests, regenerate the goldens, and read the diff**

Run: `go test ./internal/tui -run 'Legend'`
Expected: PASS.

Run: `go test ./internal/tui -run TestGolden -update && git diff internal/tui/testdata | head -80`
Expected: the sidebar's last row gains the legend wherever the rows leave room. Nothing else changes, because fills are colour only and goldens strip colour.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/tui
git commit -m "tui: sidebar selection as a fill, amber when blocked, and a state legend

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 4: Tool rows — columns, duration, cursor fill, expanded rule

**Files:**
- Modify: `internal/tui/block.go` (fields `startedAt`, `doneAt`; `drawTool`)
- Modify: `internal/tui/cache.go` (`Apply` sets the times)
- Modify: `internal/tui/style.go` (new `humanDur`)
- Test: `internal/tui/block_test.go`, `internal/tui/cache_test.go`

**Interfaces:**
- Consumes: `fill`, `colFillCursor` (Task 1).
- Produces:
  - `humanDur(d time.Duration) string`
  - `block.startedAt` and `block.doneAt` (`time.Time`)

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/block_test.go` (add `"strings"`, `"time"` and `"github.com/charmbracelet/x/ansi"` to its imports if missing):

```go
func TestHumanDur(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:            "0.0s",
		400 * time.Millisecond:  "0.4s",
		1300 * time.Millisecond: "1.3s",
		12 * time.Second:        "12s",
		65 * time.Second:        "1m05s",
	} {
		if got := humanDur(d); got != want {
			t.Errorf("humanDur(%v) = %q, want %q", d, got, want)
		}
	}
}

func doneTool() *block {
	return &block{kind: kindTool, tool: "bash", args: `{"command":"go test"}`, result: "ok", done: true,
		startedAt: t0, doneAt: t0.Add(1300 * time.Millisecond)}
}

func TestToolRowStatusSitsAtTheRightEdge(t *testing.T) {
	out := ansi.Strip(doneTool().drawTool(60, false))
	if ansi.StringWidth(out) != 60 {
		t.Fatalf("row is %d wide, want 60: %q", ansi.StringWidth(out), out)
	}
	if !strings.HasSuffix(out, "✓ ok  1.3s") || !strings.HasPrefix(out, "▸ bash ") {
		t.Fatalf("row = %q", out)
	}
}

func TestCursorToolRowIsFullWidth(t *testing.T) {
	out := ansi.Strip(doneTool().drawTool(60, true))
	if !strings.HasPrefix(out, "▶ bash") || ansi.StringWidth(out) != 60 {
		t.Fatalf("cursor row = %q", out)
	}
}

func TestExpandedToolHasARule(t *testing.T) {
	b := doneTool()
	b.expanded = true
	lines := strings.Split(ansi.Strip(b.drawTool(60, false)), "\n")
	if !strings.HasPrefix(lines[0], "▾ bash") {
		t.Fatalf("head = %q", lines[0])
	}
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "  │ ") {
			t.Fatalf("body line without the rule: %q", l)
		}
	}
}

func TestStoredToolRowHasNoDuration(t *testing.T) {
	b := doneTool()
	b.startedAt, b.doneAt = time.Time{}, time.Time{}
	if out := ansi.Strip(b.drawTool(60, false)); !strings.HasSuffix(out, "✓ ok") {
		t.Fatalf("row = %q", out)
	}
}
```

Append to `internal/tui/cache_test.go`:

```go
func TestToolTimesComeFromEvents(t *testing.T) {
	now := t0
	c := newCache(func() time.Time { return now })
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	now = now.Add(1300 * time.Millisecond)
	c.Apply(daemon.WireEvent{Session: "s", Type: daemon.WireToolResult, ToolUseID: "t1", Content: "ok"})
	b := c.get("s").tool("t1")
	if got := b.doneAt.Sub(b.startedAt); got != 1300*time.Millisecond {
		t.Fatalf("duration = %v", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'HumanDur|ToolRow|CursorToolRow|ExpandedTool|StoredToolRow|ToolTimes'`
Expected: build failure, `undefined: humanDur` and `unknown field startedAt`.

- [ ] **Step 3: Add `humanDur` to `style.go`**

Add `"time"` to the imports and append:

```go
// humanDur is a short elapsed time: 0.4s, 12s, 1m05s.
func humanDur(d time.Duration) string {
	d = max(0, d)
	switch {
	case d < 10*time.Second:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	s := int(d.Seconds())
	return fmt.Sprintf("%dm%02ds", s/60, s%60)
}
```

- [ ] **Step 4: Add the times to `block` and `cache.Apply`**

In `block.go`, add `"time"` to the imports, and in the `// kindTool` field group after `expanded bool` add:

```go
	// startedAt and doneAt time the call when it ran live. A call rebuilt
	// from a stored transcript has neither and shows no duration.
	startedAt time.Time
	doneAt    time.Time
```

In `cache.go` `Apply`, change the `WireToolCall` case to

```go
	case daemon.WireToolCall:
		sv.endStreaming()
		sv.blocks = append(sv.blocks, &block{kind: kindTool, toolID: ev.ToolUseID, tool: ev.Tool, args: ev.Args, startedAt: c.now()})
```

and in the `WireToolResult` case change the assignment line to:

```go
		b.result, b.isError, b.truncated, b.done = ev.Content, ev.IsError, ev.Truncated, true
		b.doneAt = c.now()
		b.touch()
```

- [ ] **Step 5: Rewrite `drawTool` in `block.go`**

```go
// drawTool is one line collapsed -- `▸ bash go test ./…      ✗ exit 1  1.3s`,
// with the status against the right edge -- and the full arguments and
// result under a rule when expanded.
func (b *block) drawTool(width int, selected bool) string {
	marker := "▸ "
	if b.expanded {
		marker = "▾ "
	}
	if selected {
		marker = "▶ "
	}
	status := styMuted.Render("…")
	if b.done {
		if b.isError {
			status = styDanger.Render("✗ " + clip(oneLine(firstLineOf(b.result)), 40))
		} else {
			status = styAccent.Render("✓ ") + styMuted.Render(summarise(b.result, b.truncated))
		}
		if !b.startedAt.IsZero() && !b.doneAt.IsZero() {
			status += "  " + styMuted.Render(humanDur(b.doneAt.Sub(b.startedAt)))
		}
	}
	head := styTool.Render(marker + b.tool)
	args := clip(oneLine(b.args), width-lipgloss.Width(head)-lipgloss.Width(status)-3)
	gap := max(2, width-lipgloss.Width(head)-1-lipgloss.Width(args)-lipgloss.Width(status))
	line := head + " " + styMuted.Render(args) + strings.Repeat(" ", gap) + status
	if selected {
		line = fill(line, width, colFillCursor)
	}
	if !b.expanded {
		return line
	}
	body := prettyArgs(b.args, 40)
	if b.done {
		body += "\n" + clipLines(b.result, 200)
	}
	wrapped := lipgloss.NewStyle().Width(max(10, width-4)).Render(body)
	return line + "\n" + indent(wrapped, "  "+styMuted.Render("│")+" ")
}
```

- [ ] **Step 6: Run the tests, regenerate the goldens, and read the diff**

Run: `go test ./internal/tui -run 'HumanDur|ToolRow|CursorToolRow|ExpandedTool|StoredToolRow|ToolTimes'`
Expected: PASS.

Run: `go test ./internal/tui -run TestGolden -update && git diff internal/tui/testdata | head -80`
Expected: tool rows now end at the pane's right edge, `tool-expanded-100` shows `▾` and the `│` rule, and nothing else changes.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/tui
git commit -m "tui: tool rows in columns with durations, a cursor fill, and an expanded rule

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 5: Working line in the transcript, and the frame tick

**Files:**
- Modify: `internal/tui/view.go` (`sync`, `mainView`; new `workingLine`; delete `thinkingView`, `thinkingHeight` and `elapsed`)
- Modify: `internal/tui/app.go` (tick constants, `tick` field, `tickMsg`, `armTick`)
- Modify: `internal/tui/app_test.go:192` (stub)
- Modify: `internal/tui/parity_test.go` (drop the working-row tests)
- Create: `internal/tui/working_test.go`
- Test: `internal/tui/view_test.go` (golden `working-100`)

**Interfaces:**
- Consumes:
  - `humanDur` (Task 4);
  - `waitingTool(sv *sessionView) string`, which already exists in `view.go`. Keep it.
- Produces:
  - `(m *Model) workingLine() string`
  - `Model.tick func(d time.Duration) tea.Cmd`
  - `tickMsg struct{}`
  - `frameEvery = 120 * time.Millisecond` and `secondEvery = time.Second`
  - `Model.lastBase string`

- [ ] **Step 1: Replace the working-row tests**

Delete these from `internal/tui/parity_test.go`:
- `clockModel`;
- `thinking`;
- `TestThinkingRowFollowsTheTurn`;
- `TestANewSendDoesNotShowTheLastTurnsTime`;
- `TestTheSecondTickRunsOnlyWhileSomethingCounts`.

Replace every `secondTickMsg{}` left in the file with `tickMsg{}`, and remove any import the compiler then reports unused.

In `internal/tui/app_test.go`, replace the line `m.secondTick = func() tea.Cmd { return nil }` with:

```go
	m.tick = func(time.Duration) tea.Cmd { return nil }
```

Create `internal/tui/working_test.go`:

```go
package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// clockModel is a test model whose clock the test moves by hand. It records
// the interval of every tick it arms instead of sleeping.
func clockModel(t *testing.T) (*Model, *time.Time, *[]time.Duration) {
	t.Helper()
	m := newTestModel(t, &fakeBackend{}, "s1")
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	var armed []time.Duration
	m.tick = func(d time.Duration) tea.Cmd { armed = append(armed, d); return nil }
	return m, &now, &armed
}

// working is the working line without its spinner frame, which turns with
// the clock.
func working(m *Model) string {
	s := []rune(ansi.Strip(m.workingLine()))
	if len(s) < 2 {
		return string(s)
	}
	return string(s[2:])
}

func TestWorkingLineFollowsTheTurn(t *testing.T) {
	m, now, _ := clockModel(t)
	if got := working(m); got != "" {
		t.Fatalf("idle session shows %q", got)
	}
	run(m, keyMsg("h"))
	run(m, keyMsg("i"))
	run(m, keyMsg("enter"))
	if got := working(m); got != "thinking" {
		t.Fatalf("after send: %q", got)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(12 * time.Second)
	run(m, tickMsg{})
	if got := working(m); got != "thinking · 12s" {
		t.Fatalf("after 12s: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "hel"})
	if got := working(m); got != "writing · 12s" {
		t.Fatalf("while text streams: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	if got := working(m); got != "running bash · 12s" {
		t.Fatalf("while a tool waits: %q", got)
	}
	*now = now.Add(63 * time.Second)
	run(m, tickMsg{})
	if got := working(m); got != "running bash · 1m15s" {
		t.Fatalf("past a minute: %q", got)
	}
	if !strings.Contains(ansi.Strip(m.lastContent), "running bash · 1m15s") {
		t.Fatal("the working line is not in the transcript")
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "bash"})
	if got := working(m); got != "" {
		t.Fatalf("while blocked: %q", got)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireResolved, PendingID: 1, Decision: "allow"})
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolResult, ToolUseID: "t1", Content: "ok"})
	if got := working(m); got != "thinking · 1m15s" {
		t.Fatalf("after the tool returns: %q", got)
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	if got := working(m); got != "" {
		t.Fatalf("after the turn: %q", got)
	}
	if strings.Contains(ansi.Strip(m.lastContent), "thinking") {
		t.Fatal("the working line outlived the turn")
	}
}

func TestANewSendDoesNotShowTheLastTurnsTime(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	*now = now.Add(30 * time.Second)
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, keyMsg("x"))
	run(m, keyMsg("enter"))
	if got := working(m); got != "thinking" {
		t.Fatalf("second send: %q", got)
	}
}

func TestStoppedMidToolClearsTheWorkingLine(t *testing.T) {
	m, _, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "bash"})
	feed(m, wev("s1", daemon.WireStopped))
	if got := working(m); got != "" {
		t.Fatalf("after stop: %q", got)
	}
	row := ansi.Strip(m.cache.get("s1").tool("t1").drawTool(60, false))
	if !strings.HasSuffix(row, "…") {
		t.Fatalf("orphaned tool row = %q", row)
	}
}

func TestSpinnerTurnsWithTheClock(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	a := []rune(ansi.Strip(m.workingLine()))[0]
	*now = now.Add(frameEvery)
	b := []rune(ansi.Strip(m.workingLine()))[0]
	if a == b {
		t.Fatalf("frame %q did not turn", string(a))
	}
}

func TestAFrameIsNotNewOutput(t *testing.T) {
	m, now, _ := clockModel(t)
	feed(m, wev("s1", daemon.WireTurnStarted))
	m.follow, m.unseen = false, false
	*now = now.Add(frameEvery)
	run(m, tickMsg{})
	if m.unseen {
		t.Fatal("a spinner frame lit ↓ new")
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "real output"})
	if !m.unseen {
		t.Fatal("real output did not light ↓ new")
	}
}

func TestTickIntervalFollowsWhatIsOnScreen(t *testing.T) {
	m, _, armed := clockModel(t)
	if len(*armed) != 0 {
		t.Fatalf("idle model armed %v", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireText, Text: "x"})
	if len(*armed) != 1 || (*armed)[0] != frameEvery {
		t.Fatalf("while working: %v, want one %v", *armed, frameEvery)
	}
	run(m, tickMsg{})
	if len(*armed) != 2 {
		t.Fatalf("tick did not re-arm while working: %v", *armed)
	}
	feed(m, wev("s1", daemon.WireTurnDone))
	run(m, tickMsg{})
	if len(*armed) != 2 {
		t.Fatalf("tick re-armed after the turn: %v", *armed)
	}
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell"})
	if len(*armed) != 3 || (*armed)[2] != secondEvery {
		t.Fatalf("approval only: %v, want a %v tick", *armed, secondEvery)
	}
}

func BenchmarkSyncLongTranscript(b *testing.B) {
	m := New(context.Background(), &fakeBackend{}, "s1", Options{})
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	m.tick = func(time.Duration) tea.Cmd { return nil }
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	sv := m.cache.get("s1")
	for i := 0; i < 1000; i++ {
		sv.blocks = append(sv.blocks,
			&block{kind: kindText, text: fmt.Sprintf("paragraph %d with **some** markdown and `code`", i)},
			&block{kind: kindTool, toolID: fmt.Sprint(i), tool: "bash", args: `{"command":"go test ./..."}`, result: "ok", done: true})
	}
	sv.working, sv.started = true, t0
	m.sync() // renders every block once; later frames hit each block's cache
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now = now.Add(frameEvery)
		m.sync()
	}
}
```

Append to `internal/tui/view_test.go`:

```go
func TestGoldenWorking(t *testing.T) {
	m := scene(t, 100, 24)
	now := t0
	clock := func() time.Time { return now }
	m.opts.Now, m.cache.now = clock, clock
	feed(m, wev("a1b2c3", daemon.WireTurnStarted))
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireToolCall, ToolUseID: "t3", Tool: "fs_read", Args: `{"path":"internal/tui/app.go"}`})
	now = now.Add(1300 * time.Millisecond)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireToolResult, ToolUseID: "t3", Content: "package tui\n\nimport (\n"})
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireText, Text: "The flake is a race"})
	now = now.Add(12 * time.Second)
	run(m, tickMsg{})
	golden(t, "working-100", m.View())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'Working|NewSend|StoppedMid|Spinner|AFrame|TickInterval'`
Expected: build failure, `undefined: tickMsg`, `m.tick undefined` and `m.workingLine undefined`.

- [ ] **Step 3: Replace the tick in `app.go`**

In the `const` block, replace the `secondEvery` lines with:

```go
	// frameEvery turns the working line's spinner. secondEvery redraws an
	// approval's countdown when nothing else on screen moves.
	frameEvery  = 120 * time.Millisecond
	secondEvery = time.Second
```

In the message types, replace `secondTickMsg struct{}` with `tickMsg struct{}`.

In `Model`, replace the `ticking`/`secondTick` fields with:

```go
	// ticking is true while a tick is in flight; see armTick.
	ticking bool
	tick    func(d time.Duration) tea.Cmd
	// lastBase is the transcript without its working line, so a spinner
	// frame is not mistaken for new output.
	lastBase string
```

In `New`, replace the `m.secondTick = ...` assignment with:

```go
	m.tick = func(d time.Duration) tea.Cmd {
		return tea.Tick(d, func(time.Time) tea.Msg { return tickMsg{} })
	}
```

Replace `Update` and `armSecond` with:

```go
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.sync()
	if t := m.armTick(); t != nil {
		cmd = tea.Batch(cmd, t)
	}
	return m, cmd
}

// armTick starts the redraw tick while something on screen moves with time:
// the working line's spinner every frame, or an approval's countdown every
// second. At most one tick is in flight, and it lapses once nothing moves.
func (m *Model) armTick() tea.Cmd {
	if m.ticking || m.table != nil || m.selected == "" || m.cache.State(m.selected) == daemon.SessionIdle {
		return nil
	}
	m.ticking = true
	d := secondEvery
	if m.workingLine() != "" {
		d = frameEvery
	}
	return m.tick(d)
}
```

In `update`, change `case secondTickMsg:` to `case tickMsg:`. Its body stays `m.ticking = false; return nil`.

- [ ] **Step 4: Move the working line into the transcript in `view.go`**

Delete `thinkingView`, `thinkingHeight` and `elapsed`. Keep `waitingTool`.

In `sync`:
- change the viewport-height line to
  `m.vp.Height = max(1, m.paneHeight()-1-lipgloss.Height(m.inputView())-m.overlayHeight())`;
- replace the block from `content := m.transcript(w)` to its closing `}` with:

```go
	base := m.transcript(w)
	content := base
	if wl := m.workingLine(); wl != "" {
		if base != "" {
			content += "\n\n"
		}
		content += wl
	}
	if content != m.lastContent {
		m.vp.SetContent(content)
		// A spinner frame changes content but is not something new to read.
		if base != m.lastBase && !m.follow {
			m.unseen = true
		}
		m.lastContent, m.lastBase = content, base
	}
```

In `mainView`, delete the three lines that append `m.thinkingView()`.

Add, after `waitingTool`:

```go
// spinnerFrames turn once per frameEvery while a turn runs.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// workingLine ends the transcript while the selected session's turn runs:
// a spinner, what spore is doing, and for how long. It hides while an
// approval waits: then spore is waiting on the person, and the card says so.
func (m *Model) workingLine() string {
	if m.selected == "" || m.cache.State(m.selected) != daemon.SessionWorking {
		return ""
	}
	sv := m.cache.get(m.selected)
	label := "thinking"
	if b := sv.last(); b != nil && b.kind == kindText && b.streaming {
		label = "writing"
	} else if tool := waitingTool(sv); tool != "" {
		label = "running " + tool
	}
	now := m.opts.Now()
	if !sv.started.IsZero() {
		label += " · " + humanDur(now.Sub(sv.started))
	}
	frame := spinnerFrames[int(now.UnixMilli()/frameEvery.Milliseconds())%len(spinnerFrames)]
	return styAccent.Render(frame) + " " + styMuted.Render(label)
}
```

If the compiler reports `fmt` or `time` unused in `view.go`, remove them. `deadline` still uses both, so they should stay.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/tui -run 'Working|NewSend|StoppedMid|Spinner|AFrame|TickInterval|Approval'`
Expected: PASS.

- [ ] **Step 6: Run the benchmark and apply the rule**

Run: `go test ./internal/tui -run '^$' -bench SyncLongTranscript -benchtime 200x`
Look at `ns/op`.
- If it is **2000000 or less**, change nothing.
- If it is **above 2000000**, change `frameEvery` to `time.Second` in `app.go`. The tests refer to `frameEvery` by name, so they keep passing. Record the figure in the commit message either way.

- [ ] **Step 7: Regenerate the goldens and read the diff**

Run: `go test ./internal/tui -run TestGolden -update && git diff --stat internal/tui/testdata && cat internal/tui/testdata/working-100.golden`
Expected:
- `working-100.golden` is new. Its transcript ends in a blank line, then `<frame> writing · 13s`.
- The `fs_read` row reads `✓ 3 lines  1.3s` against the right edge.
- No other golden changes.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tui
git commit -m "tui: working line with a spinner at the end of the transcript

Replaces the pinned row. The tick runs every frame while the line shows
and every second while only a countdown does. sync on a 2,000-block
transcript: <ns/op from step 6>.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 6: Input placeholder per mode

**Files:**
- Modify: `internal/tui/app.go` (constants, `New`)
- Modify: `internal/tui/view.go` (`sync`)
- Test: `internal/tui/view_test.go`

**Interfaces:**
- Produces: `placeholderInsert` and `placeholderNormal` (string constants).

The spec asks for the bracketed part of the placeholder in a dimmer muted. The bubbles textarea styles its placeholder as one string, so the whole placeholder is in the placeholder style. This is a deliberate simplification; say so in the commit.

- [ ] **Step 1: Write the failing test**

Append to `internal/tui/view_test.go`:

```go
func TestPlaceholderFollowsTheMode(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Ask spore something…") || strings.Contains(v, "(i to type") {
		t.Fatalf("INSERT placeholder wrong:\n%s", v)
	}
	press(m, "esc")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Ask spore something…  (i to type · : for commands)") {
		t.Fatalf("NORMAL placeholder wrong:\n%s", v)
	}
}
```

Add `"strings"` to `view_test.go`'s imports if it is missing.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tui -run PlaceholderFollows`
Expected: FAIL, `NORMAL placeholder wrong`.

- [ ] **Step 3: Implement**

In `app.go`, add to the `const` block:

```go
	placeholderInsert = "Ask spore something…"
	placeholderNormal = "Ask spore something…  (i to type · : for commands)"
```

In `New`, change `in.Placeholder = "Ask spore something…"` to `in.Placeholder = placeholderInsert`.

In `view.go` `sync`, add as the first lines of the function:

```go
	if m.mode == modeInsert {
		m.input.Placeholder = placeholderInsert
	} else {
		m.input.Placeholder = placeholderNormal
	}
```

- [ ] **Step 4: Run the test, regenerate the goldens, and read the diff**

Run: `go test ./internal/tui -run PlaceholderFollows`
Expected: PASS.

Run: `go test ./internal/tui -run TestGolden -update && git diff internal/tui/testdata | head -60`
Expected: only the input box line changes, and only in goldens taken in NORMAL, COMMAND, FILTER or CONFIRM mode.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "tui: NORMAL placeholder says how to type and run a command

The textarea styles its placeholder as one string, so the hint is not
dimmer than the prompt as the spec drew it.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 7: Approval card over a dimmed transcript, and the draft line

**Files:**
- Create: `internal/tui/approval.go`
- Modify: `internal/tui/view.go` (`sync`, `mainView`; delete `overlayView`, `overlayHeight` and `deadline`)
- Delete: `internal/tui/parity_test.go` (its approval tests move)
- Create: `internal/tui/approval_test.go`

**Interfaces:**
- Consumes:
  - `fill`, `colFillCursor`, `styApprovalCard`, `styFaint` (Task 1);
  - `clockModel` and `tickMsg` (Task 5).
- Produces:
  - `(m *Model) waiting() bool`
  - `(m *Model) approvalCard(w, h int) string`
  - `callBox(tool, args string, maxRows, w int) []string`
  - `originLine(ev daemon.WireEvent) string`
  - `ruleLine(rule string) string`
  - `(m *Model) deadline(expires string) string`
  - `faint(s string) string`
  - `(m *Model) draftLine() string`
  - `(m *Model) draftLineHeight() int`

- [ ] **Step 1: Write the failing tests**

Delete `internal/tui/parity_test.go`. Create `internal/tui/approval_test.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

func card(m *Model) string { return ansi.Strip(m.approvalCard(100, 40)) }

func approvalOn(m *Model, ev daemon.WireEvent) {
	ev.Session, ev.Type = "s1", daemon.WireApproval
	if ev.PendingID == 0 {
		ev.PendingID = 1
	}
	feed(m, ev)
}

func TestApprovalCardShowsProfileRuleCallAndCountdown(t *testing.T) {
	m, now, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Profile: "local",
		Args: `{"command":"go test ./..."}`, ExpiresAt: t0.Add(2*time.Minute + 5*time.Second).Format(time.RFC3339)})
	got := card(m)
	for _, want := range []string{"spore wants to run shell_exec", "profile local", "matched ask  shell_exec",
		"$ go test ./...", "y allow once", "n deny", "s allow shell_exec this session", "auto-deny in 2:05"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	*now = now.Add(2 * time.Minute)
	run(m, tickMsg{})
	if got := card(m); !strings.Contains(got, "auto-deny in 0:05") {
		t.Fatalf("after 2m:\n%s", got)
	}
	*now = now.Add(10 * time.Second)
	if got := card(m); !strings.Contains(got, "auto-denying…") {
		t.Fatalf("past the deadline:\n%s", got)
	}
}

func TestApprovalCardWithoutDeadlineOrProfile(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec"})
	got := card(m)
	if !strings.Contains(got, "waiting (no timeout)") {
		t.Fatalf("no deadline:\n%s", got)
	}
	if strings.Contains(got, "profile") || strings.Contains(got, "from sub-agent") {
		t.Fatalf("empty origin row drawn:\n%s", got)
	}
}

func TestApprovalCardNamesTheSubAgentAndPattern(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Origin: "7f3e99", Profile: "remote",
		Pattern: "shell_exec(command matches go test*)"})
	got := card(m)
	for _, want := range []string{"from sub-agent 7f3e · profile remote", "p always allow shell_exec(command matches go test*)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestDefaultRuleReadsAsNoRuleMatched(t *testing.T) {
	if got := ansi.Strip(ruleLine("policy.default")); got != "no rule matched · default is ask" {
		t.Fatalf("ruleLine = %q", got)
	}
}

func boxText(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimSpace(ansi.Strip(l))
	}
	return out
}

func TestShellCallBox(t *testing.T) {
	got := boxText(callBox("shell_exec", `{"command":"go test","timeout_seconds":30}`, 8, 40))
	if strings.Join(got, "|") != "$ go test|timeout 30s" {
		t.Fatalf("with timeout: %q", got)
	}
	got = boxText(callBox("shell_exec", `{"command":"go test"}`, 8, 40))
	if strings.Join(got, "|") != "$ go test" {
		t.Fatalf("without timeout: %q", got)
	}
	for _, l := range callBox("shell_exec", `{"command":"go test"}`, 8, 40) {
		if ansi.StringWidth(l) != 40 {
			t.Fatalf("box row is %d wide, want 40", ansi.StringWidth(l))
		}
	}
}

func TestOtherToolsShowTheirArgs(t *testing.T) {
	got := boxText(callBox("fs_write", `{"path":"a"}`, 8, 40))
	if strings.Join(got, "|") != `{|"path": "a"|}` {
		t.Fatalf("args: %q", got)
	}
}

func longCommand() string {
	var lines []string
	for i := 0; i < 20; i++ {
		lines = append(lines, fmt.Sprintf("echo %d", i))
	}
	return strings.Join(lines, `\n`)
}

func TestCallBoxClipsToItsRows(t *testing.T) {
	got := boxText(callBox("shell_exec", `{"command":"`+longCommand()+`"}`, 8, 40))
	if len(got) != 8 || got[7] != "… 13 more lines" {
		t.Fatalf("clipped box: %q", got)
	}
}

func TestCardShrinksItsCallBoxBeforeClipping(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Args: `{"command":"` + longCommand() + `"}`})
	full := lipgloss.Height(m.approvalCard(100, 40))
	if got := lipgloss.Height(m.approvalCard(100, full-4)); got != full-4 {
		t.Fatalf("height %d, want %d", got, full-4)
	}
	if got := lipgloss.Height(m.approvalCard(100, 5)); got != full-7 {
		t.Fatalf("squeezed height %d, want %d (box down to one row)", got, full-7)
	}
}

func TestCardFitsANarrowPane(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec", Rule: "shell_exec", Profile: "local",
		Pattern: "shell_exec(command matches go test -race -count=50 ./internal/tui/...)",
		Args:    `{"command":"go test -race -count=50 -run 'TestA|TestB|TestC' ./internal/tui/... ./cmd/spore/..."}`})
	for _, l := range strings.Split(m.approvalCard(56, 20), "\n") {
		if w := ansi.StringWidth(l); w > 56 {
			t.Fatalf("card row is %d wide in a 56-column pane: %q", w, ansi.Strip(l))
		}
	}
}

func TestDraftLineUnderTheInput(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec"})
	if v := ansi.Strip(m.View()); !strings.Contains(v, "approval keys work in NORMAL — esc, then y/n/s/p") || strings.Contains(v, "draft kept") {
		t.Fatalf("empty input:\n%s", v)
	}
	run(m, keyMsg("h"))
	if v := ansi.Strip(m.View()); !strings.Contains(v, "draft kept · approval keys work in NORMAL — esc, then y/n/s/p") {
		t.Fatalf("with a draft:\n%s", v)
	}
	press(m, "esc")
	if v := ansi.Strip(m.View()); strings.Contains(v, "approval keys work in NORMAL") {
		t.Fatalf("still shown in NORMAL:\n%s", v)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'ApprovalCard|DefaultRule|ShellCallBox|OtherTools|CallBoxClips|CardShrinks|CardFits|DraftLine'`
Expected: build failure, `undefined: callBox` and `m.approvalCard undefined`.

- [ ] **Step 3: Create `internal/tui/approval.go`**

```go
package tui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

const (
	// cardMaxWidth keeps the approval card's rows readable on a wide terminal.
	cardMaxWidth = 76
	// callBoxLines is the most rows the card gives to what the call does.
	callBoxLines = 8
)

// waiting reports whether an approval waits on the selected session, its
// own or one of its sub-agents'.
func (m *Model) waiting() bool {
	if m.selected == "" {
		return false
	}
	_, _, ok := m.cache.approvalFor(m.selected)
	return ok
}

// approvalCard is the selected session's approval as a card for a w x h
// area, or empty when nothing waits. When h is short the call box gives up
// rows first, down to one; past that the card is taller than h, and the
// pane clips its foot.
func (m *Model) approvalCard(w, h int) string {
	_, ev, ok := m.cache.approvalFor(m.selected)
	if !ok || m.selected == "" {
		return ""
	}
	cw := min(cardMaxWidth, w-4)
	inner := max(10, cw-6)
	build := func(box []string) string {
		rows := []string{styApprovalTitle.Render(clip("spore wants to run "+ev.Tool, inner))}
		if o := originLine(ev); o != "" {
			rows = append(rows, clip(o, inner))
		}
		rows = append(rows, clip(ruleLine(ev.Rule), inner), "")
		rows = append(rows, box...)
		rows = append(rows, "", clip(styKey.Render("y")+styMuted.Render(" allow once   ")+
			styKey.Render("n")+styMuted.Render(" deny   ")+
			styKey.Render("s")+styMuted.Render(" allow "+ev.Tool+" this session"), inner))
		if ev.Pattern != "" {
			rows = append(rows, clip(styKey.Render("p")+styMuted.Render(" always allow ")+styAccent.Render(ev.Pattern), inner))
		}
		rows = append(rows, "", styMuted.Render(m.deadline(ev.ExpiresAt)))
		return styApprovalCard.Width(cw - 2).Render(strings.Join(rows, "\n"))
	}
	box := callBox(ev.Tool, ev.Args, callBoxLines, inner)
	card := build(box)
	if extra := lipgloss.Height(card) - h; extra > 0 {
		card = build(callBox(ev.Tool, ev.Args, max(1, len(box)-extra), inner))
	}
	return card
}

// originLine says who asked and under which profile; empty when neither
// is known.
func originLine(ev daemon.WireEvent) string {
	var parts []string
	if ev.Origin != "" {
		parts = append(parts, styMuted.Render("from sub-agent ")+styTool.Render(short(ev.Origin)))
	}
	if ev.Profile != "" {
		parts = append(parts, styMuted.Render("profile "+ev.Profile))
	}
	return strings.Join(parts, styMuted.Render(" · "))
}

// ruleLine names the rule that asked. An approval is always an ask, so the
// event needs no decision field to say so.
func ruleLine(rule string) string {
	switch rule {
	case "policy.default":
		return styMuted.Render("no rule matched · default is ask")
	case "":
		return styMuted.Render("matched ") + styWarn.Render("ask")
	}
	return styMuted.Render("matched ") + styWarn.Render("ask") + styMuted.Render("  "+rule)
}

// callBox is what the call will do, in at most maxRows rows of width w on a
// filled background: a shell command as a prompt line, anything else as
// its arguments.
func callBox(tool, args string, maxRows, w int) []string {
	var lines []string
	if tool == "shell_exec" {
		var in struct {
			Command        string `json:"command"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if json.Unmarshal([]byte(args), &in) == nil && in.Command != "" {
			for i, l := range strings.Split(strings.TrimRight(in.Command, "\n"), "\n") {
				if i == 0 {
					l = "$ " + l
				} else {
					l = "  " + l
				}
				lines = append(lines, l)
			}
			if in.TimeoutSeconds > 0 {
				lines = append(lines, styMuted.Render(fmt.Sprintf("timeout %ds", in.TimeoutSeconds)))
			}
		}
	}
	if lines == nil {
		lines = strings.Split(prettyArgs(args, 1<<30), "\n")
	}
	if len(lines) > maxRows {
		if maxRows >= 2 {
			more := len(lines) - (maxRows - 1)
			lines = append(lines[:maxRows-1:maxRows-1], styMuted.Render(fmt.Sprintf("… %d more lines", more)))
		} else {
			lines = lines[:1]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = fill(clip(" "+l, w), w, colFillCursor)
	}
	return out
}

// deadline is when the daemon denies an unanswered approval. expires is
// empty after a daemon restart, when nothing will time the ask out.
func (m *Model) deadline(expires string) string {
	at, err := time.Parse(time.RFC3339, expires)
	if expires == "" || err != nil {
		return "waiting (no timeout)"
	}
	left := at.Sub(m.opts.Now()).Round(time.Second)
	if left <= 0 {
		return "auto-denying…"
	}
	return fmt.Sprintf("auto-deny in %d:%02d", int(left.Minutes()), int(left.Seconds())%60)
}

// faint dims every row of s, so what is behind the card reads as behind it.
func faint(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = styFaint.Render(ansi.Strip(l))
	}
	return strings.Join(lines, "\n")
}

// draftLine tells someone typing that the approval keys wait for NORMAL.
func (m *Model) draftLine() string {
	if m.mode != modeInsert || !m.waiting() {
		return ""
	}
	msg := "approval keys work in NORMAL — esc, then y/n/s/p"
	if strings.TrimSpace(m.input.Value()) != "" {
		msg = "draft kept · " + msg
	}
	return styWarn.Render(clip(msg, m.mainWidth()))
}

func (m *Model) draftLineHeight() int {
	if m.draftLine() != "" {
		return 1
	}
	return 0
}
```

- [ ] **Step 4: Use the card in `view.go`**

Delete `overlayView`, `overlayHeight` and `deadline`. `deadline` now lives in `approval.go`.

In `sync`, change the viewport-height line to:

```go
	m.vp.Height = max(1, m.paneHeight()-1-lipgloss.Height(m.inputView())-m.draftLineHeight())
```

In `mainView`, replace from `parts := []string{...}` through the `if ov := ...` block with:

```go
	vpView := m.vp.View()
	if card := m.approvalCard(m.vp.Width, m.vp.Height); card != "" {
		vpView = placeOver(faint(vpView), card, m.vp.Width)
	}
	parts := []string{clip(m.sessionFacts(), m.mainWidth()), vpView, m.inputView()}
	if dl := m.draftLine(); dl != "" {
		parts = append(parts, dl)
	}
```

Remove `fmt` and `time` from `view.go`'s imports if the compiler reports them unused.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/tui -run 'ApprovalCard|DefaultRule|ShellCallBox|OtherTools|CallBoxClips|CardShrinks|CardFits|DraftLine'`
Expected: PASS.

Run: `go test ./internal/tui`
Expected: only the three `approval-*` goldens fail. `TestAYTypedInInsertNeverAnswersAnApproval` passes, because `esc, then y/n/s/p` is now in the draft line.

- [ ] **Step 6: Regenerate the goldens and read the diff**

Run: `go test ./internal/tui -run TestGolden -update && cat internal/tui/testdata/approval-normal-100.golden`
Expected:
- the card is centred in the transcript area with a double border (`╔`, `╚`);
- the stacked box above the input is gone;
- the input is back at full height;
- `approval-insert-100` has the draft line under the input;
- the other goldens do not change.

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add -A internal/tui
git commit -m "tui: approval as a centred card over a dimmed transcript

The call box shows a shell command as a prompt line, the rule reads as an
ask, and a draft line under the input replaces the key row's swap.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 8: Blocked pane, title and APPROVAL badge

**Files:**
- Modify: `internal/tui/view.go` (`paneBox` signature and all four callers, `mainView`, `statusView`)
- Modify: `internal/tui/header.go` (`paneTitle`, `keyHints`, new `approvalHints`, `approvalBar`)
- Test: `internal/tui/approval_test.go`, `internal/tui/view_test.go` (new goldens)

**Interfaces:**
- Consumes:
  - `waiting` and `approvalCard` (Task 7);
  - `nextBlockedID` and `styModeWarn` (Task 1).
- Produces:
  - `paneBox(title, body string, w, h int, focused bool, pad int, warn bool) string`
  - `(m *Model) approvalBar() bool`
  - `(m *Model) approvalHints() []string`

- [ ] **Step 1: Write the failing tests**

Append to `internal/tui/approval_test.go` (add `tea "github.com/charmbracelet/bubbletea"` to its imports):

```go
func TestBlockedPaneSaysWaitingOnYou(t *testing.T) {
	m := scene(t, 100, 30)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireApproval, PendingID: 6, Tool: "shell_exec", Rule: "shell_exec"})
	press(m, "esc", "tab")
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "┏━ a1b2 fix flaky test · waiting on you") {
		t.Fatalf("chat pane not heavy with the waiting title while the sidebar has focus:\n%s", v)
	}
	press(m, "tab")
	line := statusLine(m)
	if !strings.HasPrefix(line, " APPROVAL ") {
		t.Fatalf("badge: %q", line)
	}
	for _, want := range []string{"y once", "n deny", "s session", "b next blocked (c9d0)"} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
	if strings.Contains(line, "p pattern") || strings.Contains(line, "other keys do nothing") {
		t.Fatalf("status line = %q", line)
	}
}

func TestNoNextBlockedWhenOnlyTheSelectedWaits(t *testing.T) {
	m, _, _ := clockModel(t)
	approvalOn(m, daemon.WireEvent{Tool: "shell_exec"})
	press(m, "esc")
	if line := statusLine(m); strings.Contains(line, "next blocked") {
		t.Fatalf("offered itself: %q", line)
	}
}

func TestSubAgentSelectedStillShowsItsApproval(t *testing.T) {
	m := scene(t, 100, 30)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireApproval, PendingID: 9, Tool: "shell_exec", Origin: "7f3e99"})
	// Select the child directly: the approval lives on the root, and
	// approvalFor must find it through Origin.
	m.selected = "7f3e99"
	run(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	if !m.waiting() {
		t.Fatal("the sub-agent's own approval is not waiting on it")
	}
	v := ansi.Strip(m.View())
	for _, want := range []string{"7f3e audit the tests · waiting on you", "from sub-agent 7f3e"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q:\n%s", want, v)
		}
	}
}
```

Append to `internal/tui/view_test.go`:

```go
func TestGoldenApprovalCard(t *testing.T) {
	exp := t0.Add(4*time.Minute + 32*time.Second).Format(time.RFC3339)
	m := scene(t, 60, 24)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireApproval, PendingID: 7, Tool: "shell_exec",
		Args: `{"command":"go test -race -count=50 ./internal/tui/..."}`, Rule: "shell_exec", Profile: "local", ExpiresAt: exp})
	press(m, "esc")
	golden(t, "approval-card-60", m.View())

	m = scene(t, 100, 30)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireApproval, PendingID: 8, Tool: "shell_exec",
		Args: `{"command":"go test -race -count=50 ./internal/tui/...","timeout_seconds":300}`, Rule: "shell_exec",
		Origin: "7f3e99", Profile: "local", Pattern: "shell_exec(command matches go test*)", ExpiresAt: exp})
	press(m, "esc")
	golden(t, "approval-subagent-100", m.View())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui -run 'BlockedPane|NoNextBlocked|SubAgentSelected'`
Expected: FAIL. `TestBlockedPaneSaysWaitingOnYou` fails on the missing title, and `TestSubAgentSelectedStillShowsItsApproval` fails on the missing `waiting on you`.

- [ ] **Step 3: Give `paneBox` a warn state in `view.go`**

Replace `paneBox`'s signature and its border selection with:

```go
// paneBox frames body in w x h cells with title in the top border and pad
// blank columns inside each side. The focused pane gets a heavy accent
// border, the other a light muted one, so the difference survives a
// terminal without colour. warn outranks focus: a pane waiting on the person
// is heavy and amber whether or not it has focus.
func paneBox(title, body string, w, h int, focused bool, pad int, warn bool) string {
	iw, ih := max(1, w-2), max(1, h-2)
	cw := max(1, iw-2*pad)
	gap := strings.Repeat(" ", pad)
	b, sty, tsty := lipgloss.RoundedBorder(), styMuted, styMuted
	switch {
	case warn:
		b, sty, tsty = lipgloss.ThickBorder(), styWarn, styApprovalTitle
	case focused:
		b, sty, tsty = lipgloss.ThickBorder(), styAccent, styKey
	}
```

The rest of the body stays as it is. Update the four callers:
- the table: `paneBox(m.table.title(), body, m.width, m.bodyHeight(), true, 0, false)`;
- help: `paneBox("help", helpText(), m.chatOuter(), m.bodyHeight(), on, 1, false)`;
- chat: `paneBox(m.paneTitle(), strings.Join(parts, "\n"), m.chatOuter(), m.bodyHeight(), on, 1, m.waiting())`;
- the sidebar: `paneBox("sessions", withLegend(body, h), sidebarOuter, m.bodyHeight(), m.focused() == paneSidebar, 0, false)`.

In `statusView`, replace the badge line with:

```go
	badge := styMode.Render(" " + m.mode.String() + " ")
	if m.approvalBar() {
		badge = styModeWarn.Render(" APPROVAL ")
	}
```

- [ ] **Step 4: Add the title, the bar test and the hints in `header.go`**

Replace `paneTitle` with:

```go
// paneTitle is the chat pane's border title: the session's id and title,
// and that it waits on the person while an approval does.
func (m *Model) paneTitle() string {
	if m.selected == "" {
		return "no session"
	}
	title := m.cache.get(m.selected).info.Title
	if title == "" {
		title = "untitled"
	}
	out := short(m.selected) + " " + oneLine(title)
	if m.waiting() {
		out += " · waiting on you"
	}
	return out
}

// approvalBar is whether the status bar belongs to the approval: NORMAL,
// the chat on screen, and an approval waiting on the selected session.
func (m *Model) approvalBar() bool {
	return m.mode == modeNormal && m.table == nil && !m.help && m.waiting()
}

// approvalHints are the answers the card offers, and the next blocked
// session when there is one.
func (m *Model) approvalHints() []string {
	_, ev, _ := m.cache.approvalFor(m.selected)
	parts := []string{hint("y", "once"), hint("n", "deny"), hint("s", "session")}
	if ev.Pattern != "" {
		parts = append(parts, hint("p", "pattern"))
	}
	if id := m.nextBlockedID(); id != "" {
		parts = append(parts, hint("b", "next blocked ("+short(id)+")"))
	}
	return parts
}
```

In `keyHints`, change `default: parts = m.normalHints()` to:

```go
	default:
		if m.approvalBar() {
			parts = m.approvalHints()
		} else {
			parts = m.normalHints()
		}
```

- [ ] **Step 5: Run the tests, regenerate the goldens, and read the diff**

Run: `go test ./internal/tui -run 'BlockedPane|NoNextBlocked|SubAgentSelected'`
Expected: PASS.

Run: `go test ./internal/tui -run TestGolden -update && cat internal/tui/testdata/approval-card-60.golden internal/tui/testdata/approval-subagent-100.golden`
Expected:
- `approval-card-60` has no sidebar, a `┏━ a1b2 fix flaky test · waiting on you` title, a card that fits inside the pane, and an ` APPROVAL ` badge;
- `approval-subagent-100` shows `from sub-agent 7f3e · profile local`, `$ go test …`, `timeout 300s`, `p always allow …` and `auto-deny in 4:32`;
- among the existing goldens, only `approval-normal-100` and `approval-modal-100` change (title and badge).

Run: `go test ./internal/tui`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/tui
git commit -m "tui: a blocked session turns its pane, title and status badge amber

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

---

### Task 9: Full gate and the backlog

**Files:**
- Modify: `docs/backlog.md` (the "TUI: catch up with the web UI refresh" section)

- [ ] **Step 1: Run the whole CI gate from the repository root**

```bash
go vet -tags sqlite_fts5 ./... && make fmtcheck && make lint && make vulncheck && make tidycheck && go test -tags sqlite_fts5 -race -timeout 10m ./...
```

Expected:
- vet, fmtcheck and tidycheck print nothing, or only the make echo lines;
- lint prints `0 issues.`;
- vulncheck prints `Your code is affected by 0 vulnerabilities.`;
- every test package prints `ok` or `no test files`.

If anything fails, fix it inside the task that owns the code and re-run from the top.

- [ ] **Step 2: Add a paragraph to the backlog**

In `docs/backlog.md`, append this paragraph to the end of the "TUI: catch up with the web UI refresh" section:

```markdown
Followed by the canvas match on the same branch
(`docs/superpowers/specs/2026-09-29-tui-canvas-match-design.md`). It moves
the working line into the transcript with a spinner, draws the approval as
a centred card over a dimmed transcript, and brings the header, sidebar,
tool rows, placeholder and status bar to the canvas's chat and approval
boards. Still missing, because they need daemon data: live token and cache
figures on the working line, the rule's config location, and nested
`spore.*` calls inside `go_run`.
```

- [ ] **Step 3: Commit**

```bash
git add docs/backlog.md
git commit -m "backlog: record the TUI canvas match

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_011BAigyPxa65N3MHd7EFBbp"
```

- [ ] **Step 4: Hand over the manual check**

Do not claim the manual check was done. Tell the person to run the branch build with `go build -tags sqlite_fts5 -o /tmp/spore ./cmd/spore && /tmp/spore chat`, in a dark terminal and in a light one, and to:

1. send a message and watch the spinner line;
2. ask for `run ls` and check the card, the amber pane, the row fill and the ` APPROVAL ` badge;
3. press `i` and type a draft, and check the line under the input;
4. answer, and check everything returns to normal.

Then compare with the canvas's "TUI · chat" and "TUI · approval modal" boards.
