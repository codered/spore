package persona

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
)

func ctxAt(ws string) context.Context {
	return policy.WithSession(context.Background(), policy.Session{ID: "s1", Workspace: ws})
}

func TestAgentNoteCreatesTheFile(t *testing.T) {
	ws := t.TempDir()
	tool := newAgentNote(config.Default())

	out, err := tool.Call(ctxAt(ws), json.RawMessage(`{"text":"Always run make lint before pushing."}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(ws, ".spore", "agent.md"))
	if err != nil {
		t.Fatalf("agent.md was not created: %v", err)
	}
	if got, want := string(b), "- Always run make lint before pushing.\n"; got != want {
		t.Fatalf("agent.md = %q, want %q", got, want)
	}
	if !strings.Contains(out, "agent.md") {
		t.Fatalf("result %q does not say where it wrote", out)
	}
}

func TestAgentNoteAppends(t *testing.T) {
	ws := t.TempDir()
	tool := newAgentNote(config.Default())
	for _, s := range []string{"first", "second"} {
		if _, err := tool.Call(ctxAt(ws), json.RawMessage(`{"text":"`+s+`"}`)); err != nil {
			t.Fatalf("Call(%s): %v", s, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(ws, ".spore", "agent.md"))
	if got, want := string(b), "- first\n- second\n"; got != want {
		t.Fatalf("agent.md = %q, want %q", got, want)
	}
}

func TestAgentNoteKeepsAFileTheUserWrote(t *testing.T) {
	// The user owns this file too. A note appended to hand-written prose
	// must not disturb what is already there.
	ws := t.TempDir()
	dir := filepath.Join(ws, ".spore")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.md"), []byte("Hand written.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgentNote(config.Default()).Call(ctxAt(ws), json.RawMessage(`{"text":"added"}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "agent.md"))
	if got, want := string(b), "Hand written.\n- added\n"; got != want {
		t.Fatalf("agent.md = %q, want %q", got, want)
	}
}

func TestAgentNoteRefusesWithoutAWorkspace(t *testing.T) {
	_, err := newAgentNote(config.Default()).Call(ctxAt(""), json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("a session with no workspace has nowhere to write agent.md")
	}
}

func TestAgentNoteRefusesEmptyText(t *testing.T) {
	_, err := newAgentNote(config.Default()).Call(ctxAt(t.TempDir()), json.RawMessage(`{"text":"   "}`))
	if err == nil {
		t.Fatal("an empty note would add a bare bullet to the prompt forever")
	}
}

func TestAgentNoteIsNotReadOnly(t *testing.T) {
	// It writes files, so the loop must not dispatch it alongside others.
	if newAgentNote(config.Default()).ReadOnly() {
		t.Fatal("agent_note writes and must not be marked read-only")
	}
}

func TestAgentNoteWritesPrivateModes(t *testing.T) {
	ws := t.TempDir()
	if _, err := newAgentNote(config.Default()).Call(ctxAt(ws), json.RawMessage(`{"text":"x"}`)); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(filepath.Join(ws, ".spore"))
	if err != nil {
		t.Fatal(err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Fatalf(".spore mode = %o, want 700", got)
	}
	fi, err := os.Stat(filepath.Join(ws, ".spore", "agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("agent.md mode = %o, want 600", got)
	}
}
