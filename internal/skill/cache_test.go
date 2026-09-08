package skill

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestCacheReloadPicksUpNewSkills(t *testing.T) {
	dir := t.TempDir()
	c := NewCache(dir)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(c.Skills()) != 0 {
		t.Fatal("a fresh directory has no skills")
	}
	write(t, dir, "release-checklist", good)
	if errs := c.Reload(); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(c.Skills()) != 1 {
		t.Fatalf("want one skill after reload, got %d", len(c.Skills()))
	}
}

func TestCacheSkillsReturnsACopy(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	c := NewCache(dir)
	c.Reload()
	got := c.Skills()
	got[0].Name = "mutated"
	if c.Skills()[0].Name != "release-checklist" {
		t.Fatal("a caller trimming for a budget must not be able to edit the shared set")
	}
}

func TestCacheKeepsSkillsOnDirectoryFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads an unreadable directory anyway")
	}
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	c := NewCache(dir)
	c.Reload()
	if len(c.Skills()) != 1 {
		t.Fatal("setup failed")
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Skip("cannot chmod in this environment")
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	errs := c.Reload()
	if len(errs) == 0 {
		t.Fatal("an unreadable directory must be reported")
	}
	if len(c.Skills()) != 1 {
		t.Fatal("the cache must keep its skills when the directory read fails")
	}
}

func TestCacheReportsPerFileErrors(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "broken", "no frontmatter here")
	c := NewCache(dir)
	c.Reload()
	if len(c.Errors()) != 1 {
		t.Fatalf("a broken skill must stay visible: %v", c.Errors())
	}
}

func TestCachesAreKeyedByDirectory(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "release-checklist", good)
	write(t, b, "aaa-first", strings.ReplaceAll(good, "release-checklist", "aaa-first"))
	cs := NewCaches()
	if got := cs.Skills(a); len(got) != 1 || got[0].Name != "release-checklist" {
		t.Fatalf("wrong skills for dir a: %+v", got)
	}
	if got := cs.Skills(b); len(got) != 1 || got[0].Name != "aaa-first" {
		t.Fatalf("wrong skills for dir b: %+v", got)
	}
}

func TestCachesEmptyDirectoryMeansNoSkills(t *testing.T) {
	cs := NewCaches()
	if got := cs.Skills(""); got != nil {
		t.Fatalf("an empty directory means skills are off, got %+v", got)
	}
	if got := cs.Errors(""); got != nil {
		t.Fatalf("an empty directory reports no errors, got %+v", got)
	}
	cs.Invalidate("") // must not panic
}

func TestCachesInvalidateSeesAWrite(t *testing.T) {
	dir := t.TempDir()
	cs := NewCaches()
	if len(cs.Skills(dir)) != 0 {
		t.Fatal("setup failed")
	}
	// Written after the first read: without Invalidate the TTL would hide it
	// from the next turn's index.
	write(t, dir, "release-checklist", good)
	if len(cs.Skills(dir)) != 0 {
		t.Fatal("the cached set is meant to be stale until it is invalidated")
	}
	cs.Invalidate(dir)
	if len(cs.Skills(dir)) != 1 {
		t.Fatal("Invalidate must make the next read see the disk")
	}
}

func TestCachesEvictIdleDirectories(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "release-checklist", good)
	write(t, b, "release-checklist", good)

	cs := NewCaches()
	now := time.Now()
	cs.now = func() time.Time { return now }

	cs.Skills(a)
	if len(cs.m) != 1 {
		t.Fatalf("after the first directory, len(m) = %d, want 1", len(cs.m))
	}

	// Advance past the idle window, then touch a different directory: the
	// miss must sweep the first one.
	now = now.Add(idleTTL + time.Second)
	cs.Skills(b)
	if len(cs.m) != 1 {
		t.Fatalf("after the second directory, len(m) = %d, want 1", len(cs.m))
	}
	if _, ok := cs.m[a]; ok {
		t.Fatal("an idle directory must be evicted")
	}
	if _, ok := cs.m[b]; !ok {
		t.Fatal("the directory just read must be cached")
	}
}

func TestCachesKeepDirectoriesStillInUse(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	write(t, a, "release-checklist", good)
	write(t, b, "release-checklist", good)

	cs := NewCaches()
	now := time.Now()
	cs.now = func() time.Time { return now }

	cs.Skills(a)
	// Most of the window passes, then a is used again, which must reset its
	// idle clock.
	now = now.Add(idleTTL - time.Minute)
	cs.Skills(a)
	now = now.Add(idleTTL - time.Minute)

	cs.Skills(b)
	if _, ok := cs.m[a]; !ok {
		t.Fatal("a directory used within the window must survive the sweep")
	}
}
