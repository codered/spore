// Package mcp hosts client connections to MCP servers declared in config and
// exposes their tools to the registry as a tool.Source, namespaced
// mcp__<server>__<tool>.
//
// Declaring a server in the config file is the authorization to run it, so
// there is no sandbox here. What there is, is a child process that gets
// nothing it was not given: its environment is built from scratch, its
// working directory is the policy workspace, and it is killed rather than
// left behind at shutdown.
package mcp

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/codered/spore/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// childEnv builds a stdio server's environment from scratch, with layers in
// precedence order: PATH (always), inherited names (from Inherit), explicit values
// (from Env). Explicit values overwrite inherited ones with the same key.
//
// A map deduplicates the environment before serializing to []string, preventing
// duplicate entries where an operator might override PATH or inherit a name they
// also set explicitly. The returned slice is sorted by key for determinism.
//
// PATH is always passed because without it a command like "npx" cannot resolve,
// and it names no secrets. Everything else in spore's environment —
// ANTHROPIC_API_KEY and every other provider credential — is invisible to the
// child unless the operator names it.
func childEnv(s config.MCPServer) []string {
	env := make(map[string]string)
	// PATH first: give it to the child unless Env overrides it.
	if p, ok := os.LookupEnv("PATH"); ok {
		env["PATH"] = p
	}
	// Inherited values second.
	for _, name := range s.Inherit {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	// Explicit values last: these overwrite anything already in the map,
	// so the operator can override PATH or any inherited name.
	for k, v := range s.Env {
		env[k] = v
	}
	// Serialize to a sorted slice for deterministic output and testability.
	out := make([]string, 0, len(env))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// terminateGrace is how long CommandTransport.Close waits after closing the
// child's stdin before it sends SIGTERM.
const terminateGrace = 3 * time.Second

// transportFor builds the SDK transport for one server. For stdio it also
// returns the command, which the host keeps so it can kill a process that
// outlives the SDK's own termination sequence; for http it returns nil.
func transportFor(s config.MCPServer, workspace string) (sdk.Transport, *exec.Cmd, error) {
	switch s.Transport {
	case "stdio":
		cmd := exec.Command(s.Command, s.Args...)
		cmd.Env = childEnv(s)
		cmd.Dir = workspace
		cmd.Stderr = os.Stderr // a server's diagnostics belong in spore's log
		setPgid(cmd)
		return &sdk.CommandTransport{Command: cmd, TerminateDuration: terminateGrace}, cmd, nil
	case "http":
		return &sdk.StreamableClientTransport{Endpoint: s.URL}, nil, nil
	default:
		return nil, nil, fmt.Errorf("mcp server %q: unknown transport %q", s.Name, s.Transport)
	}
}
