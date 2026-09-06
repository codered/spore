package skill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const good = "---\nname: release-checklist\ndescription: How to cut a release\n---\n\nTag from master only.\n"

func TestLoadSortsAndParses(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	write(t, dir, "aaa-first", strings.ReplaceAll(good, "release-checklist", "aaa-first"))

	skills, errs := Load(dir)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(skills) != 2 || skills[0].Name != "aaa-first" || skills[1].Name != "release-checklist" {
		t.Fatalf("want two skills sorted by name, got %+v", skills)
	}
	if skills[1].Description != "How to cut a release" || skills[1].Body != "Tag from master only." {
		t.Fatalf("bad parse: %+v", skills[1])
	}
	if skills[1].Path != filepath.Join(dir, "release-checklist", "SKILL.md") {
		t.Fatalf("Path must name the file that was read: %q", skills[1].Path)
	}
}

func TestLoadMissingDirIsEmpty(t *testing.T) {
	skills, errs := Load(filepath.Join(t.TempDir(), "nope"))
	if len(skills) != 0 || len(errs) != 0 {
		t.Fatalf("want no skills and no error, got %v %v", skills, errs)
	}
}

func TestLoadNameMustMatchDirectory(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "other-name", good)
	skills, errs := Load(dir)
	if len(skills) != 0 || len(errs) != 1 {
		t.Fatalf("want one error and no skill, got %v %v", skills, errs)
	}
	if !strings.Contains(errs[0].Error(), "does not match") {
		t.Fatalf("error should name the mismatch: %v", errs[0])
	}
}

func TestLoadOneBrokenSkillCostsOneSkill(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	write(t, dir, "broken", "no frontmatter here")
	skills, errs := Load(dir)
	if len(skills) != 1 || len(errs) != 1 {
		t.Fatalf("want one skill and one error, got %v %v", skills, errs)
	}
}

func TestLoadIgnoresLooseFilesAndEmptyDirectories(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	if err := os.WriteFile(filepath.Join(dir, "notes.md"), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}
	skills, errs := Load(dir)
	if len(skills) != 1 || len(errs) != 0 {
		t.Fatalf("only a directory holding a SKILL.md is a skill: %v %v", skills, errs)
	}
}

func TestValidNameRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"../escape", "a/b", "Upper", "", ".", "trailing-", "dot.name"} {
		if err := ValidName(bad); err == nil {
			t.Fatalf("ValidName(%q) should fail", bad)
		}
	}
	if err := ValidName("release-checklist"); err != nil {
		t.Fatalf("ValidName rejected a good name: %v", err)
	}
}

func TestReadRoundTripsWrite(t *testing.T) {
	dir := t.TempDir()
	in := Skill{Name: "release-checklist", Description: "How to cut a release", Body: "Tag from master only."}
	if err := Write(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := Read(dir, "release-checklist")
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != in.Name || out.Description != in.Description || out.Body != in.Body {
		t.Fatalf("round trip changed the skill: %+v", out)
	}
}

func TestWriteLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Skill{Name: "release-checklist", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "release-checklist"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "SKILL.md" {
		t.Fatalf("want only SKILL.md left behind, got %v", entries)
	}
}

func TestWriteRejectsBadName(t *testing.T) {
	if err := Write(t.TempDir(), Skill{Name: "../evil", Description: "d", Body: "b"}); err == nil {
		t.Fatal("Write must reject a name that is not kebab-case")
	}
}

func TestWriteRejectsIncompleteSkill(t *testing.T) {
	dir := t.TempDir()
	if err := Write(dir, Skill{Name: "a-skill", Description: "", Body: "b"}); err == nil {
		t.Fatal("a skill with no description must be rejected: the index is all the model sees")
	}
	if err := Write(dir, Skill{Name: "a-skill", Description: "d", Body: "  "}); err == nil {
		t.Fatal("a skill with no body must be rejected")
	}
	if err := Write(dir, Skill{Name: "a-skill", Description: "one\ntwo", Body: "b"}); err == nil {
		t.Fatal("a multi-line description must be rejected: it is one line of the index")
	}
}

func TestReadRejectsBadName(t *testing.T) {
	if _, err := Read(t.TempDir(), "../../etc"); err == nil {
		t.Fatal("Read must reject a name that is not kebab-case")
	}
}

func TestReadMissingSkillNamesIt(t *testing.T) {
	_, err := Read(t.TempDir(), "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("the error must name the skill asked for: %v", err)
	}
}

func TestExists(t *testing.T) {
	dir := t.TempDir()
	if Exists(dir, "release-checklist") {
		t.Fatal("nothing is installed yet")
	}
	write(t, dir, "release-checklist", good)
	if !Exists(dir, "release-checklist") {
		t.Fatal("an installed skill must be reported")
	}
	if Exists(dir, "../evil") {
		t.Fatal("Exists must not follow a traversal name")
	}
}
