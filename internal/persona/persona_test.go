package persona

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsTheFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "soul.md")
	if err := os.WriteFile(p, []byte("Be blunt.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != "Be blunt.\n" {
		t.Fatalf("Load = %q, want %q", got, "Be blunt.\n")
	}
}

func TestLoadTreatsAMissingFileAsEmpty(t *testing.T) {
	// A machine with no soul.md must behave exactly as spore does today.
	// Returning an error here would turn "not configured" into a failure on
	// every turn.
	got, err := Load(filepath.Join(t.TempDir(), "absent.md"))
	if err != nil {
		t.Fatalf("a missing file is not an error, got %v", err)
	}
	if got != "" {
		t.Fatalf("Load = %q, want empty", got)
	}
}

func TestLoadReturnsAnEmptyPathAsEmpty(t *testing.T) {
	// AgentPath returns "" when the session has no workspace. Load must
	// absorb that rather than making every caller check first.
	got, err := Load("")
	if err != nil || got != "" {
		t.Fatalf("Load(\"\") = %q, %v; want \"\", nil", got, err)
	}
}

func TestLoadKeepsAnOversizedFileWhole(t *testing.T) {
	// These are the user's own standing instructions, deliberate in a way a
	// fact is not. Silently cutting them in half is worse than the tokens.
	dir := t.TempDir()
	p := filepath.Join(dir, "soul.md")
	body := strings.Repeat("x", warnBytes+500)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(body) {
		t.Fatalf("oversized file was truncated: got %d bytes, want %d", len(got), len(body))
	}
}

func TestLoadReportsARealReadError(t *testing.T) {
	// A directory where a file is expected is a misconfiguration, not an
	// absence, and must not masquerade as "no soul.md".
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("reading a directory as a persona file should error")
	}
}
