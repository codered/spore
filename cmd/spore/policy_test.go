package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
)

// spore policy check is how an operator asks why a call was refused, so the
// containment rule's explanation has to reach it.
func TestPolicyCheckPrintsTheDetailLine(t *testing.T) {
	ws := t.TempDir()
	body := "default_model = \"anthropic/claude-opus-5\"\n" +
		"[policy]\nworkspace = \"" + ws + "\"\n\n" +
		"[[mcp.server]]\nname = \"files\"\ntransport = \"stdio\"\ncommand = \"/bin/true\"\n"
	path := filepath.Join(t.TempDir(), "spore.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	out := captureStdout(t, func() error {
		return cmdPolicyCheck(cfg, "local", ws, "mcp__files__read", `{"path":"/etc/passwd"}`)
	})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want a decision line and an indented detail line", out)
	}
	if !strings.HasPrefix(lines[0], "deny\tmcp__files__read\tmcp__*(any path outside workspace)\t") {
		t.Errorf("first line = %q, want the deny and the rule", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  ") || !strings.Contains(lines[1], "/etc/passwd") {
		t.Errorf("second line = %q, want an indented detail naming the path", lines[1])
	}

	// A decision with no detail still prints exactly one line.
	plain := captureStdout(t, func() error {
		return cmdPolicyCheck(cfg, "local", ws, "fs_read", `{"path":"`+ws+`/a"}`)
	})
	if got := len(strings.Split(strings.TrimRight(plain, "\n"), "\n")); got != 1 {
		t.Errorf("output = %q, want one line", plain)
	}
}
