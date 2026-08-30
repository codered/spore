package daemon_test

import (
	"go/build"
	"strings"
	"testing"
)

// Spec invariant 1: the core never imports a transport. internal/daemon
// depends on internal/agent, so the check has to run from outside the core —
// this asserts the dependency stays one-directional.
func TestAgentCoreImportsNoTransport(t *testing.T) {
	banned := map[string]string{
		"net/http":                             "the core must not know about HTTP",
		"github.com/codered/spore/internal/daemon":    "the core must not import its own transport",
		"github.com/codered/spore/internal/scheduler": "the core must not import the scheduler",
	}
	for _, pkg := range []string{
		"github.com/codered/spore/internal/agent",
		"github.com/codered/spore/internal/provider",
		"github.com/codered/spore/internal/router",
	} {
		p, err := build.Import(pkg, "", 0)
		if err != nil {
			t.Fatalf("import %s: %v", pkg, err)
		}
		for _, imp := range p.Imports {
			if why, bad := banned[imp]; bad {
				t.Errorf("%s imports %s: %s", pkg, imp, why)
			}
			if strings.HasPrefix(imp, "github.com/codered/spore/internal/tool") {
				t.Errorf("%s imports %s: the core reaches tools through the ToolRunner seam", pkg, imp)
			}
		}
	}
}
