package policy

import (
	"os"
	"path/filepath"
	"testing"
)

// realTemp resolves the temp dir through symlinks so comparisons are stable
// on macOS, where /tmp itself is a link.
func realTemp(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestInsideAcceptsPathsWithinWorkspace(t *testing.T) {
	ws := realTemp(t)
	if err := os.MkdirAll(filepath.Join(ws, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		ws,
		filepath.Join(ws, "a"),
		filepath.Join(ws, "a", "b", "new-file.go"), // need not exist yet
		"a/b/new-file.go",                          // relative resolves against the workspace
		filepath.Join(ws, "a", "..", "a", "b"),     // harmless traversal that stays inside
	} {
		if !Inside(ws, p) {
			t.Errorf("Inside(%q) = false, want true", p)
		}
	}
}

func TestInsideRejectsTraversal(t *testing.T) {
	ws := realTemp(t)
	for _, p := range []string{
		"..",
		"../outside.txt",
		"a/../../outside.txt",
		filepath.Join(ws, "..", "sibling", "secret"),
		"/etc/passwd",
		filepath.Dir(ws), // the parent is not inside
	} {
		if Inside(ws, p) {
			t.Errorf("Inside(%q) = true, want false", p)
		}
	}
}

func TestInsideRejectsSymlinkEscape(t *testing.T) {
	root := realTemp(t)
	ws := filepath.Join(root, "ws")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A link inside the workspace pointing at a file outside it.
	link := filepath.Join(ws, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if Inside(ws, link) {
		t.Error("a symlink out of the workspace was reported inside")
	}
	// A link to a directory outside, with a path continuing through it.
	dirLink := filepath.Join(ws, "elsewhere")
	if err := os.Symlink(outside, dirLink); err != nil {
		t.Fatal(err)
	}
	if Inside(ws, filepath.Join(dirLink, "secret.txt")) {
		t.Error("a path through a symlinked directory escaped the workspace")
	}
	if Inside(ws, filepath.Join(dirLink, "does-not-exist-yet.txt")) {
		t.Error("a not-yet-existing path through a symlinked directory escaped")
	}
}

func TestInsideRejectsPrefixLookalike(t *testing.T) {
	root := realTemp(t)
	ws := filepath.Join(root, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	// "/root/ws-evil" shares a string prefix with "/root/ws" but is not inside
	// it. A naive strings.HasPrefix check fails this test.
	if Inside(ws, filepath.Join(root, "ws-evil", "file")) {
		t.Error("a sibling directory sharing a name prefix was reported inside")
	}
}

func TestResolveExpandsHomeAndRelative(t *testing.T) {
	ws := realTemp(t)
	got, err := Resolve(ws, "sub/file.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, "sub", "file.go"); got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err = Resolve(ws, "~/x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "x.txt"); got != want {
		t.Errorf("Resolve(~) = %q, want %q", got, want)
	}
}
