package modelcmd

import (
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
)

func view() models.View {
	return models.View{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "b/two", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "gone/x", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "b/two", Default: "b/two"},
		},
		Groups: []models.Group{
			{Provider: "a", Refs: []string{"a/one", "a/three"}},
			{Provider: "b", Refs: []string{"b/two"}},
			{Provider: "down", Refs: []string{}, Error: "connection refused"},
		},
	}
}

func TestOptionsOrderSelectedDefaultThenTheRest(t *testing.T) {
	opts := Options(view(), "chat")
	var refs []string
	for _, o := range opts {
		refs = append(refs, o.Ref)
	}
	if strings.Join(refs, ",") != "b/two,a/one,a/three" {
		t.Fatalf("order = %v", refs)
	}
	if !opts[0].Selected || !opts[1].Default || opts[2].Selected || opts[2].Default {
		t.Fatalf("marks = %+v", opts)
	}
}

func TestAnUnavailableSelectionIsShownButNotChoosable(t *testing.T) {
	opts := Options(view(), "title")
	if opts[0].Ref != "gone/x" || opts[0].Available || opts[0].Choosable() {
		t.Fatalf("first = %+v, want gone/x shown, unavailable, not choosable", opts[0])
	}
	if !opts[1].Default || !opts[1].Choosable() {
		t.Fatalf("default = %+v, want choosable", opts[1])
	}
}

func TestRenderMarksAndNumbers(t *testing.T) {
	out := Render(view())
	for _, want := range []string{
		"chat", "-> b/two", "* a/one", "(this session",
		"   1 -> b/two",
		"   2    a/one *",
		"gone/x (unavailable)",
		"! down: connection refused",
		"/model <operation> <number>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Render lacks %q:\n%s", want, out)
		}
	}
}

func TestParse(t *testing.T) {
	if op, n, err := Parse(nil); op != "" || n != 0 || err != nil {
		t.Fatalf("Parse(nil) = %q %d %v", op, n, err)
	}
	if op, n, err := Parse([]string{"title", "2"}); op != "title" || n != 2 || err != nil {
		t.Fatalf("Parse(title 2) = %q %d %v", op, n, err)
	}
	for _, bad := range [][]string{{"chat"}, {"nope", "1"}, {"chat", "x"}, {"chat", "0"}, {"chat", "1", "2"}, {"chat", "a/one"}} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%v) succeeded", bad)
		}
	}
}

func TestPick(t *testing.T) {
	v := view()
	if ref, err := Pick(v, "chat", 3); ref != "a/three" || err != nil {
		t.Fatalf("Pick(chat 3) = %q %v", ref, err)
	}
	if _, err := Pick(v, "chat", 4); err == nil {
		t.Fatal("out of range accepted")
	}
	if _, err := Pick(v, "title", 1); err == nil {
		t.Fatal("unavailable option accepted")
	}
}

func TestConfirmSaysWhereItApplies(t *testing.T) {
	if got := Confirm(view(), "chat"); !strings.Contains(got, "this session") {
		t.Fatalf("chat confirm = %q", got)
	}
	if got := Confirm(view(), "compaction"); !strings.Contains(got, "every session") || !strings.Contains(got, "default") {
		t.Fatalf("compaction confirm = %q", got)
	}
}
