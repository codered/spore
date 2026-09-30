package discord

import (
	"reflect"
	"testing"

	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
)

// The operator's revoke, reconnect and delete-fact controls must never be
// reachable from Discord. The bridge reaches the daemon only through
// Options, so none of the collaborators those actions need may be in it.
func TestOptionsHoldNoOperatorControls(t *testing.T) {
	forbidden := map[reflect.Type]bool{
		reflect.TypeOf((*policy.Reloader)(nil)): true,
		reflect.TypeOf((*mcp.Host)(nil)):        true,
		reflect.TypeOf((*memory.Cache)(nil)):    true,
	}
	opts := reflect.TypeOf(Options{})
	for i := 0; i < opts.NumField(); i++ {
		f := opts.Field(i)
		if forbidden[f.Type] {
			t.Errorf("discord.Options.%s is a %s; operator controls must not reach the bridge", f.Name, f.Type)
		}
	}
}
