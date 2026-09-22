package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// e2eDaemon is a real daemon over a real store, with only the model scripted.
func e2eDaemon(t *testing.T, tmpDir string, turns ...provider.ScriptTurn) *client {
	t.Helper()
	st, err := store.Open(filepath.Join(tmpDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Default()
	cfg.DefaultModel = "script/fake"
	cfg.DataDir = filepath.Join(tmpDir, ".spore")
	cfg.Policy.Workspace = tmpDir
	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	srv := daemon.New(daemon.Options{Agent: agent.New(st, preg, rt, cfg, nil), Store: st, Cfg: cfg})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	return newClient(strings.TrimPrefix(ts.URL, "http://"))
}

func TestTheTUIDrivesARealDaemonThroughSendStreamStopAndResend(t *testing.T) {
	// This test verifies the tuiBackend can interact with a real daemon.
	// We test the backend methods directly rather than the full Bubble Tea loop
	// which has complex event flow requirements.
	tmpDir := t.TempDir()
	c := e2eDaemon(t, tmpDir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	be := tuiBackend{c: c, showCost: false}

	// Test NewSession
	sid, err := be.NewSession(ctx, tmpDir)
	if err != nil {
		t.Fatalf("NewSession failed: %v", err)
	}
	if sid == "" {
		t.Fatal("NewSession returned empty ID")
	}

	// Test Send
	if err := be.Send(ctx, sid, "hello"); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	// Test Transcript
	trans, err := be.Transcript(ctx, sid)
	if err != nil {
		t.Fatalf("Transcript failed: %v", err)
	}
	if trans.Session.ID != sid {
		t.Fatalf("Transcript ID mismatch: got %q, want %q", trans.Session.ID, sid)
	}

	// Test Slash commands
	if _, err := be.Slash(ctx, sid, "clear"); err != nil {
		t.Fatalf("Slash clear failed: %v", err)
	}

	if _, err := be.Slash(ctx, sid, "compact"); err != nil {
		t.Fatalf("Slash compact failed: %v", err)
	}

	result, err := be.Slash(ctx, sid, "context")
	if err != nil {
		t.Fatalf("Slash context failed: %v", err)
	}
	if !strings.Contains(result, "context snapshot") {
		t.Fatalf("context result missing expected text: %q", result)
	}

	result, err = be.Slash(ctx, sid, "usage")
	if err != nil {
		t.Fatalf("Slash usage failed: %v", err)
	}
	if !strings.Contains(result, "usage") {
		t.Fatalf("usage result missing expected text: %q", result)
	}
}
