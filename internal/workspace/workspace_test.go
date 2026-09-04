package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tree writes a set of files, creating parent directories as needed. A value
// of "" still writes an empty file, which is enough for a listing test.
func tree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listOf(t *testing.T, root string) []string {
	t.Helper()
	out, _, _ := list(root)
	return out
}

func has(entries []string, want string) bool {
	for _, e := range entries {
		if e == want {
			return true
		}
	}
	return false
}

func TestDescribeReportsWorkingDirectoryAndFiles(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{"main.go": "package main", "docs/readme.md": "hi"})

	got := Describe(root)
	if !strings.Contains(got, "Working directory: "+root) {
		t.Errorf("missing working directory line:\n%s", got)
	}
	for _, want := range []string{"main.go", "docs/", "docs/readme.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
}

func TestDescribeCarriesNoFileContents(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{"secret.txt": "SUPER-SECRET-BODY"})

	if got := Describe(root); strings.Contains(got, "SUPER-SECRET-BODY") {
		t.Errorf("file contents leaked into the prompt:\n%s", got)
	}
}

func TestDescribeEmptyOrMissingRoot(t *testing.T) {
	if got := Describe(""); got != "" {
		t.Errorf("empty root: got %q, want no section", got)
	}
	if got := Describe(filepath.Join(t.TempDir(), "gone")); got != "" {
		t.Errorf("missing root: got %q, want no section", got)
	}
	if got := Describe(t.TempDir()); !strings.Contains(got, "empty") {
		t.Errorf("empty directory: want an explicit note, got:\n%s", got)
	}
}

func TestGitignoreExcludesMatchingEntries(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{
		".gitignore":       "*.log\nbuild/\n/rootonly.txt\n",
		"keep.go":          "",
		"debug.log":        "",
		"rootonly.txt":     "",
		"build/out.bin":    "",
		"sub/nested.log":   "",
		"sub/rootonly.txt": "",
	})

	got := listOf(t, root)
	for _, want := range []string{"keep.go", "sub/", "sub/rootonly.txt"} {
		if !has(got, want) {
			t.Errorf("expected %q in listing, got %v", want, got)
		}
	}
	for _, unwanted := range []string{"debug.log", "rootonly.txt", "build/", "build/out.bin", "sub/nested.log"} {
		if has(got, unwanted) {
			t.Errorf("ignored entry %q appeared in listing: %v", unwanted, got)
		}
	}
}

func TestGitignoreNegationReincludes(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{
		".gitignore": "*.log\n!keep.log\n",
		"drop.log":   "",
		"keep.log":   "",
	})

	got := listOf(t, root)
	if !has(got, "keep.log") {
		t.Errorf("negated pattern should re-include keep.log: %v", got)
	}
	if has(got, "drop.log") {
		t.Errorf("drop.log should stay ignored: %v", got)
	}
}

func TestGitignoreDoubleStarAndNestedFiles(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{
		".gitignore":      "a/**/skip.txt\n",
		"sub/.gitignore":  "local.txt\n",
		"a/b/c/skip.txt":  "",
		"a/keep.txt":      "",
		"sub/local.txt":   "",
		"sub/visible.txt": "",
		"other/local.txt": "",
	})

	got := listOf(t, root)
	for _, unwanted := range []string{"a/b/c/skip.txt", "sub/local.txt"} {
		if has(got, unwanted) {
			t.Errorf("%q should be ignored: %v", unwanted, got)
		}
	}
	for _, want := range []string{"a/keep.txt", "sub/visible.txt", "other/local.txt"} {
		if !has(got, want) {
			t.Errorf("%q should be listed: %v", want, got)
		}
	}
}

func TestGitDirectoryIsNeverListed(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{".git/config": "", "main.go": ""})

	got := listOf(t, root)
	for _, e := range got {
		if strings.HasPrefix(e, ".git") {
			t.Errorf("git internals leaked into the listing: %v", got)
		}
	}
}

