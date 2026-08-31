package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

func TestEnsureDaemonUsesAnAlreadyRunningOne(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			hits++
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Daemon.Addr = strings.TrimPrefix(ts.URL, "http://")

	c, err := ensureDaemon(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ensureDaemon: %v", err)
	}
	if c == nil {
		t.Fatal("ensureDaemon returned no client")
	}
	if hits == 0 {
		t.Error("ensureDaemon never probed /healthz")
	}
	// Nothing was spawned, so no pidfile should have appeared.
	if _, err := daemonPid(cfg); err == nil {
		t.Error("ensureDaemon wrote a pidfile for a daemon it did not start")
	}
}

func TestWaitForHealthGivesUp(t *testing.T) {
	// Port 1 on loopback refuses instantly, so this exercises the give-up
	// path without waiting on a real timeout.
	c := newClient("127.0.0.1:1")
	start := time.Now()
	err := waitForHealth(context.Background(), c, 300*time.Millisecond)
	if err == nil {
		t.Fatal("waitForHealth reported a healthy daemon on a closed port")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waitForHealth took %v; it should give up after its timeout", elapsed)
	}
}

func TestWaitForHealthReturnsAsSoonAsItIsUp(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	c := newClient(strings.TrimPrefix(ts.URL, "http://"))
	if err := waitForHealth(context.Background(), c, 5*time.Second); err != nil {
		t.Fatalf("waitForHealth: %v", err)
	}
}
