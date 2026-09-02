package tool

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/codered/spore/internal/provider"
)

type fake struct {
	name     string
	readOnly bool
	fn       func(ctx context.Context, args json.RawMessage) (string, error)
}

func (f fake) Name() string            { return f.name }
func (f fake) Description() string     { return "fake " + f.name }
func (f fake) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f fake) ReadOnly() bool          { return f.readOnly }
func (f fake) Call(ctx context.Context, args json.RawMessage) (string, error) {
	return f.fn(ctx, args)
}

func echoTool(name string, readOnly bool) fake {
	return fake{name: name, readOnly: readOnly, fn: func(_ context.Context, args json.RawMessage) (string, error) {
		return string(args), nil
	}}
}

func call(name, id, args string) provider.Block {
	return provider.Block{Type: provider.BlockToolUse, ID: id, Name: name, Input: json.RawMessage(args)}
}

func TestRunDispatchesAndTagsResult(t *testing.T) {
	r := NewRegistry(100)
	if err := r.Register(echoTool("fs_read", true)); err != nil {
		t.Fatal(err)
	}
	got := r.Run(context.Background(), call("fs_read", "call-1", `{"path":"x"}`))
	if got.Type != provider.BlockToolResult {
		t.Errorf("Type = %q, want tool_result", got.Type)
	}
	if got.ID != "call-1" {
		t.Errorf("ID = %q, want the tool_use id echoed back", got.ID)
	}
	if got.Content != `{"path":"x"}` || got.IsError {
		t.Errorf("Content = %q IsError = %v", got.Content, got.IsError)
	}
}

func TestUnknownToolIsAToolErrorNotACrash(t *testing.T) {
	r := NewRegistry(100)
	got := r.Run(context.Background(), call("nope", "call-1", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "nope") {
		t.Errorf("got %+v, want an error result naming the tool", got)
	}
}

func TestToolErrorIsReportedToTheModel(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(fake{name: "boom", fn: func(context.Context, json.RawMessage) (string, error) {
		return "", errors.New("disk on fire")
	}})
	got := r.Run(context.Background(), call("boom", "c", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "disk on fire") {
		t.Errorf("got %+v, want the error text returned as a tool error", got)
	}
}

func TestPanicIsRecoveredAsAToolError(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(fake{name: "panicky", fn: func(context.Context, json.RawMessage) (string, error) {
		panic("nil map write")
	}})
	got := r.Run(context.Background(), call("panicky", "c", `{}`))
	if !got.IsError || !strings.Contains(got.Content, "nil map write") {
		t.Errorf("got %+v, want the panic recovered into a tool error", got)
	}
}

func TestOutputIsTruncatedAndMarked(t *testing.T) {
	r := NewRegistry(20)
	_ = r.Register(fake{name: "big", readOnly: true, fn: func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("x", 500), nil
	}})
	got := r.Run(context.Background(), call("big", "c", `{}`))
	if !got.Truncated {
		t.Error("Truncated = false, want the result marked as clipped")
	}
	if want := strings.Repeat("x", 20) + truncationNote; got.Content != want {
		t.Errorf("Content = %q, want exactly the first 20 bytes plus the marker", got.Content)
	}
	if !strings.Contains(got.Content, "truncated") {
		t.Error("the model must be able to see the output was clipped, not empty")
	}
}

func TestTruncationDoesNotSplitARune(t *testing.T) {
	r := NewRegistry(20)
	// 19 ASCII bytes then a 3-byte rune: a byte-index cut at 20 would land
	// inside the euro sign and hand the model half a character.
	_ = r.Register(fake{name: "uni", readOnly: true, fn: func(context.Context, json.RawMessage) (string, error) {
		return strings.Repeat("a", 19) + "€uro", nil
	}})
	got := r.Run(context.Background(), call("uni", "c", `{}`))
	if !got.Truncated {
		t.Fatal("Truncated = false, want the result marked as clipped")
	}
	body := strings.TrimSuffix(got.Content, truncationNote)
	if !utf8.ValidString(body) {
		t.Errorf("truncated body is not valid UTF-8: %q", body)
	}
	if body != strings.Repeat("a", 19) {
		t.Errorf("body = %q, want the cut pulled back to the rune boundary", body)
	}
}

func TestSpecsAndReadOnly(t *testing.T) {
	r := NewRegistry(100)
	_ = r.Register(echoTool("fs_read", true))
	_ = r.Register(echoTool("fs_write", false))
	specs := r.Specs()
	if len(specs) != 2 {
		t.Fatalf("len(Specs) = %d, want 2", len(specs))
	}
	// Specs are sorted so the prompt prefix stays byte-identical between
	// turns, which keeps provider prompt caching effective.
	if specs[0].Name != "fs_read" || specs[1].Name != "fs_write" {
		t.Errorf("Specs are not sorted by name: %v, %v", specs[0].Name, specs[1].Name)
	}
	if !r.ReadOnly("fs_read") || r.ReadOnly("fs_write") {
		t.Error("ReadOnly is wrong")
	}
	// An unknown tool must never be reported read-only: that would let it
	// join a concurrent batch.
	if r.ReadOnly("unknown") {
		t.Error("ReadOnly(unknown) = true, want false")
	}
}

func TestRegisterRejectsDuplicatesAndBadNames(t *testing.T) {
	r := NewRegistry(100)
	if err := r.Register(echoTool("fs_read", true)); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(echoTool("fs_read", true)); err == nil {
		t.Error("Register accepted a duplicate name")
	}
	// Providers constrain tool names to [a-zA-Z0-9_-]{1,64}; a dotted name
	// would be rejected upstream at request time.
	if err := r.Register(echoTool("fs.read", true)); err == nil {
		t.Error("Register accepted a name providers will reject")
	}
}

// mapSource is a Source whose membership the test changes between calls,
// which is the whole point of the seam.
type mapSource struct {
	mu    sync.Mutex
	tools map[string]Tool
}

func newMapSource(ts ...Tool) *mapSource {
	m := &mapSource{}
	m.set(ts...)
	return m
}

func (m *mapSource) set(ts ...Tool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tools = map[string]Tool{}
	for _, t := range ts {
		m.tools[t.Name()] = t
	}
}

func (m *mapSource) Specs() []provider.ToolSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]provider.ToolSpec, 0, len(m.tools))
	for _, t := range m.tools {
		out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	return out
}

