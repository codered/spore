package policy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/codered/spore/internal/config"
)

var (
	// anyURLScheme matches the scheme of any absolute URL, so a value with a
	// scheme is left alone.
	anyURLScheme = regexp.MustCompile(`\A[A-Za-z][A-Za-z0-9+.-]*://`)
	// gitRemote matches scp-style remotes such as git@github.com:o/r.git.
	gitRemote = regexp.MustCompile(`\A[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)
	// winDrive matches a Windows drive path. spore runs on Unix only.
	winDrive = regexp.MustCompile(`\A[A-Za-z]:[\/]`)
)

// notAPath reports values that are never judged as paths, under a named key
// or not.
func notAPath(s string) bool {
	if s == "" {
		return true
	}
	return anyURLScheme.MatchString(s) || gitRemote.MatchString(s) || winDrive.MatchString(s)
}

// looksLikePath reports whether a string is path-shaped enough to be judged
// under any key at all. It is deliberately narrow: prose containing a path is
// out of scope, so anything with whitespace is not a path, and "//" keeps a
// commented-out line from being read as one.
func looksLikePath(s string) bool {
	if strings.ContainsAny(s, " \t\n\r\v\f") {
		return false
	}
	switch {
	case strings.HasPrefix(s, "//"):
		return false
	case strings.HasPrefix(s, "/"):
		return true
	case s == "~" || strings.HasPrefix(s, "~/"):
		return true
	}
	return false
}

// mcpPathKeys are the argument names whose values are treated as paths
// whatever they look like. Names are compared after lower-casing and removing
// "_" and "-", so filePath, file_path and file-path are one key. Glob and
// pattern keys are deliberately absent: they are relative to a searched
// directory that is itself checked.
var mcpPathKeys = map[string]bool{
	"path": true, "paths": true, "dir": true, "directory": true,
	"file": true, "filename": true, "filepath": true,
	"source": true, "destination": true, "root": true, "cwd": true, "uri": true,
}

// normaliseKey folds an argument name to the form mcpPathKeys is keyed by.
func normaliseKey(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "_", "")
	return strings.ReplaceAll(s, "-", "")
}

// mcpServerName pulls <server> out of mcp__<server>__<tool>. A tool name that
// does not have that shape yields "", which is not in the server table and so
// is checked with no working directory: unknown servers fail closed.
func mcpServerName(tool string) string {
	rest, ok := strings.CutPrefix(normaliseToolName(tool), "mcp__")
	if !ok {
		return ""
	}
	server, _, ok := strings.Cut(rest, "__")
	if !ok {
		return ""
	}
	return server
}

// mcpCandidates returns every path-shaped argument value in the call, in a
// deterministic order so the reported offender does not change between runs.
// A value under a path-like key is taken whatever its shape, including a
// relative path; a value found anywhere else is taken only when it looks like
// a path.
func mcpCandidates(c Call) []string {
	var v any
	if err := json.Unmarshal(c.Args, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(n any, named bool)
	walk = func(n any, named bool) {
		switch t := n.(type) {
		case string:
			if notAPath(t) {
				return
			}
			if named || looksLikePath(t) {
				out = append(out, t)
			}
		case []any:
			for _, e := range t {
				walk(e, named)
			}
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				// Named-ness is not inherited through a nested object: only a
				// string, or an array of strings, directly under the key is
				// the path the key names.
				walk(t[k], mcpPathKeys[normaliseKey(k)])
			}
		}
	}
	walk(v, false)
	return out
}

// resolveMCPCandidate turns one candidate into an absolute path on this
// machine, or explains why it cannot be one.
func resolveMCPCandidate(cwd, raw string) (string, error) {
	p := raw
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("the home directory is unknown")
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		if cwd == "" {
			return "", fmt.Errorf("it is relative and this server's working directory is unknown; send an absolute path")
		}
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), nil
}

// mcpOutsideWorkspace is the predicate behind the baseline rule
// mcp__*(any path outside workspace).
type mcpOutsideWorkspace struct{}

func (m mcpOutsideWorkspace) match(c Call, env Env) bool {
	_, bad := m.offender(c, env)
	return bad
}

func (m mcpOutsideWorkspace) explain(c Call, env Env) string {
	detail, _ := m.offender(c, env)
	return detail
}

// offender finds the first candidate that is outside the workspace or cannot
// be resolved, and describes it. The description names the offending path,
// where it resolved and the session root, and nothing else from the
// arguments.
func (m mcpOutsideWorkspace) offender(c Call, env Env) (string, bool) {
	mode, known := env.MCP[mcpServerName(c.Tool)]
	if !known {
		mode = config.MCPPathMode{Checked: true}
	}
	if !mode.Checked {
		return "", false
	}
	for _, cand := range mcpCandidates(c) {
		resolved, err := resolveMCPCandidate(mode.Cwd, cand)
		if err != nil {
			return fmt.Sprintf("%q is not a path this session may use: %v", cand, err), true
		}
		if !Inside(env.Workspace, resolved) {
			return fmt.Sprintf("%q resolves to %s, which is outside the session workspace %s", cand, resolved, env.Workspace), true
		}
	}
	return "", false
}
