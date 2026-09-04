package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionWorkspaceDefaultsToCwd(t *testing.T) {
	got, err := sessionWorkspace("")
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	if got != cwd {
		t.Fatalf("workspace = %q, want the working directory %q", got, cwd)
	}
}

func TestSessionWorkspaceMakesTheFlagAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	got, err := sessionWorkspace("sub/dir")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "sub", "dir"); got != want {
		t.Fatalf("workspace = %q, want %q", got, want)
	}
}
