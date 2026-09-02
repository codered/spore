package mcp

import (
	"context"
	"log/slog"
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
	h.swap(st, newSnapshot("notion", cs, append(tools, tools[0]), time.Minute))

	got := h.Status()
	if len(got) != 1 || len(got[0].Skipped) != 1 {
		t.Fatalf("Status() = %+v, want one server reporting one skipped tool", got)
	}
	if !slices.Contains(got[0].Tools, "mcp__notion__search") {
		t.Errorf("Status().Tools = %v, want the registered name", got[0].Tools)
	}
}
