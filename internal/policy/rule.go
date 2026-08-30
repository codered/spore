// Package policy decides whether a tool call may run. Every call resolves to
// allow, ask or deny by matching ordered rules against the tool name AND its
// arguments. Deny is evaluated first and is absolute: no approval, learned
// rule or trust profile can override it.
//
// Rule grammar:
//
//	<tool-glob>                              e.g. fs_read, web.*, mcp__*
//	<tool-glob>(path outside workspace)      path arguments leaving the workspace
//	<tool-glob>(path matches <glob>, ...)    path arguments matching any glob
//	<tool-glob>(matches <text>, ...)         any string argument containing any text
//
// Tool globs treat "." and "_" as the same separator, so the spec's "fs.read"
// and the wire name "fs_read" are one rule.
package policy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type Decision string

const (
	DecisionAllow Decision = "allow"
	DecisionAsk   Decision = "ask"
	DecisionDeny  Decision = "deny"
)

// Profile is the trust level of the client that started the turn. Rulesets
// may differ per profile; deny never does.
type Profile string

const (
	ProfileLocal  Profile = "local"
	ProfileRemote Profile = "remote"
)

// Call is one tool invocation under evaluation.
type Call struct {
	Tool string
	Args json.RawMessage
}

// Env is the evaluation environment shared by every rule.
type Env struct{ Workspace string }

// Rule is one parsed policy line.
type Rule struct {
	Decision Decision
	// Raw is the rule exactly as written in config, used in audit records,
	// spans and the message the model sees when a call is denied.
	Raw string

	tool *regexp.Regexp
	pred predicate
}

type predicate interface {
	match(c Call, env Env) bool
}

// ParseRule compiles one rule string under the given decision.
func ParseRule(d Decision, src string) (Rule, error) {
	raw := strings.TrimSpace(src)
	if raw == "" {
		return Rule{}, fmt.Errorf("empty policy rule")
	}
	toolSrc, predSrc := raw, ""
	if i := strings.Index(raw, "("); i >= 0 {
		if !strings.HasSuffix(raw, ")") {
			return Rule{}, fmt.Errorf("policy rule %q: unbalanced parentheses", raw)
		}
		toolSrc = strings.TrimSpace(raw[:i])
		predSrc = strings.TrimSpace(raw[i+1 : len(raw)-1])
	}
	if toolSrc == "" {
		return Rule{}, fmt.Errorf("policy rule %q: missing tool name", raw)
	}
	re, err := compileToolGlob(toolSrc)
	if err != nil {
		return Rule{}, fmt.Errorf("policy rule %q: %w", raw, err)
	}
	r := Rule{Decision: d, Raw: raw, tool: re}
	if predSrc != "" {
		p, err := parsePredicate(predSrc)
		if err != nil {
			return Rule{}, fmt.Errorf("policy rule %q: %w", raw, err)
		}
		r.pred = p
	}
	return r, nil
}

// Match reports whether the rule applies to this call.
func (r Rule) Match(c Call, env Env) bool {
	if !r.tool.MatchString(normaliseToolName(c.Tool)) {
		return false
	}
	if r.pred == nil {
		return true
	}
	return r.pred.match(c, env)
}

func normaliseToolName(s string) string { return strings.ReplaceAll(s, ".", "_") }

// compileToolGlob turns a tool glob into an anchored regexp. "*" matches any
// run of characters; there is no path structure in a tool name.
func compileToolGlob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	for _, ch := range normaliseToolName(g) {
		if ch == '*' {
			b.WriteString(`.*`)
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(ch)))
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

// compilePathGlob understands "**" (any number of segments), "*" (within one
// segment) and "?". A leading "**/" also matches a bare filename, so
// "**/.env" matches ".env".
func compilePathGlob(g string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString(`\A`)
	for i := 0; i < len(g); i++ {
		switch g[i] {
		case '*':
			if i+1 < len(g) && g[i+1] == '*' {
				i++
				if i+1 < len(g) && g[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
					continue
				}
				b.WriteString(`.*`)
				continue
			}
			b.WriteString(`[^/]*`)
		case '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	b.WriteString(`\z`)
	return regexp.Compile(b.String())
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func parsePredicate(src string) (predicate, error) {
	switch {
	case src == "path outside workspace":
		return outsideWorkspace{}, nil
	case strings.HasPrefix(src, "path matches "):
		globs := splitList(strings.TrimPrefix(src, "path matches "))
		if len(globs) == 0 {
			return nil, fmt.Errorf("predicate %q: no globs listed", src)
		}
		var res []*regexp.Regexp
		for _, g := range globs {
			re, err := compilePathGlob(g)
			if err != nil {
				return nil, fmt.Errorf("predicate %q: bad glob %q: %w", src, g, err)
			}
			res = append(res, re)
		}
		return pathMatches{res}, nil
	case strings.HasPrefix(src, "matches "):
		needles := splitList(strings.TrimPrefix(src, "matches "))
		if len(needles) == 0 {
			return nil, fmt.Errorf("predicate %q: nothing to match", src)
		}
		for i, n := range needles {
			needles[i] = normaliseSpace(n)
		}
		return argMatches{needles}, nil
	default:
		return nil, fmt.Errorf("unknown predicate %q (want \"path outside workspace\", \"path matches ...\" or \"matches ...\")", src)
	}
}

var spaceRun = regexp.MustCompile(`\s+`)

// normaliseSpace collapses runs of whitespace so "rm    -rf /" matches a rule
// written "rm -rf /". Quoting and chaining are handled by searching every
// string argument for the needle rather than parsing the shell.
func normaliseSpace(s string) string { return spaceRun.ReplaceAllString(s, " ") }

// pathArgKeys are the argument names a path predicate inspects. A tool that
// takes a path must name it one of these.
var pathArgKeys = map[string]bool{"path": true, "paths": true, "dir": true, "file": true}

// argPaths returns every path-shaped argument value in the call.
func argPaths(c Call) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(c.Args, &m); err != nil {
		return nil
	}
	var out []string
	for k, raw := range m {
		if !pathArgKeys[k] {
			continue
		}
		var one string
		if err := json.Unmarshal(raw, &one); err == nil {
			out = append(out, one)
			continue
		}
		var many []string
		if err := json.Unmarshal(raw, &many); err == nil {
			out = append(out, many...)
		}
	}
	return out
}

// argStrings returns every string value in the arguments, at any depth.
func argStrings(c Call) []string {
	var v any
	if err := json.Unmarshal(c.Args, &v); err != nil {
		return nil
	}
	var out []string
	var walk func(any)
	walk = func(n any) {
		switch t := n.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return out
}

type outsideWorkspace struct{}

func (outsideWorkspace) match(c Call, env Env) bool {
	paths := argPaths(c)
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !Inside(env.Workspace, p) {
			return true
		}
	}
	return false
}

type pathMatches struct{ globs []*regexp.Regexp }

func (p pathMatches) match(c Call, env Env) bool {
	for _, raw := range argPaths(c) {
		candidates := []string{raw}
		if resolved, err := Resolve(env.Workspace, raw); err == nil {
			candidates = append(candidates, resolved)
		}
		for _, cand := range candidates {
			for _, re := range p.globs {
				if re.MatchString(cand) {
					return true
				}
			}
		}
	}
	return false
}

type argMatches struct{ needles []string }

func (a argMatches) match(c Call, _ Env) bool {
	for _, s := range argStrings(c) {
		s = normaliseSpace(s)
		for _, n := range a.needles {
			if strings.Contains(s, n) {
				return true
			}
		}
	}
	return false
}

