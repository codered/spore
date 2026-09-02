package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildProbe compiles the fixture server and returns the binary's path.
func buildProbe(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "envprobe")
	cmd := exec.Command("go", "build", "-o", bin, "./testdata/envprobe")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building the envprobe fixture: %v", err)
	}
	return bin
}

func TestChildEnvGivesOnlyWhatWasNamed(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-never-be-visible")
	t.Setenv("SPORE_TEST_INHERITED", "inherited-value")

	got := childEnv(config.MCPServer{
		Env:     map[string]string{"NOTION_TOKEN": "explicit-value"},
		Inherit: []string{"SPORE_TEST_INHERITED"},
	})

	if !slices.Contains(got, "NOTION_TOKEN=explicit-value") {
		t.Errorf("childEnv = %v, want the explicit NOTION_TOKEN", got)
	}
	if !slices.Contains(got, "SPORE_TEST_INHERITED=inherited-value") {
		t.Errorf("childEnv = %v, want the inherited value", got)
	}
	for _, kv := range got {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatal("childEnv leaked ANTHROPIC_API_KEY to the child")
		}
	}
	var sawPath bool
	for _, kv := range got {
		sawPath = sawPath || strings.HasPrefix(kv, "PATH=")
	}
	if !sawPath {
		t.Error("childEnv dropped PATH; a command like npx could not resolve")
	}
}

// The real subprocess test: start the fixture over stdio and ask it what it
// actually got. This is the one that proves the allowlist and the pinned
// working directory hold end to end rather than only in childEnv's unit test.
func TestStdioChildIsConfinedInPractice(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-never-be-visible")
	bin := buildProbe(t)
	workspace := t.TempDir()

	srv := config.MCPServer{
		Name: "probe", Transport: "stdio", Command: bin,
		Env: map[string]string{"NOTION_TOKEN": "explicit-value"},
	}
	transport, cmd, err := transportFor(srv, workspace)
	if err != nil {
		t.Fatalf("transportFor: %v", err)
	}
	if cmd == nil {
		t.Fatal("transportFor returned no command for a stdio server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := sdk.NewClient(&sdk.Implementation{Name: "spore", Version: "test"}, nil)
	cs, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		_ = cs.Close()
		killGroup(cmd)
	}()

	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "probe", Arguments: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	text, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is %T, want *sdk.TextContent", res.Content[0])
	}
	var got struct {
		Env []string `json:"env"`
		Cwd string   `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(text.Text), &got); err != nil {
		t.Fatalf("probe report: %v", err)
	}

	for _, kv := range got.Env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") {
			t.Fatal("the child process could read ANTHROPIC_API_KEY")
		}
	}
	if !slices.Contains(got.Env, "NOTION_TOKEN=explicit-value") {
		t.Errorf("child env = %v, want the explicit NOTION_TOKEN", got.Env)
	}
	// macOS reports /var as /private/var; compare resolved paths.
	wantCwd, _ := filepath.EvalSymlinks(workspace)
	gotCwd, _ := filepath.EvalSymlinks(got.Cwd)
	if gotCwd != wantCwd {
		t.Errorf("child cwd = %q, want the workspace %q", gotCwd, wantCwd)
	}
}

func TestTransportForHTTP(t *testing.T) {
	transport, cmd, err := transportFor(config.MCPServer{
		Name: "remote", Transport: "http", URL: "https://example.com/mcp",
	}, "/ws")
	if err != nil {
		t.Fatalf("transportFor: %v", err)
	}
	if cmd != nil {
		t.Error("transportFor returned a command for an http server")
	}
	st, ok := transport.(*sdk.StreamableClientTransport)
	if !ok {
		t.Fatalf("transport is %T, want *sdk.StreamableClientTransport", transport)
	}
	if st.Endpoint != "https://example.com/mcp" {
		t.Errorf("Endpoint = %q, want the configured url", st.Endpoint)
	}
}
