package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// cmdCompanion is the operator's view of the companion. Everything but
// "answer" reads and writes the database directly, like spore recall, so it
// works with or without a running daemon; "answer" goes through the daemon
// so the answer is posted where the proposal was. The text comes from
// companion.Run, which the daemon's /companion also uses.
func cmdCompanion(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore companion status | interests | outreach | pause [duration] | resume | answer <id> yes|not-now|never | interests undecline <key> | check [--promote <key> [--force]] [--research <key>]")
	}
	switch args[0] {
	case "answer":
		return companionAnswer(ctx, cfg, args[1:])
	case "check":
		return companionCheck(ctx, cfg, args[1:])
	}
	st, err := store.Open(cfg.DBPath())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	return companion.Run(ctx, os.Stdout, companion.NewService(st, cfg), args)
}

// companionAnswer presses a proposal's button from the terminal.
func companionAnswer(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: spore companion answer <outreach id> yes|not-now|never")
	}
	if _, err := strconv.ParseInt(args[0], 10, 64); err != nil {
		return fmt.Errorf("outreach id %q must be a number", args[0])
	}
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	var out struct {
		Message string `json:"message"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/companion/proposals/"+args[0], map[string]string{"choice": args[1]}, &out); err != nil {
		return err
	}
	fmt.Println(out.Message)
	return nil
}

// companionCheck runs one heartbeat in the daemon now, skipping pause, quiet
// hours and the unchanged-state skip, and prints what it did. --promote
// makes an observing interest a candidate first; --force also tells the
// model it must propose it. --research tells the model to start a research
// run for an active interest; it sends nothing. What it sends is real: Yes
// starts a watch.
func companionCheck(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("companion check", flag.ContinueOnError)
	promote := fs.String("promote", "", "make this observing interest a candidate first")
	force := fs.Bool("force", false, "tell the model it must propose the --promote interest")
	research := fs.String("research", "", "tell the model to start a research run for this active interest")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: spore companion check [--promote <key> [--force]] [--research <key>]")
	}
	if *force && *promote == "" {
		return fmt.Errorf("--force needs --promote <key>")
	}
	if *research != "" && (*promote != "" || *force) {
		return fmt.Errorf("--research cannot be combined with --promote or --force")
	}
	c, err := ensureDaemon(ctx, cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var out struct {
		Result string `json:"result"`
	}
	if err := c.doSlow(ctx, http.MethodPost, "/api/companion/check", map[string]any{"promote": *promote, "force": *force, "research": *research}, &out); err != nil {
		return err
	}
	fmt.Println(out.Result)
	return nil
}
