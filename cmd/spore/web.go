package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/mattn/go-isatty"
)

// cmdWeb opens the web UI signed in. The URL carries the daemon token once;
// the daemon swaps it for a cookie and redirects it out of the address bar.
func cmdWeb(ctx context.Context, cfg *config.Config) error {
	if _, err := ensureDaemon(ctx, cfg); err != nil {
		return err
	}
	tty := isatty.IsTerminal(os.Stdout.Fd())
	return cmdWebURL(cfg, os.Stdout, tty, openBrowser)
}

// cmdWebURL prints the URL only to a terminal: a model running spore web
// through shell_exec has no terminal, and must not be handed the token.
func cmdWebURL(cfg *config.Config, out io.Writer, tty bool, open func(string) error) error {
	tok, err := daemon.ReadToken(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("read the daemon token: %w", err)
	}
	u := fmt.Sprintf("http://%s/?token=%s", cfg.Daemon.Addr, tok)
	openErr := open(u)
	if tty {
		_, _ = fmt.Fprintf(out, "opening %s\n", u)
	} else {
		_, _ = fmt.Fprintln(out, "opened the spore web UI in your browser")
	}
	if openErr != nil && tty {
		_, _ = fmt.Fprintf(out, "could not open a browser (%v); open the URL above by hand\n", openErr)
	}
	return nil
}

func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u) //nolint:gosec // G204: u is the local daemon URL built above
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u) //nolint:gosec // G204: u is the local daemon URL built above
	default:
		cmd = exec.Command("xdg-open", u) //nolint:gosec // G204: u is the local daemon URL built above
	}
	return cmd.Start()
}
