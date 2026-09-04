package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestDescribersEvictsIdleRoots(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "b.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := NewDescribers()
	now := time.Now()
	d.now = func() time.Time { return now }

	// Describe root A.
	d.Describe(a)
	if len(d.byRoot) != 1 {
		t.Fatalf("after first describe, len(byRoot) = %d, want 1", len(d.byRoot))
	}
	if _, ok := d.byRoot[a]; !ok {
		t.Fatalf("root A not in byRoot after first describe")
	}

	// Advance time past the idle threshold.
	now = now.Add(idleTTL + time.Second)

	// Describe root B. This should evict A.
	d.Describe(b)
	if len(d.byRoot) != 1 {
		t.Fatalf("after second describe, len(byRoot) = %d, want 1", len(d.byRoot))
	}
	if _, ok := d.byRoot[a]; ok {
		t.Fatalf("root A still in byRoot after eviction")
	}
	if _, ok := d.byRoot[b]; !ok {
		t.Fatalf("root B not in byRoot after second describe")
	}
}
