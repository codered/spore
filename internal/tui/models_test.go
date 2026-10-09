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
	press(m, "alt+esc", ":") // leave INSERT, where the test model starts
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
	typeText(m, "/model") // the test model starts in INSERT
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
