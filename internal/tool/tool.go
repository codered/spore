// Package tool holds the tool registry and the shape every builtin
// implements. The registry dispatches, recovers panics and truncates; it
// makes no policy decisions — internal/policy.Guard wraps it for that.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"sync"
	"unicode/utf8"

	"github.com/codered/spore/internal/provider"
)

// Tool is one callable operation. Name must be wire-safe for both the
// Anthropic and OpenAI tool schemas.
type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema for the tool's arguments object.
	Schema() json.RawMessage
	// ReadOnly reports whether the tool mutates anything. Read-only tools
	// may be dispatched concurrently within one assistant message.
	ReadOnly() bool
	// Call runs the tool. A returned error becomes a tool error the model
	// can read and route around; it never fails the turn.
	Call(ctx context.Context, args json.RawMessage) (string, error)
}

// Source is a dynamic set of tools whose membership changes while spore runs.
// A source is consulted on every lookup rather than copied into the registry,
// because an MCP server's tool list changes when it drops and is redialled.
// Builtins are always consulted first: a source can neither shadow nor evict
// one.
type Source interface {
	Specs() []provider.ToolSpec
	Lookup(name string) (Tool, bool)
}

// nameRE is the intersection of the Anthropic and OpenAI tool-name rules.
var nameRE = regexp.MustCompile(`\A[a-zA-Z0-9_-]{1,64}\z`)

const truncationNote = "\n\n[truncated: output exceeded the tool output budget]"

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
	// sources are dynamic tool sets consulted after the builtin map.
	sources []Source
	// maxOutput caps one result in bytes before truncation.
	maxOutput int
}

func NewRegistry(maxOutput int) *Registry {
	if maxOutput <= 0 {
		maxOutput = 30_000
	}
	return &Registry{tools: map[string]Tool{}, maxOutput: maxOutput}
}

func (r *Registry) Register(t Tool) error {
	name := t.Name()
	if !nameRE.MatchString(name) {
		return fmt.Errorf("tool name %q must match %s", name, nameRE)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.tools[name]; dup {
		return fmt.Errorf("tool %q is already registered", name)
	}
	r.tools[name] = t
	return nil
}

// AddSource attaches a dynamic tool set. Sources are consulted in the order
// they were added, after the builtins.
func (r *Registry) AddSource(s Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sources = append(r.sources, s)
}

// lookup resolves a name against the builtins first, then each source. It
// snapshots the source slice under the lock and queries outside it, so a
// source's own locking can never deadlock against the registry's.
func (r *Registry) lookup(name string) (Tool, bool) {
	r.mu.RLock()
	t, ok := r.tools[name]
	srcs := r.sources
	r.mu.RUnlock()
	if ok {
		return t, true
	}
	for _, s := range srcs {
		if t, ok := s.Lookup(name); ok {
			return t, ok
		}
	}
	return nil, false
}

// Specs returns every tool's schema, builtin and sourced, sorted by name so
// the serialised prompt prefix is stable between turns and stays cacheable
// upstream. A source whose membership changed since the last turn changes
// this list, and that invalidates the upstream cache — the accepted price of
// a tool set that can change while spore runs.
func (r *Registry) Specs() []provider.ToolSpec {
	r.mu.RLock()
	out := make([]provider.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	builtin := make(map[string]bool, len(r.tools))
	for name := range r.tools {
		builtin[name] = true
	}
	srcs := r.sources
	r.mu.RUnlock()

	for _, s := range srcs {
		for _, spec := range s.Specs() {
			if builtin[spec.Name] {
				continue // a source may not shadow a builtin
			}
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.tools))
	for name := range r.tools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReadOnly reports false for tools it does not know, so an unknown name can
// never join a concurrent batch.
func (r *Registry) ReadOnly(name string) bool {
	t, ok := r.lookup(name)
	return ok && t.ReadOnly()
}

// Result builds a tool_result block. Every result in spore is built here so
// the truncation marker and error flag are set in exactly one place.
func Result(id, content string, isErr, truncated bool) provider.Block {
	return provider.Block{
		Type:      provider.BlockToolResult,
		ID:        id,
		Content:   content,
		IsError:   isErr,
		Truncated: truncated,
	}
}

func ErrResult(id string, err error) provider.Block {
	return Result(id, err.Error(), true, false)
}

// Run dispatches one call. It never returns an error: a failure the model
// should see is returned as an error result so the agent can pick another
// path instead of losing the turn.
func (r *Registry) Run(ctx context.Context, call provider.Block) (out provider.Block) {
	t, ok := r.lookup(call.Name)
	if !ok {
		return ErrResult(call.ID, fmt.Errorf("no tool named %q is registered", call.Name))
	}

	defer func() {
		if rec := recover(); rec != nil {
			out = ErrResult(call.ID, fmt.Errorf("tool %s panicked: %v", call.Name, rec))
		}
	}()

	content, err := t.Call(ctx, call.Input)
	if err != nil {
		return ErrResult(call.ID, fmt.Errorf("tool %s: %w", call.Name, err))
	}
	if len(content) > r.maxOutput {
		// Pull the cut back to a rune boundary: maxOutput is a byte budget,
		// and slicing mid-rune hands the model a half-encoded character that
		// JSON marshalling silently turns into U+FFFD.
		cut := r.maxOutput
		for cut > 0 && !utf8.RuneStart(content[cut]) {
			cut--
		}
		return Result(call.ID, content[:cut]+truncationNote, false, true)
	}
	return Result(call.ID, content, false, false)
}
