package fs

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

func tools(t *testing.T) (map[string]tool.Tool, string) {
	t.Helper()
	ws := t.TempDir()
	m := map[string]tool.Tool{}
	for _, tl := range New(1 << 20) {
		m[tl.Name()] = tl
	}
	return m, ws
}

// ctxFor is what every test in this file calls instead of context.Background:
// a filesystem tool has no workspace of its own any more, so a call without a
// session on the context is not a call it can run.
func ctxFor(ws string) context.Context {
	return policy.WithSession(context.Background(),
		policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: ws})
}

func run(t *testing.T, tl tool.Tool, ws string, args any) string {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tl.Call(ctxFor(ws), raw)
	if err != nil {
		t.Fatalf("%s: %v", tl.Name(), err)
	}
	return out
}

func runErr(t *testing.T, tl tool.Tool, ws string, args any) error {
	t.Helper()
	raw, _ := json.Marshal(args)
	_, err := tl.Call(ctxFor(ws), raw)
	return err
}

func TestReadWriteRoundTrip(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "a/b/hello.txt", "content": "hi\nthere\n"})
	onDisk, err := os.ReadFile(filepath.Join(ws, "a", "b", "hello.txt"))
	if err != nil {
		t.Fatalf("fs_write did not create parent directories: %v", err)
	}
	if string(onDisk) != "hi\nthere\n" {
		t.Errorf("on disk = %q", onDisk)
	}
	if got := run(t, m["fs_read"], ws, map[string]string{"path": "a/b/hello.txt"}); !strings.Contains(got, "there") {
		t.Errorf("fs_read = %q", got)
	}
}

func TestReadOffsetAndLimit(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "n.txt", "content": "1\n2\n3\n4\n5\n"})
	got := run(t, m["fs_read"], ws, map[string]any{"path": "n.txt", "offset": 2, "limit": 2})
	if strings.Contains(got, "1") || !strings.Contains(got, "2") || !strings.Contains(got, "3") || strings.Contains(got, "4") {
		t.Errorf("offset/limit window wrong: %q", got)
	}
}

func TestReadMissingFileIsAnError(t *testing.T) {
	m, ws := tools(t)
	if err := runErr(t, m["fs_read"], ws, map[string]string{"path": "nope.txt"}); err == nil {
		t.Error("reading a missing file must be a tool error")
	}
}

func TestEditReplacesOnceAndRequiresUniqueness(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "e.txt", "content": "a\nb\na\n"})
	// An ambiguous edit must fail rather than guess which occurrence to take.
	if err := runErr(t, m["fs_edit"], ws, map[string]string{"path": "e.txt", "old": "a", "new": "z"}); err == nil {
		t.Error("an edit matching twice must be refused")
	}
	run(t, m["fs_edit"], ws, map[string]any{"path": "e.txt", "old": "a", "new": "z", "replace_all": true})
	got, _ := os.ReadFile(filepath.Join(ws, "e.txt"))
	if string(got) != "z\nb\nz\n" {
		t.Errorf("replace_all = %q", got)
	}
	if err := runErr(t, m["fs_edit"], ws, map[string]string{"path": "e.txt", "old": "absent", "new": "x"}); err == nil {
		t.Error("an edit matching nothing must be refused")
	}
}

func TestListAndGlob(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "src/one.go", "content": "package x"})
	run(t, m["fs_write"], ws, map[string]string{"path": "src/two.md", "content": "# x"})
	listing := run(t, m["fs_list"], ws, map[string]string{"path": "src"})
	if !strings.Contains(listing, "one.go") || !strings.Contains(listing, "two.md") {
		t.Errorf("fs_list = %q", listing)
	}
	globbed := run(t, m["fs_glob"], ws, map[string]string{"pattern": "**/*.go"})
	if !strings.Contains(globbed, "one.go") || strings.Contains(globbed, "two.md") {
		t.Errorf("fs_glob = %q", globbed)
	}
}

func TestGrepReportsFileAndLine(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "src/a.go", "content": "package x\nfunc Target() {}\n"})
	run(t, m["fs_write"], ws, map[string]string{"path": "src/b.md", "content": "Target in markdown\n"})
	got := run(t, m["fs_grep"], ws, map[string]string{"pattern": "func Target", "path": "src"})
	if !strings.Contains(got, "a.go:2") {
		t.Errorf("fs_grep = %q, want a file:line hit", got)
	}
	filtered := run(t, m["fs_grep"], ws, map[string]string{"pattern": "Target", "glob": "**/*.md"})
	if strings.Contains(filtered, "a.go") || !strings.Contains(filtered, "b.md") {
		t.Errorf("glob filter ignored: %q", filtered)
	}
	if err := runErr(t, m["fs_grep"], ws, map[string]string{"pattern": "("}); err == nil {
		t.Error("an invalid regexp must be a tool error, not a panic")
	}
}

