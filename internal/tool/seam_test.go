package tool_test

import (
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/tool"
)

// The registry must satisfy agent.ToolRunner without internal/tool importing
// internal/agent — the assertion lives in an external test package so the
// dependency stays one-directional.
func TestRegistrySatisfiesToolRunner(t *testing.T) {
	var _ agent.ToolRunner = tool.NewRegistry(100)
}
