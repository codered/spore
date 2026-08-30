// Package fs implements spore's filesystem builtins. These tools do no
// confinement of their own: internal/policy decides which paths are legal,
// and both use policy.Resolve so a relative path means the same thing to the
// rule that judged the call and the tool that runs it.
package fs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	// aliased: this package is itself named fs.
	iofs "io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tool"
)

const noMatches = "no matches"

// New builds the six filesystem tools bound to a workspace. maxBytes caps a
// single file read before the registry's own output budget applies.
func New(workspace string, maxBytes int) []tool.Tool {
	b := base{ws: workspace, maxBytes: maxBytes}
	return []tool.Tool{
		readTool{b}, writeTool{b}, editTool{b},
		listTool{b}, globTool{b}, grepTool{b},
	}
}

type base struct {
	ws       string
	maxBytes int
}

func (b base) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	return policy.Resolve(b.ws, p)
}

// rel renders a path for the model relative to the workspace when possible,
// so transcripts stay short and stable.
func (b base) rel(p string) string {
	if r, err := filepath.Rel(b.ws, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		return fmt.Errorf("no arguments supplied")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// ---- fs_read ----

type readTool struct{ base }

func (readTool) Name() string { return "fs_read" }
func (readTool) Description() string {
	return "Read a text file. Returns numbered lines. Use offset and limit to page through a large file."
}
func (readTool) ReadOnly() bool { return true }
func (readTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"offset":{"type":"integer","description":"1-based first line to return."},
"limit":{"type":"integer","description":"Maximum number of lines to return."}},
"required":["path"]}`)
}

func (t readTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	if len(raw) > t.maxBytes {
		raw = raw[:t.maxBytes]
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	start := 0
	if a.Offset > 0 {
		start = a.Offset - 1
	}
	if start > len(lines) {
		start = len(lines)
	}
	end := len(lines)
	if a.Limit > 0 && start+a.Limit < end {
		end = start + a.Limit
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if b.Len() == 0 {
		return "(empty file)", nil
	}
	return b.String(), nil
}

// ---- fs_write ----

type writeTool struct{ base }

func (writeTool) Name() string { return "fs_write" }
func (writeTool) Description() string {
	return "Write a file, creating parent directories and overwriting any existing content."
}
func (writeTool) ReadOnly() bool { return false }
func (writeTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"content":{"type":"string","description":"Full new contents of the file."}},
"required":["path","content"]}`)
}

func (t writeTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Path, Content string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), t.rel(p)), nil
}

// ---- fs_edit ----

type editTool struct{ base }

func (editTool) Name() string { return "fs_edit" }
func (editTool) Description() string {
	return "Replace an exact string in a file. Fails unless the string appears exactly once, unless replace_all is set."
}
func (editTool) ReadOnly() bool { return false }
func (editTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"File path, absolute or relative to the workspace."},
"old":{"type":"string","description":"Exact text to replace, including indentation."},
"new":{"type":"string","description":"Replacement text."},
"replace_all":{"type":"boolean","description":"Replace every occurrence instead of requiring exactly one."}},
"required":["path","old","new"]}`)
}

func (t editTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old"`
		New        string `json:"new"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Old == "" {
		return "", fmt.Errorf("old must not be empty")
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	body := string(raw)
	n := strings.Count(body, a.Old)
	switch {
	case n == 0:
		return "", fmt.Errorf("%s: the text to replace was not found", t.rel(p))
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("%s: the text to replace appears %d times; pass more surrounding context or set replace_all", t.rel(p), n)
	}
	if a.ReplaceAll {
		body = strings.ReplaceAll(body, a.Old, a.New)
	} else {
		body = strings.Replace(body, a.Old, a.New, 1)
	}
	info, err := os.Stat(p)
	mode := iofs.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("replaced %d occurrence(s) in %s", n, t.rel(p)), nil
}

// ---- fs_list ----

type listTool struct{ base }

func (listTool) Name() string        { return "fs_list" }
func (listTool) Description() string { return "List the entries of a directory." }
func (listTool) ReadOnly() bool      { return true }
func (listTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"path":{"type":"string","description":"Directory path. Defaults to the workspace root."}}}`)
}

