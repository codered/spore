package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/codered/spore/internal/config"
	sporetrace "github.com/codered/spore/internal/trace"
)

// tracingFixture writes a config with tracing switched on and pointed at a
// stand-in collector, and returns the config path and the collector's hit
// counter. The collector answers 200 to anything: this asserts that spans
// arrive, not what Phoenix makes of them.
func tracingFixture(t *testing.T) (configPath string, hits *atomic.Int64) {
	t.Helper()
	hits = &atomic.Int64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			hits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.toml")
	body := "default_model = \"p/m\"\ndata_dir = \"" + dir + "\"\n\n" +
		"[providers.p]\nkind = \"anthropic\"\napi_key = \"x\"\n\n" +
		"[trace]\nenabled = true\nendpoint = \"" + srv.URL + "/v1/traces\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, hits
}

// Tracing has to still be live while the command runs. The provider used to
// be shut down in a goroutine immediately after Init, which left every later
// span non-recording and Phoenix empty however the config read.
func TestRunKeepsTracingLiveForTheCommand(t *testing.T) {
	path, hits := tracingFixture(t)

	orig := dispatchFn
	t.Cleanup(func() { dispatchFn = orig })
	dispatchFn = func(ctx context.Context, cfg *config.Config, args []string) error {
		_, span := sporetrace.StartTurn(ctx, "session", "test")
		span.End()
		return nil
	}

	if err := run([]string{"-config", path, "session", "list"}); err != nil {
		t.Fatal(err)
	}
	// run returns only after the deferred shutdown has flushed the batch, so
	// by here a span opened inside the command must have reached the
	// collector.
	if hits.Load() == 0 {
		t.Error("no spans reached the collector: tracing was shut down before the command ran")
	}
}
