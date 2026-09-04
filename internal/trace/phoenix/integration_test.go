//go:build phoenix

package phoenix

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	sporetrace "github.com/codered/spore/internal/trace"
)

// This file asserts against a real Phoenix what a stub cannot tell you: that
// the exporter spore builds is one Phoenix accepts, on the endpoint the
// default config names.
//
// Run it with: make test-phoenix (after `spore trace setup`).
func liveEndpoint(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	endpoint := os.Getenv("SPORE_TRACE_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://127.0.0.1:6006/v1/traces"
	}
	health, err := HealthURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ready(ctx, health); err != nil {
		t.Skipf("phoenix is not running at %s: %v (run `spore trace setup`)", endpoint, err)
	}
	return endpoint
}

func TestReadyAgainstARealServer(t *testing.T) {
	endpoint := liveEndpoint(t)
	health, err := HealthURL(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := Ready(context.Background(), health); err != nil {
		t.Errorf("a running phoenix reported not ready: %v", err)
	}
}

func TestExportReachesARealServer(t *testing.T) {
	endpoint := liveEndpoint(t)
	ctx := context.Background()

	shutdown, err := sporetrace.Init(ctx, config.TraceConfig{
		Enabled:    true,
		Endpoint:   endpoint,
		SampleRate: 1.0,
	})
	if err != nil {
		t.Fatalf("building the exporter phoenix is meant to accept: %v", err)
	}

	_, turn := sporetrace.StartTurn(ctx, "integration-session", "test")
	turn.End()

	// Shutdown flushes the batcher. An export phoenix rejects surfaces here
	// and nowhere else, which is the whole reason this test exists.
	flush, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := shutdown(flush); err != nil {
		t.Errorf("phoenix rejected the export: %v", err)
	}
}