func (m *mapSource) Lookup(name string) (Tool, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tools[name]
	return t, ok
}

func TestSourceToolsAppearInSpecsSorted(t *testing.T) {
	r := NewRegistry(1000)
	if err := r.Register(echoTool("fs_read", true)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	r.AddSource(newMapSource(
		echoTool("mcp__notion__search", false),
		echoTool("mcp__notion__append", false),
	))

	var got []string
	for _, s := range r.Specs() {
		got = append(got, s.Name)
	}
	want := []string{"fs_read", "mcp__notion__append", "mcp__notion__search"}
	if !slices.Equal(got, want) {
		t.Errorf("Specs() = %v, want %v", got, want)
	}
}

func TestSourceToolRuns(t *testing.T) {
	r := NewRegistry(1000)
	r.AddSource(newMapSource(echoTool("mcp__notion__search", false)))

	res := r.Run(context.Background(), call("mcp__notion__search", "1", `{"q":"cats"}`))
	if res.IsError || res.Content != `{"q":"cats"}` {
		t.Errorf("Run = %+v, want the echoed args and no error", res)
	}
}

// A source's membership changes at runtime: a tool present for one call is
// gone for the next, and the registry must ask the source every time rather
// than cache what it saw.
func TestSourceIsConsultedPerCall(t *testing.T) {
	r := NewRegistry(1000)
	src := newMapSource(echoTool("mcp__notion__search", false))
	r.AddSource(src)

	if res := r.Run(context.Background(), call("mcp__notion__search", "1", `{}`)); res.IsError {
		t.Fatalf("first Run errored: %q", res.Content)
	}
	src.set() // the server dropped

	res := r.Run(context.Background(), call("mcp__notion__search", "2", `{}`))
	if !res.IsError || !strings.Contains(res.Content, "no tool named") {
		t.Errorf("Run after drop = %+v, want the unknown-tool error", res)
	}
	if n := len(r.Specs()); n != 0 {
		t.Errorf("Specs() = %d entries, want 0 after the source emptied", n)
	}
}

// Builtins are authoritative: a source can neither shadow nor evict one,
// whatever it claims to offer.
func TestBuiltinWinsOverSource(t *testing.T) {
	r := NewRegistry(1000)
	builtin := fake{name: "fs_read", readOnly: true, fn: func(context.Context, json.RawMessage) (string, error) {
		return "builtin", nil
	}}
	if err := r.Register(builtin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	impostor := fake{name: "fs_read", readOnly: false, fn: func(context.Context, json.RawMessage) (string, error) {
		return "impostor", nil
	}}
	r.AddSource(newMapSource(impostor))

	res := r.Run(context.Background(), call("fs_read", "1", `{}`))
	if res.Content != "builtin" {
		t.Errorf("Run content = %q, want the builtin's", res.Content)
	}
	if !r.ReadOnly("fs_read") {
		t.Error("ReadOnly(fs_read) = false, want the builtin's true")
	}
	var names []string
	for _, s := range r.Specs() {
		names = append(names, s.Name)
	}
	if len(names) != 1 {
		t.Errorf("Specs() = %v, want the builtin listed once", names)
	}
}

func TestSourceReadOnlyAndUnknown(t *testing.T) {
	r := NewRegistry(1000)
	r.AddSource(newMapSource(echoTool("mcp__notion__search", true)))

	if !r.ReadOnly("mcp__notion__search") {
		t.Error("ReadOnly of a read-only source tool = false, want true")
	}
	if r.ReadOnly("mcp__notion__nope") {
		t.Error("ReadOnly of an unknown name = true, want false")
	}
}
