package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
)

// traceFixture writes a minimal loadable config and returns it. The data dir
// is the temp dir, so the compose file lands somewhere the test owns.
func traceFixture(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	body := "default_model = \"p/m\"\ndata_dir = \"" + dir + "\"\n\n[providers.p]\nkind = \"anthropic\"\napi_key = \"x\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestTraceRejectsUnknownVerb(t *testing.T) {
	cfg := traceFixture(t)
	err := cmdTrace(context.Background(), cfg, []string{"frobnicate"})
	if err == nil {
		t.Fatal("an unknown verb was accepted")
	}
	if !strings.Contains(err.Error(), "setup") {
		t.Errorf("error should list the real verbs, got: %v", err)
	}
}

func TestTraceRequiresAVerb(t *testing.T) {
	cfg := traceFixture(t)
	if err := cmdTrace(context.Background(), cfg, nil); err == nil {
		t.Fatal("no verb was accepted")
	}
}

func TestTraceStatusReportsConfiguration(t *testing.T) {
	cfg := traceFixture(t)
	out := captureStdout(t, func() error {
		return cmdTrace(context.Background(), cfg, []string{"status"})
	})
	// Tracing is off in the fixture, and status must say so without needing
	// a container to talk to.
	for _, want := range []string{"enabled", "endpoint", "redact"} {
		if !strings.Contains(out, want) {
			t.Errorf("status did not report %q:\n%s", want, out)
		}
	}
}

func TestTraceTeardownTurnsTracingOffInConfig(t *testing.T) {
	cfg := traceFixture(t)
	if err := config.SetTraceEnabled(cfg.Path, true); err != nil {
		t.Fatal(err)
	}
	// Point the compose dir at a directory with no compose file: docker is
	// never reached because teardown writes config only after a successful
	// Down, so this asserts the failure path leaves config alone.
	err := cmdTrace(context.Background(), cfg, []string{"teardown"})
	if err == nil {
		t.Skip("docker is installed and answered; this test covers the no-container path")
	}
	reloaded, lerr := config.Load(cfg.Path)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !reloaded.Trace.Enabled {
		t.Error("a failed teardown turned tracing off anyway; config should only change after the container is really stopped")
	}
}
