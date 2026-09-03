package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

const good = `---
name: prefers-tabs
description: How the user wants Go code formatted
type: user
---

Gofmt defaults, tabs, no line-length limit.
`

func TestLoadParsesAFact(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prefers-tabs.md", good)

	facts, errs := Load(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(facts) != 1 {
		t.Fatalf("got %d facts, want 1", len(facts))
	}
	f := facts[0]
	if f.Name != "prefers-tabs" || f.Type != "user" {
		t.Fatalf("bad frontmatter: %+v", f)
	}
	if f.Description != "How the user wants Go code formatted" {
		t.Fatalf("bad description: %q", f.Description)
	}
	if f.Body != "Gofmt defaults, tabs, no line-length limit." {
		t.Fatalf("body not trimmed: %q", f.Body)
	}
	if f.Path != filepath.Join(dir, "prefers-tabs.md") {
		t.Fatalf("bad path: %q", f.Path)
	}
}

// A human edits these files by hand, so one broken file must cost exactly one
// fact -- never the whole set, and never a returned error that could fail a turn.
func TestLoadDegradesPerFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "prefers-tabs.md", good)
	write(t, dir, "broken.md", "no frontmatter at all\n")
	write(t, dir, "notes.txt", "ignored, not markdown")

	facts, errs := Load(dir)
	if len(facts) != 1 || facts[0].Name != "prefers-tabs" {
		t.Fatalf("good fact not returned alongside the broken one: %+v", facts)
	}
	if len(errs) != 1 {
		t.Fatalf("got %d errors, want exactly 1: %v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Error(), "broken.md") {
		t.Fatalf("error does not name the file: %v", errs[0])
	}
}

func TestLoadMissingDirIsNotAnError(t *testing.T) {
	facts, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if len(facts) != 0 || len(errs) != 0 {
		t.Fatalf("missing dir should be empty and quiet, got %v %v", facts, errs)
	}
}

func TestLoadSortsByName(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"zebra", "alpha", "middle"} {
		write(t, dir, n+".md", "---\nname: "+n+"\ndescription: d\ntype: user\n---\n\nbody\n")
	}
	facts, _ := Load(dir)
	if len(facts) != 3 || facts[0].Name != "alpha" || facts[2].Name != "zebra" {
		t.Fatalf("not sorted by name: %+v", facts)
	}
}

func TestLoadRejectsNameFilenameMismatch(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "actual.md", "---\nname: claimed\ndescription: d\ntype: user\n---\n\nbody\n")
	facts, errs := Load(dir)
	if len(facts) != 0 || len(errs) != 1 {
		t.Fatalf("mismatch must be rejected: %v %v", facts, errs)
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "x.md", "---\nname: x\ndescription: d\ntype: wibble\n---\n\nbody\n")
	if facts, errs := Load(dir); len(facts) != 0 || len(errs) != 1 {
		t.Fatalf("unknown type must be rejected: %v %v", facts, errs)
	}
}

func TestLoadIgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, sub, "hidden.md", good)
	facts, errs := Load(dir)
	if len(facts) != 0 || len(errs) != 0 {
		t.Fatalf("subdirectory must not become context: %v %v", facts, errs)
	}
}

func TestWriteRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "memory")
	f := Fact{Name: "likes-go", Description: "a taste", Type: "user", Body: "Go, mostly."}
	if err := Write(dir, f); err != nil {
		t.Fatal(err)
	}
	facts, errs := Load(dir)
	if len(errs) != 0 || len(facts) != 1 || facts[0].Body != "Go, mostly." {
		t.Fatalf("round trip failed: %v %v", facts, errs)
	}
}

// A name is attacker-influenced: the model chooses it. It must never be able
// to place or remove a file outside the memory directory.
func TestNamesCannotEscapeTheDirectory(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape", "a/b", "", ".", "..", "Caps", "with space", "under_score", "-lead", "trail-"} {
		if err := Write(dir, Fact{Name: bad, Description: "d", Type: "user", Body: "b"}); err == nil {
			t.Fatalf("Write accepted name %q", bad)
		}
		if err := Delete(dir, bad); err == nil {
			t.Fatalf("Delete accepted name %q", bad)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("rejected writes left files behind: %v", entries)
	}
}

func TestWriteRequiresDescriptionAndType(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Fact{Name: "ok", Type: "user"}); err == nil {
		t.Fatal("empty description accepted")
	}
	if err := Write(dir, Fact{Name: "ok", Description: "d", Type: "nope"}); err == nil {
		t.Fatal("bad type accepted")
	}
	if err := Write(dir, Fact{Name: "ok", Description: "one\ntwo", Type: "user"}); err == nil {
		t.Fatal("multi-line description accepted")
	}
}

func TestDeleteRemovesAndReportsMissing(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "gone.md", "---\nname: gone\ndescription: d\ntype: user\n---\n\nb\n")
	if err := Delete(dir, "gone"); err != nil {
		t.Fatal(err)
	}
	if facts, _ := Load(dir); len(facts) != 0 {
		t.Fatal("fact still present")
	}
	if err := Delete(dir, "gone"); err == nil {
		t.Fatal("deleting a missing fact should report it")
	}
}

func TestCacheReloadsAfterWrite(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatal(errs)
	}
	if len(c.Facts()) != 0 {
		t.Fatal("cache should start empty")
	}
	if err := Write(dir, Fact{Name: "new", Description: "d", Type: "user", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(c.Facts()) != 0 {
		t.Fatal("cache must not see a write until it reloads")
	}
	c.Reload()
	if len(c.Facts()) != 1 {
		t.Fatal("reload did not pick up the new fact")
	}
}

func TestCacheFactsIsACopy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.md", "---\nname: a\ndescription: d\ntype: user\n---\n\nb\n")
	c := NewCache(dir)
	c.Reload()
	got := c.Facts()
	got[0].Body = "mutated"
	if c.Facts()[0].Body != "b" {
		t.Fatal("caller mutated the cache's backing array")
	}
}
