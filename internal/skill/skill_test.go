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

// Skills written for other agents use YAML block scalars and extra keys. The
// parser reads that subset rather than rejecting the whole skill over it.
func TestParseFrontmatterFromOtherAgents(t *testing.T) {
	cases := []struct {
		name, head, want string
	}{
		{"folded", "description: >\n  Use this when\n  cutting a release.\n", "Use this when cutting a release."},
		{"folded strip", "description: >-\n  Use this when\n\n  cutting a release.\n", "Use this when cutting a release."},
		{"literal", "description: |\n  Use this when\n  cutting a release.\n", "Use this when cutting a release."},
		{"plain continuation", "description: Use this when\n  cutting a release.\n", "Use this when cutting a release."},
		{"double quoted", "description: \"Use this: when cutting a release.\"\n", "Use this: when cutting a release."},
		{"single quoted", "description: 'It''s for releases.'\n", "It's for releases."},
		{"extra keys", "license: MIT\nallowed-tools: Bash, Read\ndescription: How to cut a release\nmetadata:\n  version: 1.2\n  tags: [a, b]\n", "How to cut a release"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := parse("---\nname: release-checklist\n" + c.head + "---\n\nTag from master only.\n")
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if s.Name != "release-checklist" || s.Description != c.want || s.Body != "Tag from master only." {
				t.Fatalf("got %+v, want description %q", s, c.want)
			}
		})
	}
}

func TestParseRejectsMalformedFrontmatter(t *testing.T) {
	for name, head := range map[string]string{
		"indented first line": "  name: release-checklist\ndescription: x\n",
		"not key value":       "name: release-checklist\ndescription: x\njust words\n",
		"missing description": "name: release-checklist\nlicense: MIT\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse("---\n" + head + "---\n\nbody\n"); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func skillWithFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "release-checklist", good)
	root := filepath.Join(dir, "release-checklist")
	for path, body := range map[string]string{
		"references/steps.md":  "1. tag\n",
		"template.md":          "# Release\n",
		".SKILL-123.md":        "orphan",
		".git/config":          "hidden",
		"references/.notes.md": "hidden",
	} {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestFilesListsSupportingFiles(t *testing.T) {
	dir := skillWithFiles(t)
	files, err := Files(dir, "release-checklist")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"references/steps.md", "template.md"}
	if strings.Join(files, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v, want %v (no SKILL.md, no dotfiles)", files, want)
	}
}

func TestReadFileReadsInsideTheSkill(t *testing.T) {
	dir := skillWithFiles(t)
	got, err := ReadFile(dir, "release-checklist", "references/steps.md")
	if err != nil || got != "1. tag\n" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestReadFileStaysInsideTheSkill(t *testing.T) {
	dir := skillWithFiles(t)
	write(t, dir, "other-skill", strings.ReplaceAll(good, "release-checklist", "other-skill"))
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "release-checklist", "link.md")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"../other-skill/SKILL.md",
		"references/../../other-skill/SKILL.md",
		outside,
		"link.md",
		".git/config",
		"references",
		"",
	} {
		if got, err := ReadFile(dir, "release-checklist", rel); err == nil {
			t.Errorf("%q: want refusal, got %q", rel, got)
		}
	}
}