func (t listTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &a)
	}
	p, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "(empty directory)", nil
	}
	var b strings.Builder
	for _, e := range entries {
		suffix := ""
		if e.IsDir() {
			suffix = "/"
		}
		fmt.Fprintf(&b, "%s%s\n", e.Name(), suffix)
	}
	return b.String(), nil
}

// ---- shared walking ----

// walkFiles visits every regular file under root, skipping the noise
// directories that would otherwise dominate every result.
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true}

func walkFiles(root string, visit func(path string) error) error {
	return filepath.WalkDir(root, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry is skipped, not fatal
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return visit(p)
	})
}

// ---- fs_glob ----

type globTool struct{ base }

func (globTool) Name() string { return "fs_glob" }
func (globTool) Description() string {
	return "Find files by glob pattern. Supports ** for any number of directories."
}
func (globTool) ReadOnly() bool { return true }
func (globTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"pattern":{"type":"string","description":"Glob such as **/*.go."},
"path":{"type":"string","description":"Directory to search. Defaults to the workspace root."}},
"required":["pattern"]}`)
}

// globRE reuses the policy path-glob semantics so a pattern means the same
// thing in a rule and in a search.
func globRE(pattern string) (*regexp.Regexp, error) {
	return regexp.Compile(policy.GlobSource(pattern))
}

func (t globTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Pattern, Path string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	re, err := globRE(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}
	root, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	var hits []string
	err = walkFiles(root, func(p string) error {
		r := t.rel(p)
		if re.MatchString(r) || re.MatchString(filepath.Base(p)) {
			hits = append(hits, r)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return noMatches, nil
	}
	sort.Strings(hits)
	return strings.Join(hits, "\n"), nil
}

// ---- fs_grep ----

type grepTool struct{ base }

func (grepTool) Name() string { return "fs_grep" }
func (grepTool) Description() string {
	return "Search file contents with a regular expression (RE2). Returns file:line: matched-line."
}
func (grepTool) ReadOnly() bool { return true }
func (grepTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
"pattern":{"type":"string","description":"RE2 regular expression."},
"path":{"type":"string","description":"Directory to search. Defaults to the workspace root."},
"glob":{"type":"string","description":"Only search files whose path matches this glob."}},
"required":["pattern"]}`)
}

const maxGrepHits = 200

func (t grepTool) Call(_ context.Context, args json.RawMessage) (string, error) {
	var a struct{ Pattern, Path, Glob string }
	if err := decode(args, &a); err != nil {
		return "", err
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return "", fmt.Errorf("invalid pattern %q: %w", a.Pattern, err)
	}
	var filter *regexp.Regexp
	if a.Glob != "" {
		filter, err = globRE(a.Glob)
		if err != nil {
			return "", fmt.Errorf("invalid glob %q: %w", a.Glob, err)
		}
	}
	root, err := t.resolve(a.Path)
	if err != nil {
		return "", err
	}
	var hits []string
	err = walkFiles(root, func(p string) error {
		r := t.rel(p)
		if filter != nil && !filter.MatchString(r) && !filter.MatchString(filepath.Base(p)) {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for line := 1; sc.Scan(); line++ {
			if len(hits) >= maxGrepHits {
				return filepath.SkipAll
			}
			if re.MatchString(sc.Text()) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", r, line, strings.TrimSpace(sc.Text())))
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return noMatches, nil
	}
	out := strings.Join(hits, "\n")
	if len(hits) >= maxGrepHits {
		out += fmt.Sprintf("\n\n[stopped after %d matches; narrow the pattern or the path]", maxGrepHits)
	}
	return out, nil
}
