package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/trace/phoenix"
)

// cmdTrace provisions and reports on the trace sidecar. Span creation needs
// none of this: it is configured by [trace] and happens whether or not spore
// started the collector.
func cmdTrace(ctx context.Context, cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spore trace setup | status | teardown")
	}
	switch args[0] {
	case "setup":
		return traceSetupCmd(ctx, cfg, args[1:])
	case "status":
		return traceStatusCmd(ctx, cfg)
	case "teardown":
		return traceTeardownCmd(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("unknown trace command %q: want setup, status or teardown", args[0])
	}
}

// phoenixDir is where the compose file lives: next to the database rather
// than in the workspace, because it is spore's state and not the operator's
// project.
func phoenixDir(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, "phoenix")
}

func traceSetupCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("trace setup", flag.ContinueOnError)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait for the container to become ready")
	if err := fs.Parse(args); err != nil {
		return err
	}

	health, err := phoenix.HealthURL(cfg.Trace.Endpoint)
	if err != nil {
		return err
	}
	if err := phoenix.DockerAvailable(); err != nil {
		return err
	}
	dir := phoenixDir(cfg)
	path, err := phoenix.WriteCompose(dir)
	if err != nil {
		return err
	}
	fmt.Println("wrote", path)
	fmt.Println("starting phoenix (a first run pulls the image)...")
	if err := phoenix.Up(ctx, dir); err != nil {
		return err
	}

	fmt.Print("waiting for it to answer... ")
	if err := phoenix.WaitReady(ctx, health, *timeout); err != nil {
		fmt.Println("no")
		return err
	}
	fmt.Println("ready")

	if err := config.SetTraceEnabled(cfg.Path, true); err != nil {
		return err
	}
	fmt.Printf("trace.enabled is now true in %s\n", cfg.Path)
	if !cfg.Trace.Redact {
		// Say this where the operator is looking, not only in the README.
		// Turning tracing on sends prompt and completion text to a container
		// on this machine, and some of that text arrived from other people.
		fmt.Println("note: redact is off, so prompts and completions are stored in full,")
		fmt.Println("      including messages that arrived over a bridge. Set redact = true")
		fmt.Println("      under [trace] to keep span shapes and token counts without the text.")
	}
	fmt.Println("restart the daemon to pick it up: spore serve --stop && spore serve")
	fmt.Println("the UI is at http://localhost:6006")
	return nil
}

func traceStatusCmd(ctx context.Context, cfg *config.Config) error {
	fmt.Printf("enabled:     %t\n", cfg.Trace.Enabled)
	fmt.Printf("endpoint:    %s\n", cfg.Trace.Endpoint)
	fmt.Printf("sample_rate: %g\n", cfg.Trace.SampleRate)
	fmt.Printf("redact:      %t\n", cfg.Trace.Redact)

	health, err := phoenix.HealthURL(cfg.Trace.Endpoint)
	if err != nil {
		return err
	}
	// A short deadline: status reports, it does not wait.
	probe, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := phoenix.Ready(probe, health); err != nil {
		fmt.Printf("collector:   not answering (%v)\n", err)
		return nil
	}
	fmt.Println("collector:   ready")
	return nil
}

func traceTeardownCmd(ctx context.Context, cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("trace teardown", flag.ContinueOnError)
	purge := fs.Bool("purge", false, "also delete the trace data volume")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Stop first, write config second. The reverse order would leave tracing
	// switched off in the file while the container kept running.
	if err := phoenix.Down(ctx, phoenixDir(cfg), *purge); err != nil {
		return err
	}
	if err := config.SetTraceEnabled(cfg.Path, false); err != nil {
		return err
	}
	fmt.Println("stopped; trace.enabled is back to false")
	if !*purge {
		fmt.Println("the trace volume was kept; pass --purge to delete it")
	}
	return nil
}
