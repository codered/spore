package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"text/tabwriter"

	"github.com/codered/spore/internal/config"
	mcphost "github.com/codered/spore/internal/mcp"
)

// cmdMCPList dials the configured servers, prints what each one contributed,
// and exits. It answers the only question an operator actually has here:
// why can the model not see this tool?
func cmdMCPList(ctx context.Context, cfg *config.Config) error {
	if len(cfg.MCP.Servers) == 0 {
		fmt.Println("no mcp servers configured; declare one with [[mcp.server]]")
		return nil
	}
	host := mcphost.New(cfg.MCP, cfg.Policy.Workspace, slog.Default())
	defer host.Close()
	host.DialAll(ctx)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVER\tTRANSPORT\tSTATE\tTOOLS\tERROR")
	for _, s := range host.Status() {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", s.Name, s.Transport, s.State, len(s.Tools), s.LastErr)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	for _, s := range host.Status() {
		if len(s.Tools) == 0 && len(s.Skipped) == 0 {
			continue
		}
		fmt.Printf("\n%s:\n", s.Name)
		for _, name := range s.Tools {
			fmt.Printf("  %s\n", name)
		}
		for _, sk := range s.Skipped {
			fmt.Printf("  (skipped) %s — %s\n", sk.Tool, sk.Reason)
		}
	}
	return nil
}
