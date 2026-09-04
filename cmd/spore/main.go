// Command spore is the CLI front end to the agent core.
package main

import (
	"context"
	"fmt"
	"log/slog"
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
  spore mcp list               dial the configured MCP servers and print their tools
  spore recall search <query>  keyword-search the message/summary/fact index
  spore recall status          report index counts and backend health
  spore recall reindex         rebuild the index from SQLite and fact files
  spore recall setup           provision the vector store and backfill it
  spore recall teardown        stop the vector store and return to keyword search
  spore trace setup            provision the phoenix collector and turn tracing on
  spore trace status           report trace configuration and collector health
  spore trace teardown         stop the collector and turn tracing off

flags:
  -config <path>       config file (default ~/.spore/config.toml)
  --workspace <dir>    root a new session here instead of the current directory;
                       on a resume, re-root the session (once, chat)
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

	ctx := context.Background()
	shutdown, err := sporetrace.Init(ctx, cfg.Trace)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	defer shutdown(ctx)

	switch args[0] {
	case "once":
		rest, ws, err := takeWorkspaceFlag(args[1:])
		if err != nil {
			return err
		}
		if len(rest) == 0 {
			return fmt.Errorf("once needs a prompt")
		}
		return cmdOnce(ctx, cfg, rest[0], ws)
	case "chat":
		rest, ws, err := takeWorkspaceFlag(args[1:])
		if err != nil {
			return err
		}
		id := ""
		if len(rest) > 0 {
			id = rest[0]
		}
		return cmdChat(ctx, cfg, id, ws)
	case "serve":
		st, err := openStore(ctx, cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		return cmdServe(ctx, cfg, st, args[1:])
	case "session":
		st, err := openStore(ctx, cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		return cmdSession(ctx, st, args[1:])
	case "policy":
		// spore policy check <tool> [json-args] [-profile local|remote] [-workspace <dir>]
		if len(args) < 3 || args[1] != "check" {
			return fmt.Errorf("usage: spore policy check <tool> [json-args] [-profile local|remote] [-workspace <dir>]")
		}
		profile, workspace, jsonArgs := "local", "", "{}"
		rest := args[3:]
		for i := 0; i < len(rest); i++ {
			if rest[i] == "-profile" && i+1 < len(rest) {
				profile = rest[i+1]
				i++
				continue
			}
			if rest[i] == "-workspace" && i+1 < len(rest) {
				workspace = rest[i+1]
				i++
				continue
			}
			jsonArgs = rest[i]
		}
		return cmdPolicyCheck(cfg, profile, workspace, args[2], jsonArgs)
	case "mcp":
		if len(args) < 2 || args[1] != "list" {
			return fmt.Errorf("usage: spore mcp list")
		}
		return cmdMCPList(ctx, cfg)
	case "recall":
		return cmdRecall(ctx, cfg, args[1:])
	case "trace":
		return cmdTrace(ctx, cfg, args[1:])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// sessionWorkspace is the directory a CLI-created session is rooted at: the
// --workspace flag when given, otherwise the directory spore was run in, so
// spore describes and operates on the directory you are standing in.
func sessionWorkspace(flag string) (string, error) {
	if flag == "" {
		return os.Getwd()
	}
	return filepath.Abs(flag)
}

// takeWorkspaceFlag pulls "--workspace <dir>" (or "-workspace <dir>") out of
// an argument list, wherever it appears, and returns the rest. spore's CLI
// parses by hand rather than with the flag package, because a prompt is a
// positional argument that may itself begin with a dash. If the flag
// appears more than once, the last occurrence wins.
func takeWorkspaceFlag(args []string) (rest []string, workspace string, err error) {
	for i := 0; i < len(args); i++ {
		if args[i] != "--workspace" && args[i] != "-workspace" {
			rest = append(rest, args[i])
			continue
		}
		if i+1 >= len(args) {
			return nil, "", fmt.Errorf("--workspace needs a directory")
		}
		workspace = args[i+1]
		i++
	}
	return rest, workspace, nil
}

// openStore opens the database and backfills any session written before the
// workspace column existed, so resuming one behaves exactly as it did when
// the workspace was a single daemon-wide value.
func openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return nil, err
	}
	if n, err := st.BackfillSessionWorkspaces(ctx, cfg.Policy.Workspace); err != nil {
		// Not fatal: a session whose root is empty still lists and still
		// shows, and the daemon refuses only to run a turn for it.
		slog.Default().Warn("backfilling session workspaces failed", "error", err)
	} else if n > 0 {
		slog.Default().Info("backfilled sessions with no recorded workspace", "count", n, "workspace", cfg.Policy.Workspace)
	}
	return st, nil
}
