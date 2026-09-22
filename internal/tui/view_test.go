package tui

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/provider"
)

// TestMain (in block_test.go) pins the markdown style, so these screens do
// not depend on the terminal running the tests.
var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	got = ansi.Strip(got)
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from %s:\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

// scene is the screen from the spec: a chat with a sub-agent, a blocked
// Discord session, another workspace, and an idle Discord session the
// default filter hides.
func scene(t *testing.T, width, height int) *Model {
	t.Helper()
	m := newTestModel(t, &fakeBackend{}, "a1b2c3")
	run(m, tea.WindowSizeMsg{Width: width, Height: height})
	run(m, sessionsMsg{list: []daemon.SessionJSON{
		{ID: "a1b2c3", Title: "fix flaky test", Workspace: "/work/spore", Source: "chat", UpdatedAt: t0.Add(3 * time.Minute)},
		{ID: "7f3e99", Title: "audit the tests", Workspace: "/work/spore", Source: "subagent", ParentID: "a1b2c3", UpdatedAt: t0.Add(2 * time.Minute)},
		{ID: "c9d0aa", Title: "lint audit", Workspace: "/work/spore", Source: "discord", UpdatedAt: t0.Add(time.Minute)},
		{ID: "e5f6bb", Title: "notes", Workspace: "/work/web", Source: "chat", UpdatedAt: t0},
		{ID: "d00d11", Title: "old discord", Workspace: "/work/web", Source: "discord", UpdatedAt: t0},
	}})
	run(m, transcriptMsg{id: "a1b2c3", t: daemon.TranscriptJSON{
		Session: daemon.SessionJSON{ID: "a1b2c3", Title: "fix flaky test", Workspace: "/work/spore", Source: "chat"},
		Messages: []daemon.MessageJSON{
			{Role: "user", Blocks: []provider.Block{{Type: provider.BlockText, Text: "fix the flaky tui test"}}},
			{Role: "assistant", Model: "sonnet-5", TokensIn: 1200, TokensCacheRead: 36800, Blocks: []provider.Block{
				{Type: provider.BlockText, Text: "Looking at tui_test.go…"},
				{Type: provider.BlockToolUse, ID: "t1", Name: "read", Input: json.RawMessage(`{"path":"cmd/spore/tui_test.go"}`)},
			}},
			{Role: "tool", Blocks: []provider.Block{{Type: provider.BlockToolResult, ID: "t1", Content: "package main\n\nimport \"testing\"\n"}}},
			{Role: "assistant", Blocks: []provider.Block{
				{Type: provider.BlockToolUse, ID: "t2", Name: "bash", Input: json.RawMessage(`{"cmd":"go test ./cmd/spore"}`)},
			}},
			{Role: "tool", Blocks: []provider.Block{{Type: provider.BlockToolResult, ID: "t2", Content: "exit 1\n--- FAIL: TestX", IsError: true}}},
		},
	}})
	feed(m, daemon.WireEvent{Session: "c9d0aa", Type: daemon.WireApproval, PendingID: 5, Tool: "shell", Args: `{"cmd":"make lint"}`, Rule: "shell.ask"})
	return m
}

func TestGoldenScreens(t *testing.T) {
	for _, w := range []int{60, 100, 160} {
		golden(t, fmt.Sprintf("screen-%d", w), scene(t, w, 24).View())
	}
}

func TestGoldenAllSessions(t *testing.T) {
	m := scene(t, 100, 24)
	press(m, "esc", ":")
	typeText(m, "sessions all")
	press(m, "enter")
	golden(t, "sessions-all-100", m.View())
}

func TestGoldenExpandedTool(t *testing.T) {
	m := scene(t, 100, 30)
	press(m, "esc", "o")
	golden(t, "tool-expanded-100", m.View())
}

func TestGoldenApprovalOverlay(t *testing.T) {
	m := scene(t, 100, 30)
	feed(m, daemon.WireEvent{Session: "a1b2c3", Type: daemon.WireApproval, PendingID: 6, Tool: "fs.write",
		Args: `{"path":"cmd/spore/tui_test.go"}`, Rule: "fs.write.ask", Pattern: "fs.write:cmd/spore/*"})
	golden(t, "approval-insert-100", m.View())
	press(m, "esc")
	golden(t, "approval-normal-100", m.View())
}

func TestGoldenTooSmall(t *testing.T) {
	golden(t, "too-small", scene(t, 50, 10).View())
}
