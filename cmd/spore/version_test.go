package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVersionNeedsNoConfig(t *testing.T) {
	// A config path that does not exist: run must answer without loading it.
	missing := filepath.Join(t.TempDir(), "nope", "config.toml")
	for _, arg := range []string{"version", "--version", "-v"} {
		if err := run([]string{"-config", missing, arg}); err != nil {
			t.Fatalf("spore %s: %v", arg, err)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("version created or read config: %v", err)
	}
}

func TestIsVersionArg(t *testing.T) {
	if isVersionArg([]string{"version", "extra"}) {
		t.Fatal("version with extra args should fall through to dispatch")
	}
	if isVersionArg([]string{"once"}) {
		t.Fatal("once is not a version request")
	}
}
