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
	{"func Help(tool string) (string, error)", "A tool's full description and JSON schema, with what each argument means.", ""},
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

	b.WriteString("The interpreter is Go 1.21 without three builtins: min, max and clear " +
		"are undefined, so write them out, and range over an integer " +
		"(for i := range n) is not supported; use a counted loop. When you do not know a JSON " +
		"API's exact shape, decode into map[string]any, or print a slice of the body " +
		"first, instead of guessing struct types.\n\n")
	b.WriteString("### package spore\n\n")
	for _, h := range Helpers {
		fmt.Fprintf(&b, "- `%s` — %s\n", h.Signature, h.Doc)
	}

	b.WriteString("\n### Other tools (via spore.Call)\n\n")
	b.WriteString("Each line gives a tool's arguments; ? marks an optional one and a|b the allowed values. " +
		"When a line is not enough, print spore.Help(name) for the full schema.\n\n")
	n := 0
	for _, s := range specs {
		if covered[s.Name] {
			continue
		}
		n++
		fmt.Fprintf(&b, "- `%s` — %s\n", signature(s.Name, s.Schema), oneLine(s.Description))
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

// Docs is what spore.Help returns, by tool name: the description and the full
// schema the prompt leaves out. The kernel sends it with each program, so a
// lookup is answered in the child and is not a tool call.
func Docs(specs []provider.ToolSpec) map[string]string {
	d := make(map[string]string, len(specs))
	for _, s := range specs {
		d[s.Name] = oneLine(s.Description) + "\n\nArguments (JSON schema): " + compact(s.Schema)
	}
	return d
}

// signature renders a schema as name(arg: type, opt?: type), in the schema's
// own property order. Enums show their values; anything else shows its type.
func signature(name string, schema json.RawMessage) string {
	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(schema, &s); err != nil || len(s.Properties) == 0 {
		return name + "()"
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	var args []string
	for _, p := range orderedProperties(s.Properties) {
		var v struct {
			Type any   `json:"type"`
			Enum []any `json:"enum"`
		}
		_ = json.Unmarshal(p.raw, &v)
		typ := "any"
		switch t := v.Type.(type) {
		case string:
			typ = t
		case []any:
			parts := make([]string, len(t))
			for i, x := range t {
				parts[i] = fmt.Sprint(x)
			}
			typ = strings.Join(parts, "|")
		}
		if len(v.Enum) > 0 {
			vals := make([]string, len(v.Enum))
			for i, x := range v.Enum {
				vals[i] = fmt.Sprint(x)
			}
			typ = strings.Join(vals, "|")
		}
		opt := "?"
		if required[p.name] {
			opt = ""
		}
		args = append(args, p.name+opt+": "+typ)
	}
	return name + "(" + strings.Join(args, ", ") + ")"
}

type property struct {
	name string
	raw  json.RawMessage
}

// orderedProperties reads a JSON object's members in the order they are
// written, which a map would lose.
func orderedProperties(obj json.RawMessage) []property {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var out []property
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			return out
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return out
		}
		out = append(out, property{name: fmt.Sprint(k), raw: raw})
	}
	return out
}
