package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codered/spore/internal/provider"
)

// Helper documents one typed function of the spore package.
type Helper struct {
	Signature string
	Doc       string
	// Tool is the tool the helper calls; the catalogue leaves it out,
	// because the typed helper already covers it.
	Tool string
}

// Helpers is the typed half of the spore package, in the order the prompt
// lists them. The functions themselves are built in surface.
var Helpers = []Helper{
	{"func Fetch(url string) (string, error)", "GET an http(s) URL; HTML comes back as text, anything else as the raw body.", "web_fetch"},
	{"func Search(query string, count int) (string, error)", "Web search; count 0 means the default.", "web_search"},
	{"func ReadFile(path string) (string, error)", "Read a file, relative to the workspace or absolute.", "fs_read"},
	{"func WriteFile(path, content string) error", "Create or replace a file.", "fs_write"},
	{"func EditFile(path, old, new string) error", "Replace the one occurrence of old with new.", "fs_edit"},
	{"func List(path string) (string, error)", `List a directory; "" is the workspace root.`, "fs_list"},
	{"func Glob(pattern string) (string, error)", "Find files by glob, e.g. **/*.go.", "fs_glob"},
	{"func Grep(pattern, glob string) (string, error)", `Search file contents by RE2 regexp; glob "" searches every file.`, "fs_grep"},
	{"func Shell(command string) (string, error)", "Run a bash command in the workspace.", "shell_exec"},
	{"func Recall(query string) (string, error)", "Search past conversations.", "recall_search"},
	{"func Call(tool string, args map[string]any) (string, error)", "Call any tool below by name with its JSON arguments.", ""},
}

// Reference renders the system-prompt section that teaches the model to act
// through go_run. It is a pure function of specs, and specs arrive sorted, so
// the section is byte-stable while the tool set is, which keeps it inside
// the cached prompt prefix.
func Reference(specs []provider.ToolSpec) string {
	covered := map[string]bool{ToolName: true}
	for _, h := range Helpers {
		if h.Tool != "" {
			covered[h.Tool] = true
		}
	}

	var b strings.Builder
	b.WriteString("\n\n## Acting through go_run\n\n")
	b.WriteString("You act by writing Go. Call go_run with one complete program " +
		"(package main, imports, func main) that does the whole task, every " +
		"lookup included, and prints the final result. Its output is all you " +
		"see, so print what you need and nothing else. Prefer one program " +
		"over several. If a program fails, read the error, fix the program " +
		"and run it again.\n\n")
	b.WriteString("Each program starts fresh: nothing survives between runs. " +
		"A spore.* call that policy refuses returns an error; handle it.\n\n")
	b.WriteString("Importable packages: " + strings.Join(Allowed, ", ") + ", and spore. " +
		"Nothing else — no os, net/http or os/exec; use spore.* for files, network and shell.\n\n")

	b.WriteString("### package spore\n\n")
	for _, h := range Helpers {
		fmt.Fprintf(&b, "- `%s` — %s\n", h.Signature, h.Doc)
	}

	b.WriteString("\n### Other tools (via spore.Call)\n\n")
	n := 0
	for _, s := range specs {
		if covered[s.Name] {
			continue
		}
		n++
		fmt.Fprintf(&b, "- `%s` — %s Arguments: `%s`\n", s.Name, oneLine(s.Description), compact(s.Schema))
	}
	if n == 0 {
		b.WriteString("(none)\n")
	}
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func compact(schema json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, schema); err != nil {
		return string(schema)
	}
	return buf.String()
}
