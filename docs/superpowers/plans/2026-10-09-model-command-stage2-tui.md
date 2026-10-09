# /model Stage 2 (TUI modal) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `/model` (or `:model`) in `spore chat` opens a tabbed modal: an overview tab, then one tab per operation listing its options with `->` on the selected model and `*` on the default; enter chooses.

**Architecture:** A new TUI mode `modeModels` with its own state, drawn over the screen with the existing `placeOver`, like the history picker. The Backend interface gains `Models`/`SetModel`; `cmd/spore`'s `tuiBackend` forwards them to the client methods from stage 1. Option order, availability and confirmation text come from `internal/modelcmd`, so the TUI lists exactly what plain chat numbers.

**Tech Stack:** Go 1.26, Bubble Tea, lipgloss.

**Spec:** `docs/superpowers/specs/2026-10-09-model-command-design.md` (§6 TUI)

## Global Constraints

- Worktree `/home/code/development/spore-model-command`, branch `feat/model-command`. Never `git add -A`. Tests: `go test -tags sqlite_fts5 ./internal/tui/ ./cmd/spore/`.
- Tabs in this order: `overview`, then `chat, compaction, title, classify, refinement, subagent` (the order of `View.Ops`).
- Markers: `->` selected, `*` default. Unavailable options are dimmed and refused with a message; the default is always choosable (`modelcmd.Option.Choosable`).
- Keys in the modal: `tab`/`l`/`right` next tab, `shift+tab`/`h`/`left` previous tab, `j`/`down` and `k`/`up` move, `enter` choose (on the overview: open that op's tab), `r` refresh the catalog, `esc`/`q` close.
- A failed fetch or choice shows the error inside the modal and leaves it open.
- Commit trailer:
  ```
  Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_016aHKRsXHkr3iTMvo9YixbC
  ```

## Review Focus

1. Opening `/model` from INSERT must return to INSERT on close with the draft intact; from NORMAL or `:` it returns to NORMAL.
2. A models response that arrives after the modal was closed must not reopen it.
3. A list longer than the modal height must scroll to keep the cursor visible.
4. The cursor must be clamped when a refresh shortens the list.
5. With no session selected, choosing chat/subagent shows the daemon's error in the modal instead of crashing.

---

### Task 1: The models modal

**Files:**
- Create: `internal/tui/models.go`, `internal/tui/models_test.go`
- Modify: `internal/tui/backend.go` (two methods on `Backend`), `internal/tui/app.go` (mode, field, command, message dispatch, key dispatch, `commandNames`), `internal/tui/view.go` (overlay), `internal/tui/header.go` (hints), `internal/tui/app_test.go` (fake backend methods), `cmd/spore/tui_backend.go` (forwarders)

**Interfaces:**
- Consumes (stage 1): `daemon.ModelsJSON` (= `models.View{Ops []models.Op; Groups []models.Group}`), `modelcmd.Options(v, op) []modelcmd.Option`, `(modelcmd.Option).Choosable()`, `modelcmd.Overview(v) string`, `modelcmd.Confirm(v, op) string`; client `(*client).models(ctx, id, fresh)` and `(*client).setModel(ctx, id, op, ref)`.
- Produces: `Backend.Models(ctx, id string, fresh bool) (daemon.ModelsJSON, error)`, `Backend.SetModel(ctx, id, op, ref string) (daemon.ModelsJSON, error)`; `modeModels`.

- [ ] **Step 1: Fake backend** — in `internal/tui/app_test.go`, add fields to `fakeBackend` (after `deletedFacts`):

```go
	modelsView daemon.ModelsJSON
	modelsErr  error
	modelSets  []string
	setErr     error
```

and methods (next to the other fake methods):

```go
func (f *fakeBackend) Models(context.Context, string, bool) (daemon.ModelsJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.modelsView, f.modelsErr
}

// SetModel records the choice and answers with the view as the daemon
// would: the op now selects ref.
func (f *fakeBackend) SetModel(_ context.Context, id, op, ref string) (daemon.ModelsJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelSets = append(f.modelSets, id+" "+op+" "+ref)
	if f.setErr != nil {
		return daemon.ModelsJSON{}, f.setErr
	}
	ops := append([]models.Op(nil), f.modelsView.Ops...)
	for i := range ops {
		if ops[i].Op == op {
			ops[i].Selected = ref
		}
	}
	f.modelsView.Ops = ops
	return f.modelsView, nil
}
```

Import `"github.com/codered/spore/internal/models"` in `app_test.go`.

- [ ] **Step 2: Write the failing tests** — `internal/tui/models_test.go`:

```go
package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/models"
)

func modelsFixture() daemon.ModelsJSON {
	return daemon.ModelsJSON{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "a/one", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "gone/x", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "a/one", Default: "a/one"},
		},
		Groups: []models.Group{
			{Provider: "a", Refs: []string{"a/one", "a/two"}},
			{Provider: "down", Refs: []string{}, Error: "connection refused"},
		},
	}
}

func openModelsCmd(t *testing.T, fb *fakeBackend) *Model {
	t.Helper()
	fb.modelsView = modelsFixture()
	m := newTestModel(t, fb, "s1")
	press(m, ":")
	typeText(m, "model")
	press(m, "enter")
	if m.mode != modeModels {
		t.Fatalf("mode = %s, want MODELS", m.mode)
	}
	return m
}

func TestModelCommandOpensTheOverview(t *testing.T) {
	m := openModelsCmd(t, &fakeBackend{})
	v := ansi.Strip(m.View())
	for _, want := range []string{"overview", "compaction", "subagent", "-> a/one", "-> gone/x", "! down: connection refused"} {
		if !strings.Contains(v, want) {
			t.Fatalf("modal lacks %q:\n%s", want, v)
		}
	}
}

func TestChoosingAnOptionSetsItAndStaysOpen(t *testing.T) {
	fb := &fakeBackend{}
	m := openModelsCmd(t, fb)
	press(m, "tab") // chat
	v := ansi.Strip(m.View())
	if !strings.Contains(v, "-> a/one *") || !strings.Contains(v, "a/two") {
		t.Fatalf("chat tab:\n%s", v)
	}
	press(m, "j", "enter")
	if len(fb.modelSets) != 1 || fb.modelSets[0] != "s1 chat a/two" {
		t.Fatalf("sets = %v", fb.modelSets)
	}
	if m.mode != modeModels {
		t.Fatalf("mode = %s, want the modal to stay open", m.mode)
	}
	v = ansi.Strip(m.View())
	if !strings.Contains(v, "-> a/two") || !strings.Contains(v, "chat -> a/two (this session)") {
		t.Fatalf("after choosing:\n%s", v)
	}
}

func TestAnUnavailableOptionIsRefusedInTheModal(t *testing.T) {
	fb := &fakeBackend{}
	m := openModelsCmd(t, fb)
	press(m, "tab", "tab", "tab") // title: gone/x selected, unavailable
	press(m, "enter")
	if len(fb.modelSets) != 0 {
		t.Fatalf("an unavailable model was sent: %v", fb.modelSets)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "gone/x is not available right now") {
		t.Fatalf("no refusal shown:\n%s", v)
	}
	press(m, "j", "enter") // the default, a/one: always choosable
	if len(fb.modelSets) != 1 || fb.modelSets[0] != "s1 title a/one" {
		t.Fatalf("sets = %v", fb.modelSets)
	}
}

func TestOverviewEnterOpensThatOpsTab(t *testing.T) {
	fb := &fakeBackend{}
	m := openModelsCmd(t, fb)
	press(m, "j", "j", "enter") // overview row 3: title
	if m.models.tab != 3 {
		t.Fatalf("tab = %d, want 3 (title)", m.models.tab)
	}
	press(m, "shift+tab", "h")
	if m.models.tab != 1 {
		t.Fatalf("tab = %d after two steps back, want 1", m.models.tab)
	}
}

func TestASetErrorShowsInTheModal(t *testing.T) {
	fb := &fakeBackend{setErr: errors.New("chat is chosen per session")}
	m := openModelsCmd(t, fb)
	press(m, "tab", "j", "enter")
	if m.mode != modeModels {
		t.Fatalf("mode = %s", m.mode)
	}
	if v := ansi.Strip(m.View()); !strings.Contains(v, "chat is chosen per session") {
		t.Fatalf("error not shown:\n%s", v)
	}
}

func TestEscReturnsToTheModeItOpenedFrom(t *testing.T) {
	fb := &fakeBackend{modelsView: modelsFixture()}
	m := newTestModel(t, fb, "s1")
	press(m, "i")
	typeText(m, "/model")
	press(m, "enter")
	if m.mode != modeModels {
		t.Fatalf("mode = %s, want MODELS", m.mode)
	}
	press(m, "esc")
	if m.mode != modeInsert {
		t.Fatalf("mode = %s, want INSERT", m.mode)
	}
	press(m, "ctrl+o", ":")
	typeText(m, "model")
	press(m, "enter", "q")
	if m.mode != modeNormal {
		t.Fatalf("mode = %s, want NORMAL", m.mode)
	}
}

func TestALateResponseDoesNotReopenTheModal(t *testing.T) {
	m := openModelsCmd(t, &fakeBackend{})
	press(m, "esc")
	run(m, modelsMsg{view: modelsFixture()})
	if m.mode == modeModels || m.models != nil {
		t.Fatal("a late models response reopened the modal")
	}
}

func TestTheCursorStaysOnAShorterList(t *testing.T) {
	fb := &fakeBackend{}
	m := openModelsCmd(t, fb)
	press(m, "tab", "j") // chat, cursor on a/two
	short := modelsFixture()
	short.Groups = []models.Group{{Provider: "a", Refs: []string{"a/one"}}}
	run(m, modelsMsg{view: short})
	if m.models.cursor != 0 {
		t.Fatalf("cursor = %d, want clamped to 0", m.models.cursor)
	}
}
```

- [ ] **Step 3: Run to verify failure**

Run: `go test -tags sqlite_fts5 ./internal/tui/ -run 'Model'`
Expected: FAIL to compile (`modeModels`, `modelsMsg`, `m.models` undefined; fake does not satisfy `Backend` once it is extended — that is fine, it compiles only after Step 4).

- [ ] **Step 4: Implement**

`internal/tui/backend.go`, add to `Backend` (after `RefineRollback`):

```go
	// Models is what /model shows for a session; fresh relists every
	// provider instead of using the daemon's cached listing.
	Models(ctx context.Context, id string, fresh bool) (daemon.ModelsJSON, error)
	// SetModel chooses ref for op and returns the updated view.
	SetModel(ctx context.Context, id, op, ref string) (daemon.ModelsJSON, error)
```

`internal/tui/app.go`:
1. Add `modeModels` after `modeHistory` in the `mode` constants, and `"MODELS"` at the end of the `String()` array.
2. Add `"model"` to `commandNames` (keep the list sorted: after `"jobs"`).
3. In the `Model` struct, after `confirm *confirmState`:

```go
	// models is the /model modal; nil when it is closed.
	models *modelsState
```

4. In `command`, add a case before `case "refine":`:

```go
	case "model":
		return m.openModels()
```

5. In `update`'s message switch, add (next to `noticeMsg`):

```go
	case modelsMsg:
		m.modelsLoaded(msg)
		return nil
```

6. In the key dispatch `switch m.mode`, add:

```go
	case modeModels:
		return m.keyModels(k)
```

`internal/tui/view.go`, in `View()`, after the `modeHistory` overlay:

```go
	if m.mode == modeModels && m.models != nil {
		body = placeOver(body, m.modelsView(), m.width)
	}
```

`internal/tui/header.go`: in the status hints `switch m.mode`, add

```go
	case modeModels:
		parts = modelsParts()
```

and next to `historyParts`:

```go
func modelsParts() []string {
	return []string{hint("tab", "operation"), hint("↑↓", "move"), hint("enter", "choose"), hint("r", "refresh"), hint("esc", "close")}
}
```

In `helpText()` (view.go), in the NORMAL block after the `z` line, add:

```go
		"  :model    choose the model for each operation (also /model while typing)",
```

`internal/tui/models.go`:

```go
package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/modelcmd"
)

// modelRows caps how many options the modal shows at once.
const modelRows = 12

// modelsState is the /model modal. tab 0 is the overview; tab i is
// view.Ops[i-1]. cursor is a row on the overview, an option on an op tab.
type modelsState struct {
	view    daemon.ModelsJSON
	loaded  bool
	tab     int
	cursor  int
	note    string
	isErr   bool
	back    mode
}

// modelsMsg carries a models view from the daemon: a fetch, or the answer
// to a choice (op set, with the confirmation to show).
type modelsMsg struct {
	view daemon.ModelsJSON
	op   string
	err  error
}

// openModels shows the modal and fetches what to put in it. It returns to
// INSERT if opened from there, so a draft survives a look at the models.
func (m *Model) openModels() tea.Cmd {
	back := modeNormal
	if m.mode == modeInsert {
		back = modeInsert
	}
	m.models = &modelsState{back: back}
	m.mode = modeModels
	m.input.Blur()
	return m.fetchModels(false)
}

func (m *Model) fetchModels(fresh bool) tea.Cmd {
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		v, err := be.Models(ctx, id, fresh)
		return modelsMsg{view: v, err: err}
	}
}

func (m *Model) modelsLoaded(msg modelsMsg) {
	s := m.models
	if s == nil || m.mode != modeModels {
		return // closed meanwhile
	}
	if msg.err != nil {
		s.note, s.isErr = msg.err.Error(), true
		return
	}
	s.view, s.loaded = msg.view, true
	s.note, s.isErr = "", false
	if msg.op != "" {
		s.note = strings.TrimSpace(modelcmd.Confirm(s.view, msg.op))
	}
	s.cursor = min(s.cursor, max(0, m.modelRowCount()-1))
}

// modelRowCount is how many rows the current tab has.
func (m *Model) modelRowCount() int {
	s := m.models
	if s.tab == 0 {
		return len(s.view.Ops)
	}
	return len(modelcmd.Options(s.view, s.view.Ops[s.tab-1].Op))
}

func (m *Model) closeModels() tea.Cmd {
	back := m.models.back
	m.models = nil
	if back == modeInsert {
		return m.enterInsert()
	}
	m.mode = modeNormal
	return nil
}

func (m *Model) keyModels(k tea.KeyMsg) tea.Cmd {
	s := m.models
	tabs := len(s.view.Ops) + 1
	switch k.String() {
	case "esc", "q":
		return m.closeModels()
	case "tab", "l", "right":
		s.tab, s.cursor = (s.tab+1)%tabs, 0
	case "shift+tab", "h", "left":
		s.tab, s.cursor = (s.tab+tabs-1)%tabs, 0
	case "j", "down":
		s.cursor = min(s.cursor+1, max(0, m.modelRowCount()-1))
	case "k", "up":
		s.cursor = max(0, s.cursor-1)
	case "r":
		s.note, s.isErr = "refreshing…", false
		return m.fetchModels(true)
	case "enter":
		return m.chooseModel()
	}
	return nil
}

// chooseModel acts on the row under the cursor: on the overview it opens
// that op's tab; on an op tab it sends the choice.
func (m *Model) chooseModel() tea.Cmd {
	s := m.models
	if !s.loaded {
		return nil
	}
	if s.tab == 0 {
		if s.cursor < len(s.view.Ops) {
			s.tab, s.cursor = s.cursor+1, 0
		}
		return nil
	}
	op := s.view.Ops[s.tab-1].Op
	opts := modelcmd.Options(s.view, op)
	if s.cursor >= len(opts) {
		return nil
	}
	o := opts[s.cursor]
	if !o.Choosable() {
		s.note, s.isErr = o.Ref+" is not available right now", true
		return nil
	}
	be, ctx, id := m.be, m.ctx, m.selected
	return func() tea.Msg {
		v, err := be.SetModel(ctx, id, op, o.Ref)
		return modelsMsg{view: v, op: op, err: err}
	}
}

// modelsView draws the modal: the tab bar, the tab's rows, provider
// errors, and the last confirmation or error.
func (m *Model) modelsView() string {
	s := m.models
	cw := max(30, min(90, m.width-10))
	names := []string{"overview"}
	for _, o := range s.view.Ops {
		names = append(names, o.Op)
	}
	var tabs []string
	for i, n := range names {
		if i == s.tab {
			tabs = append(tabs, styKey.Render("["+n+"]"))
		} else {
			tabs = append(tabs, styMuted.Render(" "+n+" "))
		}
	}
	lines := []string{styApprovalTitle.Render("Models") + styMuted.Render("  -> selected  * default"), strings.Join(tabs, "")}
	lines = append(lines, "")

	switch {
	case !s.loaded:
		lines = append(lines, styMuted.Render("loading…"))
	case s.tab == 0:
		for i, l := range strings.Split(strings.TrimRight(modelcmd.Overview(s.view), "\n"), "\n") {
			lines = append(lines, modelRow(clip(l, cw), i == s.cursor, true))
		}
	default:
		opts := modelcmd.Options(s.view, s.view.Ops[s.tab-1].Op)
		lo := max(0, min(s.cursor-modelRows/2, len(opts)-modelRows))
		hi := min(len(opts), lo+modelRows)
		for i := lo; i < hi; i++ {
			o := opts[i]
			mark := "  "
			if o.Selected {
				mark = "->"
			}
			text := mark + " " + o.Ref
			if o.Default {
				text += " *"
			}
			if !o.Available {
				text += " (unavailable)"
			}
			lines = append(lines, modelRow(clip(text, cw), i == s.cursor, o.Choosable()))
		}
	}
	for _, g := range s.view.Groups {
		if g.Error != "" {
			lines = append(lines, styMuted.Render(clip("! "+g.Provider+": "+g.Error, cw)))
		}
	}
	if s.note != "" {
		sty := styAccent
		if s.isErr {
			sty = styWarn
		}
		lines = append(lines, "", sty.Render(clip(s.note, cw)))
	}
	lines = append(lines, "", strings.Join(modelsParts(), "  "))
	w := 0
	for _, l := range lines {
		w = max(w, lipgloss.Width(l))
	}
	return styModal.Width(min(w, cw) + 4).Render(strings.Join(lines, "\n"))
}

// modelRow marks the cursor row and dims a row that cannot be chosen.
func modelRow(text string, cursor, choosable bool) string {
	switch {
	case cursor:
		return styAccent.Render("› " + text)
	case !choosable:
		return styMuted.Render("  " + text)
	}
	return "  " + text
}
```

(If `styApprovalTitle` or `clip` have different names, use the ones `historyView` in view.go uses; read it first.)

`cmd/spore/tui_backend.go`, after `Slash`:

```go
func (b tuiBackend) Models(ctx context.Context, id string, fresh bool) (daemon.ModelsJSON, error) {
	return b.c.models(ctx, id, fresh)
}

func (b tuiBackend) SetModel(ctx context.Context, id, op, ref string) (daemon.ModelsJSON, error) {
	return b.c.setModel(ctx, id, op, ref)
}
```

(Import `daemon` there if it is not already.)

- [ ] **Step 5: Run tests**

Run: `go test -tags sqlite_fts5 ./internal/tui/ ./cmd/spore/`
Expected: PASS (new and existing). If the fixture's `View()` is cut off at 40 rows so a string is missing, raise the test window in `openModelsCmd` with `run(m, tea.WindowSizeMsg{Width: 140, Height: 50})` rather than changing the modal.

Then the gate: `go vet -tags sqlite_fts5 ./...`, `make fmtcheck`, `make lint`, `go test -tags sqlite_fts5 -race ./internal/tui/ ./cmd/spore/`.

- [ ] **Step 6: Commit**

```bash
git add internal/tui/models.go internal/tui/models_test.go internal/tui/backend.go internal/tui/app.go internal/tui/view.go internal/tui/header.go internal/tui/app_test.go cmd/spore/tui_backend.go
git commit -m "tui: /model opens a tabbed modal to choose each operation's model"   # plus the trailer
```
