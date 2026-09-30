package daemon

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/policy"
)

func TestMCPServersJSONExplainsEachTool(t *testing.T) {
	e, err := policy.NewEngine(config.PolicyConfig{
		Default: "ask", ApprovalTimeout: "5m", Workspace: "/ws",
		Allow: []string{"mcp__gh__*"},
		Ask:   []string{"mcp__fs__*(any path outside workspace)"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := mcpServersJSON([]mcp.ServerStatus{
		{Name: "gh", Transport: "stdio", State: "up", Tools: []string{"mcp__gh__search"}},
		{Name: "fs", Transport: "http", State: "down", LastErr: "refused", Tools: []string{"mcp__fs__read"}},
	}, e)
	if len(got) != 2 || got[0].Tools[0].Decision != "allow" || got[0].Tools[0].DependsOnArgs {
		t.Fatalf("gh = %+v", got)
	}
	if got[1].LastError != "refused" || got[1].Tools[0].Decision != "ask" || !got[1].Tools[0].DependsOnArgs {
		t.Fatalf("fs = %+v", got[1])
	}
	if none := mcpServersJSON([]mcp.ServerStatus{{Name: "x", Tools: []string{"mcp__x__y"}}}, nil); none[0].Tools[0].Decision != "" {
		t.Error("with no engine the decision is left empty")
	}
}

func TestMCPRoutes(t *testing.T) {
	s, ts := newTestServer(t)
	code, body := send(t, "GET", ts.URL+"/api/mcp", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "no MCP servers configured") {
		t.Fatalf("unwired GET = %d %s", code, body)
	}

	h := mcp.New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "gh", Transport: "stdio", Command: filepath.Join(t.TempDir(), "missing")},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
	t.Cleanup(h.Close)
	s.AttachOperator(Operator{MCP: h})
	logs := captureLog(t)

	code, body = send(t, "GET", ts.URL+"/api/mcp", nil)
	var servers []MCPServerJSON
	_ = json.Unmarshal([]byte(body), &servers)
	if code != 200 || len(servers) != 1 || servers[0].Name != "gh" || servers[0].State != "down" {
		t.Fatalf("GET = %d %s", code, body)
	}
	if code, _ = send(t, "POST", ts.URL+"/api/mcp/gh/reconnect", nil); code != http.StatusAccepted {
		t.Errorf("reconnect = %d, want 202", code)
	}
	if !strings.Contains(logs.String(), "action=reconnect") {
		t.Errorf("no audit line:\n%s", logs.String())
	}
	if code, body = send(t, "POST", ts.URL+"/api/mcp/nope/reconnect", nil); code != 404 {
		t.Errorf("unknown server = %d %s", code, body)
	}
}