func TestListingIsBoundedAndSaysSo(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < maxEntries+50; i++ {
		files["f"+itoa(i)+".txt"] = ""
	}
	tree(t, root, files)

	entries, truncated, _ := list(root)
	if len(entries) != maxEntries {
		t.Errorf("listing length %d, want the cap %d", len(entries), maxEntries)
	}
	if !truncated {
		t.Error("truncation not reported")
	}
	if got := Describe(root); !strings.Contains(got, "stopped at") {
		t.Errorf("truncated listing should say so:\n%s", got[len(got)-200:])
	}
}

func TestWalkStopsAtMaxDepth(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{"a/b/c/d/e/deep.txt": ""})

	for _, e := range listOf(t, root) {
		if strings.Count(e, "/") > maxDepth {
			t.Errorf("entry %q is deeper than maxDepth %d", e, maxDepth)
		}
	}
}

func TestDescriberCachesUntilTTLExpires(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{"first.go": ""})

	now := time.Now()
	d := NewDescriber(root)
	d.now = func() time.Time { return now }

	first := d.Describe()
	tree(t, root, map[string]string{"second.go": ""})
	if second := d.Describe(); second != first {
		t.Error("describer rebuilt inside the TTL")
	}
	now = now.Add(cacheTTL + time.Second)
	if third := d.Describe(); !strings.Contains(third, "second.go") {
		t.Errorf("describer did not refresh after the TTL:\n%s", third)
	}
}

func TestCacheDirectoriesAreNeverListed(t *testing.T) {
	root := t.TempDir()
	tree(t, root, map[string]string{
		".cache/go-build/00/aaaa-a": "",
		"__pycache__/mod.pyc":       "",
		".venv/lib/thing.py":        "",
		"src/main.go":               "",
	})

	got := listOf(t, root)
	for _, e := range got {
		for _, noise := range []string{".cache", "__pycache__", ".venv"} {
			if strings.HasPrefix(e, noise) {
				t.Errorf("cache directory leaked into the listing: %q in %v", e, got)
			}
		}
	}
	if !has(got, "src/main.go") {
		t.Errorf("real source missing from the listing: %v", got)
	}
}

func TestOneFatSubtreeCannotStarveTheRest(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{"slim/keep.txt": "", "zzz.txt": ""}
	for i := 0; i < maxEntries; i++ {
		files["fat/f"+itoa(i)+".txt"] = ""
	}
	tree(t, root, files)

	got := listOf(t, root)
	for _, want := range []string{"slim/keep.txt", "zzz.txt"} {
		if !has(got, want) {
			t.Errorf("%q was starved by the fat subtree: %v", want, got)
		}
	}
	// Count what fat/ contributed below itself: its own directory line is
	// not part of the cap, and the overflow marker is allowed on top of it.
	fat := 0
	for _, e := range got {
		if strings.HasPrefix(e, "fat/") && e != "fat/" {
			fat++
		}
	}
	if fat > maxPerDir+1 {
		t.Errorf("fat/ contributed %d entries, want at most %d", fat, maxPerDir+1)
	}
}

func TestOverflowingDirectorySaysHowMuchIsHidden(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := 0; i < maxPerDir+25; i++ {
		files["fat/f"+itoa(i)+".txt"] = ""
	}
	tree(t, root, files)

	got := strings.Join(listOf(t, root), "\n")
	if !strings.Contains(got, "25 more") {
		t.Errorf("hidden entries were not accounted for:\n%s", got)
	}
}

func TestHeaderClaimsGitignoreOnlyWhenOneExists(t *testing.T) {
	bare := t.TempDir()
	tree(t, bare, map[string]string{"main.go": ""})
	if got := Describe(bare); strings.Contains(got, ".gitignore") {
		t.Errorf("claimed gitignore filtering where no .gitignore exists:\n%s", got)
	}

	repo := t.TempDir()
	tree(t, repo, map[string]string{".gitignore": "*.log\n", "main.go": ""})
	if got := Describe(repo); !strings.Contains(got, ".gitignore") {
		t.Errorf("should say the listing is gitignore-filtered:\n%s", got)
	}
}
