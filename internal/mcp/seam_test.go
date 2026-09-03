package mcp_test

import (
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/tool"
)

func TestHostSatisfiesToolSource(t *testing.T) {
	var _ tool.Source = mcp.New(config.MCPConfig{}, "/ws", nil)
}