func TestEmptyResultsSaySoExplicitly(t *testing.T) {
	m, ws := tools(t)
	// "no matches" must be distinguishable from a clipped or failed call.
	if got := run(t, m["fs_grep"], ws, map[string]string{"pattern": "zzz"}); !strings.Contains(got, "no matches") {
		t.Errorf("fs_grep = %q, want an explicit empty-result message", got)
	}
	if got := run(t, m["fs_glob"], ws, map[string]string{"pattern": "*.nothing"}); !strings.Contains(got, "no matches") {
		t.Errorf("fs_glob = %q, want an explicit empty-result message", got)
	}
}

func TestGrepReportsFilesItCouldNotScan(t *testing.T) {
	m, ws := tools(t)
	// One line longer than the scanner's 1MB buffer. Unreported, this file
	// would be indistinguishable from a file that simply had no matches.
	if err := os.WriteFile(filepath.Join(ws, "huge.txt"), []byte(strings.Repeat("x", 2<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := run(t, m["fs_grep"], ws, map[string]string{"pattern": "zzz"})
	if !strings.Contains(got, "could not be fully scanned") {
		t.Errorf("fs_grep = %q, want the unscannable file reported rather than silently skipped", got)
	}
}

func TestReadDistinguishesAnEmptyFileFromAnEmptyWindow(t *testing.T) {
	m, ws := tools(t)
	run(t, m["fs_write"], ws, map[string]string{"path": "empty.txt", "content": ""})
	if got := run(t, m["fs_read"], ws, map[string]string{"path": "empty.txt"}); !strings.Contains(got, "empty file") {
		t.Errorf("fs_read on an empty file = %q, want it named as empty", got)
	}
	run(t, m["fs_write"], ws, map[string]string{"path": "three.txt", "content": "a\nb\nc\n"})
	got := run(t, m["fs_read"], ws, map[string]any{"path": "three.txt", "offset": 99})
	if strings.Contains(got, "empty file") {
		t.Errorf("fs_read past EOF = %q, want it distinguished from an empty file", got)
	}
	if !strings.Contains(got, "3") {
		t.Errorf("fs_read past EOF = %q, want the real line count reported", got)
	}
}

func TestReadOnlyFlags(t *testing.T) {
	m, _ := tools(t)
	for name, want := range map[string]bool{
		"fs_read": true, "fs_list": true, "fs_glob": true, "fs_grep": true,
		"fs_write": false, "fs_edit": false,
	} {
		if m[name].ReadOnly() != want {
			t.Errorf("%s.ReadOnly() = %v, want %v", name, !want, want)
		}
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	m, _ := tools(t)
	for name, tl := range m {
		if !json.Valid(tl.Schema()) {
			t.Errorf("%s has an invalid schema", name)
		}
		if tl.Description() == "" {
			t.Errorf("%s has no description", name)
		}
	}
}

// Two sessions, two roots, one set of tools: the same relative path must
// resolve differently for each.
func TestRelativePathFollowsTheSessionWorkspace(t *testing.T) {
	m, a := tools(t)
	b := t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "note.txt"), []byte("from a"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "note.txt"), []byte("from b"), 0o600); err != nil {
		t.Fatal(err)
	}
	gotA := run(t, m["fs_read"], a, map[string]any{"path": "note.txt"})
	gotB := run(t, m["fs_read"], b, map[string]any{"path": "note.txt"})
	if !strings.Contains(gotA, "from a") || !strings.Contains(gotB, "from b") {
		t.Fatalf("a = %q, b = %q: each session must read its own file", gotA, gotB)
	}
}

// Fail closed. A tool reached without a session cannot know where to work,
// and must not fall back to a configured default.
func TestFSRefusesWithoutASessionWorkspace(t *testing.T) {
	m, _ := tools(t)
	raw, _ := json.Marshal(map[string]any{"path": "note.txt"})
	if _, err := m["fs_read"].Call(context.Background(), raw); err == nil {
		t.Fatal("a call with no workspace on the context must be refused")
	}
}
