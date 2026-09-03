package mcp

import (
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
)

// hostWithProbe returns a host configured with one stdio server backed by the
// envprobe fixture, plus one server that cannot possibly start.
func hostWithProbe(t *testing.T) *Host {
	t.Helper()
	bin := buildProbe(t)
	return New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "probe", Transport: "stdio", Command: bin},
		{Name: "broken", Transport: "stdio", Command: "/nonexistent/definitely-not-here"},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
}

func TestDialAllKeepsTheGoodServerAndRecordsTheBad(t *testing.T) {
	h := hostWithProbe(t)
	defer h.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.DialAll(ctx)

	var names []string
	for _, s := range h.Specs() {
		names = append(names, s.Name)
	}
	if !slices.Contains(names, "mcp__probe__probe") {
		t.Errorf("Specs() = %v, want the working server's tool", names)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "mcp__broken__") {
			t.Errorf("Specs() = %v, want nothing from the server that failed to start", names)
		}
	}

	var sawBroken bool
	for _, st := range h.Status() {
		if st.Name != "broken" {
			continue
		}
		sawBroken = true
		if st.State != "down" || st.LastErr == "" {
			t.Errorf("broken status = %+v, want state down with an error", st)
		}
	}
	if !sawBroken {
		t.Error("Status() did not report the broken server at all")
	}
}

// A server that is down has no snapshot, so its tools are absent and a call
// to one gets the registry's ordinary unknown-tool error. There are no stale
// adapters returning connection errors.
func TestLookupMissesWhileDown(t *testing.T) {
	h := hostWithProbe(t)
	defer h.Close()

	if _, ok := h.Lookup("mcp__probe__probe"); ok {
		t.Error("Lookup found a tool before the host dialled anything")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.DialAll(ctx)

	if _, ok := h.Lookup("mcp__probe__probe"); !ok {
		t.Error("Lookup missed a tool from a connected server")
	}
	if _, ok := h.Lookup("mcp__broken__anything"); ok {
		t.Error("Lookup found a tool on a server that never connected")
	}

	h.Close()
	if _, ok := h.Lookup("mcp__probe__probe"); ok {
		t.Error("Lookup still finds tools after Close")
	}
}

func TestStatusReportsSkippedTools(t *testing.T) {
	cs := serveInMemory(t, echoServer(t))
	h := New(config.MCPConfig{}, "/ws", slog.New(slog.DiscardHandler))
	st := &serverState{cfg: config.MCPServer{Name: "notion", Transport: "stdio"}}
	h.servers = append(h.servers, st)

	tools := listTools(t, cs)
	// st.gen is its zero value here (a freshly built serverState); passing it
	// back is "install unconditionally", matching swap's old behavior before
	// the generation check existed.
	h.swap(st, st.gen, newSnapshot("notion", cs, append(tools, tools[0]), time.Minute))

	got := h.Status()
	if len(got) != 1 || len(got[0].Skipped) != 1 {
		t.Fatalf("Status() = %+v, want one server reporting one skipped tool", got)
	}
	if !slices.Contains(got[0].Tools, "mcp__notion__search") {
		t.Errorf("Status().Tools = %v, want the registered name", got[0].Tools)
	}
}

// A relist that captured its session and generation before the server went
// down must not resurrect it. relist's own listing round-trip completes
// using the session it already had in hand; what matters is that its
// trailing swap call, arriving after markDown, is rejected because the
// generation it captured no longer matches. Task 6's supervisor is what
// makes a relist actually racing a markDown like this reachable — this test
// drives the same real functions (relist, markDown, swap) that the
// supervisor will call, rather than hand-setting serverState fields, so it
// exercises the actual race rather than a stand-in for it.
func TestRelistDropsAStaleGeneration(t *testing.T) {
	cs := serveInMemory(t, echoServer(t))
	h := New(config.MCPConfig{}, "/ws", slog.New(slog.DiscardHandler))
	st := &serverState{cfg: config.MCPServer{Name: "notion", Transport: "stdio"}, session: cs}
	h.servers = append(h.servers, st)

	// Install an initial snapshot through the real relist path, exactly as
	// connect does after a successful dial.
	if err := h.relist(context.Background(), st); err != nil {
		t.Fatalf("relist: %v", err)
	}
	if _, ok := h.Lookup("mcp__notion__search"); !ok {
		t.Fatal("relist did not install the initial snapshot")
	}

	// Stand in for a second, slower relist: capture the generation current
	// right now (as relist's own entry would), and complete its listing
	// round-trip while the session is still alive. Only its trailing swap
	// call is delayed past the down event below.
	st.mu.RLock()
	staleGen := st.gen
	st.mu.RUnlock()
	tools := listTools(t, cs)

	h.markDown(st, errors.New("connection reset"))

	// The stale relist's trailing swap call, arriving after the server went
	// down. Without the generation check this unconditionally reinstalls a
	// snapshot bound to the now-closed session and marks the server up.
	h.swap(st, staleGen, newSnapshot("notion", cs, tools, time.Minute))

	if _, ok := h.Lookup("mcp__notion__search"); ok {
		t.Error("Lookup found a tool from a snapshot swapped in after the server went down")
	}
	got := h.Status()
	if len(got) != 1 || got[0].State != StateDown || got[0].LastErr == "" {
		t.Fatalf("Status() = %+v, want the server still down with its error intact", got)
	}
}

// Close must coordinate with a dial that is still in flight: if it doesn't,
// a connect goroutine that finishes after Close has already returned can
// install a session (and leave its child process running) that nobody will
// ever tear down, contradicting Close's own documented contract that
// nothing outlives it. The outcome must hold regardless of which of Close
// or the dial's closed-flag check gets there first, so this test does not
// try to pin the interleaving — it races them and checks the postcondition.
func TestCloseTerminatesADialInFlight(t *testing.T) {
	bin := buildProbe(t)
	h := New(config.MCPConfig{Servers: []config.MCPServer{
		{Name: "probe", Transport: "stdio", Command: bin},
	}}, t.TempDir(), slog.New(slog.DiscardHandler))
	st := h.servers[0]

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- h.connect(ctx, st) }()
	h.Close()
	<-done // wait for the raced dial to fully settle before inspecting state

	st.mu.RLock()
	session, cmd, snap, state := st.session, st.cmd, st.snap, st.state
	st.mu.RUnlock()
	if session != nil || cmd != nil || snap != nil {
		t.Fatalf("after Close raced a dial: session=%v cmd=%v snap=%v, want all nil", session, cmd, snap)
	}
	if state != StateDown {
		t.Errorf("state = %q, want %q", state, StateDown)
	}

	// The child process itself must be gone, not merely forgotten by
	// spore's bookkeeping. pgrep against the fixture's unique per-test
	// binary path is a reliable way to observe it; poll with a deadline
	// rather than sleep, since SIGKILL delivery and reaping are async.
	waitForNoProcess(t, bin, 5*time.Second)
}

// waitForNoProcess polls until no process is running whose command line
// contains needle, or fails the test once deadline has passed. A small
// local stand-in for the waitFor-style polling idiom Task 6 introduces:
// deadline-bounded polling, not a bare sleep, since exactly how long
// shutdown takes is not something a test should assume.
func waitForNoProcess(t *testing.T, needle string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		out, err := exec.Command("pgrep", "-f", needle).Output()
		if err != nil || len(strings.TrimSpace(string(out))) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process matching %q still running after %v (pgrep: %s)", needle, timeout, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
