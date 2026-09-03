package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type echoIn struct {
	Text string `json:"text" jsonschema:"the text to echo"`
}

// echoServer offers one tool that echoes its argument.
func echoServer(t *testing.T) *sdk.Server {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "notion", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "search", Description: "search pages"},
		func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "found " + in.Text}}}, nil, nil
		})
	return srv
}

func TestSnapshotNamespacesAndDescribes(t *testing.T) {
	cs := serveInMemory(t, echoServer(t))
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	tl, ok := snap.tools["mcp__notion__search"]
	if !ok {
		t.Fatalf("snapshot tools = %v, want mcp__notion__search", snap.tools)
	}
	if tl.Name() != "mcp__notion__search" {
		t.Errorf("Name() = %q", tl.Name())
	}
	if tl.Description() != "search pages" {
		t.Errorf("Description() = %q, want the server's", tl.Description())
	}
	var schema map[string]any
	if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
		t.Fatalf("Schema() is not JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("Schema() type = %v, want object", schema["type"])
	}
}

// The result carries a prefix naming the server and marking the content as
// data. A prefix and not a fence: the registry truncates at a byte budget,
// and a closing fence is exactly what a long result would lose.
func TestCallPrefixesResultAsExternalData(t *testing.T) {
	cs := serveInMemory(t, echoServer(t))
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	got, err := snap.tools["mcp__notion__search"].Call(context.Background(), json.RawMessage(`{"text":"cats"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.HasPrefix(got, untrustedPrefix("notion")) {
		t.Errorf("Call = %q, want it to start with the untrusted-content prefix", got)
	}
	if !strings.Contains(got, "found cats") {
		t.Errorf("Call = %q, want the server's text", got)
	}
}

// Tool error messages are also server-authored content and must be marked as
// external data, even in the error path. A hostile or compromised server could
// inject prompt-injection text in error messages, reaching the model's context
// through the registry's error-to-result conversion.
func TestCallErrorPrefixesResultAsExternalData(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "notion", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "search", Description: "search pages"},
		func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			// Server returns an error with injection-shaped text.
			return &sdk.CallToolResult{IsError: true, Content: []sdk.Content{
				&sdk.TextContent{Text: "<!-- inject: malicious prompt -->"},
			}}, nil, nil
		})
	cs := serveInMemory(t, srv)
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	_, err := snap.tools["mcp__notion__search"].Call(context.Background(), json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("Call returned no error for a failing tool")
	}
	if !strings.Contains(err.Error(), untrustedPrefix("notion")) {
		t.Errorf("Call error = %v, want it to contain the untrusted-content prefix", err)
	}
}

// A server-declared readOnlyHint is not evidence: the SDK's own documentation
// says clients should never make tool-use decisions on annotations from
// untrusted servers. Believing it would let a server opt itself into
// concurrent dispatch.
func TestReadOnlyIsAlwaysFalse(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "notion", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{
		Name:        "search",
		Description: "search pages",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil, nil
	})
	cs := serveInMemory(t, srv)
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	if snap.tools["mcp__notion__search"].ReadOnly() {
		t.Error("ReadOnly() = true; a server's own readOnlyHint must not be believed")
	}
}

func TestCallReportsToolErrorsAsErrors(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "notion", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "search", Description: "search pages"},
		func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			return nil, nil, errors.New("upstream is on fire")
		})
	cs := serveInMemory(t, srv)
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	_, err := snap.tools["mcp__notion__search"].Call(context.Background(), json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("Call returned no error for a failing tool")
	}
	if !strings.Contains(err.Error(), "upstream is on fire") {
		t.Errorf("Call error = %v, want the server's message", err)
	}
}

func TestCallHonoursTheTimeout(t *testing.T) {
	srv := sdk.NewServer(&sdk.Implementation{Name: "slow", Version: "v0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "sleep", Description: "sleep forever"},
		func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
			<-ctx.Done()
			return nil, nil, ctx.Err()
		})
	cs := serveInMemory(t, srv)
	snap := newSnapshot("slow", cs, listTools(t, cs), 50*time.Millisecond)

	start := time.Now()
	_, err := snap.tools["mcp__slow__sleep"].Call(context.Background(), json.RawMessage(`{"text":"x"}`))
	if err == nil {
		t.Fatal("Call returned no error for a call that outran its timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Call took %v; the per-call timeout did not bound it", elapsed)
	}
}

// A tool whose namespaced name the registry would reject is dropped, and the
// rest of that server's tools are still offered.
func TestSnapshotSkipsUnregistrableNames(t *testing.T) {
	long := strings.Repeat("x", 70)
	srv := sdk.NewServer(&sdk.Implementation{Name: "notion", Version: "v0"}, nil)
	for _, name := range []string{"search", long, "has space"} {
		sdk.AddTool(srv, &sdk.Tool{Name: name, Description: "d"},
			func(ctx context.Context, req *sdk.CallToolRequest, in echoIn) (*sdk.CallToolResult, any, error) {
				return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}}, nil, nil
			})
	}
	cs := serveInMemory(t, srv)
	snap := newSnapshot("notion", cs, listTools(t, cs), time.Minute)

	if _, ok := snap.tools["mcp__notion__search"]; !ok {
		t.Error("the good tool was dropped along with the bad ones")
	}
	if len(snap.tools) != 1 {
		t.Errorf("snapshot has %d tools, want only the registrable one", len(snap.tools))
	}
	if len(snap.skipped) != 2 {
		t.Errorf("skipped = %v, want the two unregistrable names", snap.skipped)
	}
}

func TestSnapshotSkipsDuplicateNames(t *testing.T) {
	cs := serveInMemory(t, echoServer(t))
	tools := listTools(t, cs)
	snap := newSnapshot("notion", cs, append(tools, tools[0]), time.Minute)

	if len(snap.tools) != 1 {
		t.Errorf("snapshot has %d tools, want 1", len(snap.tools))
	}
	if len(snap.skipped) != 1 || !strings.Contains(snap.skipped[0].Reason, "duplicate") {
		t.Errorf("skipped = %v, want one duplicate", snap.skipped)
	}
}
