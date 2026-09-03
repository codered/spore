package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

// redialDelay is how long slowRedialCommand's wrapper sleeps on every launch
// after the first. It must comfortably outlast waitFor's poll interval so
// the "down" state between markDown and the redial's swap is guaranteed
// observable rather than merely likely to be sampled.
const redialDelay = 200 * time.Millisecond

// slowRedialCommand wraps bin in a shell script that execs it immediately on
// its first invocation, then sleeps redialDelay before exec'ing it on every
// later invocation (tracked with a marker file in a fresh temp dir).
//
// superviseOne redials a dead server through the same success path as its
// very first connect: connect() either fails (and only then does backoff
// apply) or it succeeds outright, and here it always succeeds, so
// h.backoffMin/backoffMax never gate this redial — a local respawn of an
// already-working binary is not a failure. Widening the observable
// down-window therefore has to happen in the process being spawned, not in
// the supervisor's retry timer: this wrapper is what turns "the window
// happened to be wider than one poll tick" into "the window is provably at
// least redialDelay wide," independent of how fast this machine can fork,
// exec and complete an MCP handshake.
func slowRedialCommand(t *testing.T, bin string) (command string, args []string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "started")
	// $0 is the conventional placeholder for a -c script's own name, $1 is
	// the marker file, $2 is the real binary, $3 is the delay in seconds.
	// Passing paths as positional parameters (rather than interpolating them
	// into the script text) sidesteps shell-quoting concerns entirely.
	script := `if [ -e "$1" ]; then sleep "$3"; else touch "$1"; fi; exec "$2"`
	return "/bin/sh", []string{"-c", script, "sh", marker, bin, fmt.Sprintf("%f", redialDelay.Seconds())}
}

// A server that dies is redialled and its tools come back. This is the whole
// point of a live registry: a flapping server heals without restarting spore.
func TestSupervisorRedialsADeadServer(t *testing.T) {
	bin := buildProbe(t)
	cmd, args := slowRedialCommand(t, bin)
	h := New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "probe", Transport: "stdio", Command: cmd, Args: args},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
	// This no longer has to be small for the down-window's sake — the
	// wrapper's redialDelay is what makes that window wide now — but it is
	// left small because backoff only governs a genuine connect failure
	// (e.g. the wrapper script itself misbehaving), and a real failure here
	// should still be retried quickly rather than papered over by a slow
	// backoff during a test.
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
		// An ordinary poll tick. TestSupervisorRedialsADeadServer no longer
		// depends on out-sampling a race here: slowRedialCommand makes its
		// down-window provably wider than this on its own.
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal(msg)
}
