package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// One describer per root: two sessions in two directories must each see their
// own files, not whichever was described first.
func TestDescribersAreKeyedByRoot(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "alpha.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "beta.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := NewDescribers()
	gotA, gotB := d.Describe(a), d.Describe(b)
	if !strings.Contains(gotA, "alpha.txt") || strings.Contains(gotA, "beta.txt") {
		t.Fatalf("describe(a) = %q", gotA)
	}
	if !strings.Contains(gotB, "beta.txt") || strings.Contains(gotB, "alpha.txt") {
		t.Fatalf("describe(b) = %q", gotB)
	}
}

func TestDescribersEmptyRoot(t *testing.T) {
	if got := NewDescribers().Describe(""); got != "" {
		t.Fatalf("describe(\"\") = %q, want empty", got)
	}
}
