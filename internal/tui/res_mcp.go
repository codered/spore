package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/daemon"
)

// mcpRes lists the configured MCP servers.
type mcpRes struct{}

func (mcpRes) Name() string   { return "mcp" }
func (mcpRes) Hotkey() string { return "C" }
func (mcpRes) Scoped() bool   { return false }
func (mcpRes) Columns() []Column {
	return []Column{
		{Title: "SERVER", Min: 10},
		{Title: "TRANSPORT", Min: 9},
		{Title: "STATE", Min: 5},
		{Title: "TOOLS", Min: 5, Right: true},
		{Title: "LAST ERROR", Min: 10, Flex: true},
	}
}

func (mcpRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	servers, err := v.MCP(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(servers))
	for _, s := range servers {
		rows = append(rows, Row{
			ID:    s.Name,
			Cells: []string{s.Name, s.Transport, s.State, strconv.Itoa(len(s.Tools)), s.LastError},
			Data:  s,
		})
	}
	return rows, nil
}

func (mcpRes) Detail(r Row) string {
	s, ok := r.Data.(daemon.MCPServerJSON)
	if !ok {
		return ""
	}
	return fmt.Sprintf("server %s (%s)\nstate: %s\ntools: %d\nlast error: %s", s.Name, s.Transport, s.State, len(s.Tools), s.LastError)
}

// Reconnect asks nothing: it requests what the supervisor already does
// after any drop, and repeating it is harmless.
func (mcpRes) Actions() []Action {
	return []Action{{
		Key:   "r",
		Label: "reconnect",
		Run: func(ctx context.Context, v Views, r Row) error {
			return v.Reconnect(ctx, r.ID)
		},
	}}
}

// Drill opens the server's tools and the decision each gets.
func (mcpRes) Drill(r Row) (Resource, bool) { return mcpToolsRes{server: r.ID}, true }

// mcpToolsRes lists one server's tools with the local profile's decision
// for a call with no arguments.
type mcpToolsRes struct{ server string }

func (t mcpToolsRes) Name() string { return t.server + " tools" }
func (mcpToolsRes) Hotkey() string { return "" }
func (mcpToolsRes) Scoped() bool   { return false }
func (mcpToolsRes) Columns() []Column {
	return []Column{
		{Title: "TOOL", Min: 12},
		{Title: "DECISION", Min: 8},
		{Title: "RULE", Min: 10, Flex: true},
	}
}

func (t mcpToolsRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	servers, err := v.MCP(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Name != t.server {
			continue
		}
		prefix := "mcp__" + s.Name + "__"
		rows := make([]Row, 0, len(s.Tools)+len(s.Skipped))
		for _, tool := range s.Tools {
			rule := tool.Rule
			if tool.DependsOnArgs {
				rule += " (depends on args)"
			}
			short := strings.TrimPrefix(tool.Name, prefix)
			rows = append(rows, Row{ID: s.Name + "/" + short, Cells: []string{short, tool.Decision, rule}, Data: tool})
		}
		for _, sk := range s.Skipped {
			rows = append(rows, Row{ID: s.Name + "/skipped/" + sk.Tool, Cells: []string{sk.Tool, "skipped", sk.Reason}, Data: sk})
		}
		return rows, nil
	}
	return nil, fmt.Errorf("no MCP server named %q", t.server)
}

func (mcpToolsRes) Detail(r Row) string { return strings.Join(r.Cells, "\n") }
func (mcpToolsRes) Actions() []Action   { return nil }
