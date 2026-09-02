package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

// A server that dies is redialled and its tools come back. This is the whole
// point of a live registry: a flapping server heals without restarting spore.
func TestSupervisorRedialsADeadServer(t *testing.T) {
	bin := buildProbe(t)
	h := New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "probe", Transport: "stdio", Command: bin},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
	h.backoffMin, h.backoffMax = 10*time.Millisecond, 50*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wait := Supervise(ctx, h)

	waitFor(t, 20*time.Second, func() bool {
		_, ok := h.Lookup("mcp__probe__probe")
		return ok
	}, "the server never came up")

	tl, _ := h.Lookup("mcp__probe__die")
	if tl == nil {
		t.Fatal("the fixture has no die tool")
	}
	_, _ = tl.Call(ctx, json.RawMessage(`{}`))

	waitFor(t, 20*time.Second, func() bool {
		_, ok := h.Lookup("mcp__probe__probe")
		return !ok
	}, "the host never noticed the server had died")

	waitFor(t, 30*time.Second, func() bool {
		_, ok := h.Lookup("mcp__probe__probe")
		return ok
	}, "the supervisor never redialled the server")

	cancel()
	done := make(chan struct{})
	go func() { wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Supervise did not stop when its context was cancelled")
	}
}

// A server that cannot start is retried rather than abandoned, and the host
// stays usable while it fails.
func TestSupervisorSurvivesAServerThatNeverStarts(t *testing.T) {
	h := New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "broken", Transport: "stdio", Command: "/nonexistent/definitely-not-here"},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
	h.backoffMin, h.backoffMax = time.Millisecond, 5*time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wait := Supervise(ctx, h)

	waitFor(t, 5*time.Second, func() bool {
		for _, s := range h.Status() {
			if s.Name == "broken" && s.LastErr != "" {
				return true
			}
		}
		return false
	}, "the failure was never recorded")

	cancel()
	wait()
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		// A local process redial (spawn + MCP handshake + one tools/list
		// call) can complete in single-digit milliseconds on this hardware,
		// so the "down" state between markDown and the redial's swap can be
		// narrower than a 20ms tick: a coarser poll would reliably step over
		// it and time out even though the supervisor behaved correctly.
		time.Sleep(time.Millisecond)
	}
	t.Fatal(msg)
}
