package kernel

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

var update = flag.Bool("update", false, "rewrite golden files")

func referenceSpecs() []provider.ToolSpec {
	spec := func(name, desc, schema string) provider.ToolSpec {
		return provider.ToolSpec{Name: name, Description: desc, Schema: json.RawMessage(schema)}
	}
	return []provider.ToolSpec{
		spec("fs_read", "Read a file.", `{"type":"object","properties":{"path":{"type":"string"}}}`),
		spec("go_run", "Run a Go program.", `{"type":"object"}`),
		spec("mcp__gh__list_prs", "List pull requests.", `{
  "type": "object",
  "properties": {"repo": {"type": "string"}}
}`),
		spec("schedule_list", "List scheduled jobs.", `{"type":"object","properties":{}}`),
	}
}

func TestReferenceMatchesGolden(t *testing.T) {
	got := Reference(referenceSpecs())
	golden := filepath.Join("testdata", "reference.golden")
	if *update {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("Reference changed; diff against %s or rerun with -update:\n%s", golden, got)
	}
}

func TestReferenceCatalogue(t *testing.T) {
	got := Reference(referenceSpecs())
	for _, want := range []string{
		"## Acting through go_run",
		"func Fetch(url string) (string, error)",
		"encoding/json",
		`mcp__gh__list_prs`,
		`{"type":"object","properties":{"repo":{"type":"string"}}}`,
		"schedule_list",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("reference lacks %q", want)
		}
	}
	_, catalogue, ok := strings.Cut(got, "### Other tools")
	if !ok {
		t.Fatal("reference has no catalogue section")
	}
	for _, unwanted := range []string{"go_run", "fs_read"} {
		if strings.Contains(catalogue, unwanted) {
			t.Errorf("catalogue lists %s, which is covered elsewhere", unwanted)
		}
	}
}

func TestReferenceIsDeterministic(t *testing.T) {
	first, second := Reference(referenceSpecs()), Reference(referenceSpecs())
	if first != second {
		t.Error("two calls over the same specs differ, which breaks the cached prefix")
	}
}

// yaegi v0.16.1 lacks the min, max and clear builtins and crashes on range
// over an int. A model writing modern Go reaches for all four, and each
// costs a round trip, so the prompt must say so up front.
func TestReferenceWarnsAboutMissingLanguageFeatures(t *testing.T) {
	got := Reference(referenceSpecs())
	for _, want := range []string{"min", "max", "clear", "range over an integer", "map[string]any"} {
		if !strings.Contains(got, want) {
			t.Errorf("reference does not mention %q", want)
		}
	}
}
