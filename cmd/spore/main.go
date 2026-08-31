// Command spore is the CLI front end to the agent core.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
	sporetrace "github.com/codered/spore/internal/trace"
)

const usage = `spore — a personal agent

usage:
  spore once <prompt>          run one turn in a fresh session and print the reply
  spore chat [session-id]      interactive session (resumes when given an id)
  spore serve                  run the daemon (HTTP API + web UI + scheduler)
  spore serve --status         report whether a daemon is running
  spore serve --stop           stop a running daemon
  spore session list           list recent sessions
  spore session show <id>      print a session transcript
  spore policy check <tool> [json-args]
                               print the decision a tool call would get

flags:
  -config <path>   config file (default ~/.spore/config.toml)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "spore:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	configPath := ""
	if len(args) >= 2 && args[0] == "-config" {
		configPath, args = args[1], args[2:]
	}
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	if configPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		configPath = filepath.Join(home, ".spore", "config.toml")
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	shutdown, err := sporetrace.Init(ctx, cfg.Trace)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer shutdown(ctx)

	switch args[0] {
	case "once":
		if len(args) < 2 {
			return fmt.Errorf("once needs a prompt")
		}
		return cmdOnce(ctx, cfg, st, args[1])
	case "chat":
		id := ""
		if len(args) > 1 {
			id = args[1]
		}
		return cmdChat(ctx, cfg, st, id)
	case "serve":
		return cmdServe(ctx, cfg, st, args[1:])
	case "session":
		return cmdSession(ctx, st, args[1:])
	case "policy":
		// spore policy check <tool> [json-args] [-profile local|remote]
		if len(args) < 3 || args[1] != "check" {
			return fmt.Errorf("usage: spore policy check <tool> [json-args] [-profile local|remote]")
		}
		profile, jsonArgs := "local", "{}"
		rest := args[3:]
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-profile" && i+1 < len(rest) {
				profile = rest[i+1]
				i++
				continue
			}
			jsonArgs = rest[i]
		}
		return cmdPolicyCheck(cfg, profile, args[2], jsonArgs)
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}
