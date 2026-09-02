package mcp

import (
	"context"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/tool"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// dialTimeout bounds one connect-and-list attempt, so a server that accepts a
// connection and then says nothing cannot hold up startup.
const dialTimeout = 30 * time.Second

// State is what an operator sees in "spore mcp list".
const (
	StateUp   = "up"
	StateDown = "down"
)

// serverState is one configured server and whatever spore currently knows
// about it. snap is nil whenever the server is down, which is what makes a
// down server's tools simply absent rather than present and broken.
type serverState struct {
	cfg config.MCPServer

	mu      sync.RWMutex
	snap    *snapshot
	state   string
	lastErr error
	session *sdk.ClientSession
	cmd     *exec.Cmd

	// changed carries tools/list_changed notifications from the SDK's
	// dispatch goroutine to the supervisor, which does the re-listing. It is
	// buffered and written non-blockingly: a burst of notifications collapses
	// into one re-list, and the SDK's goroutine is never held up.
	changed chan struct{}
}

// Host owns spore's MCP client connections and presents their tools to the
// registry as a single dynamic Source.
type Host struct {
	workspace string
	log       *slog.Logger
	servers   []*serverState
}

func New(cfg config.MCPConfig, workspace string, log *slog.Logger) *Host {
	if log == nil {
		log = slog.Default()
	}
	h := &Host{workspace: workspace, log: log}
	for _, s := range cfg.Servers {
		h.servers = append(h.servers, &serverState{
			cfg:     s,
			state:   StateDown,
			changed: make(chan struct{}, 1),
		})
	}
	return h
}

// Configured reports whether any server is declared, so callers can skip
// wiring a host that would do nothing.
func (h *Host) Configured() bool { return len(h.servers) > 0 }

// Specs implements tool.Source.
func (h *Host) Specs() []provider.ToolSpec {
	var out []provider.ToolSpec
	for _, st := range h.servers {
		snap := st.snapshot()
		if snap == nil {
			continue
		}
		for _, t := range snap.tools {
			out = append(out, provider.ToolSpec{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
		}
	}
	return out
}

// Lookup implements tool.Source.
func (h *Host) Lookup(name string) (tool.Tool, bool) {
	for _, st := range h.servers {
		snap := st.snapshot()
		if snap == nil {
			continue
		}
		if t, ok := snap.tools[name]; ok {
			return t, true
		}
	}
	return nil, false
}

func (st *serverState) snapshot() *snapshot {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.snap
}

// swap installs a server's new tool set in one assignment. This is why a
// reconnect is invisible to a turn: the set never exists half-updated.
func (h *Host) swap(st *serverState, snap *snapshot) {
	st.mu.Lock()
	prev := st.snap
	st.snap = snap
	st.state = StateUp
	st.lastErr = nil
	st.mu.Unlock()

	for _, sk := range snap.skipped {
		h.log.Warn("mcp tool skipped", "server", st.cfg.Name, "tool", sk.Tool, "reason", sk.Reason)
	}
	// Log only a real change: a tool set that flaps is worth seeing in the
	// cost data, because each change invalidates the upstream prompt cache.
	if prev == nil || !sameNames(prev.names(), snap.names()) {
		h.log.Info("mcp tool set changed", "server", st.cfg.Name, "tools", len(snap.tools))
	}
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as, bs := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func (h *Host) markDown(st *serverState, err error) {
	st.mu.Lock()
	st.snap = nil
	st.state = StateDown
	st.lastErr = err
	session, cmd := st.session, st.cmd
	st.session, st.cmd = nil, nil
	st.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}
	killGroup(cmd)
}

// connect dials one server, lists its tools and installs the snapshot. A
// failure is returned for the caller to log and retry; it is never fatal.
func (h *Host) connect(ctx context.Context, st *serverState) error {
	transport, cmd, err := transportFor(st.cfg, h.workspace)
	if err != nil {
		h.markDown(st, err)
		return err
	}

	opts := &sdk.ClientOptions{
		// A server may announce that its tool list changed. Hand that to the
		// supervisor rather than re-listing here: this runs on the SDK's
		// dispatch goroutine, and calling back into the same session from it
		// invites a deadlock.
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) {
			select {
			case st.changed <- struct{}{}:
			default:
			}
		},
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "spore", Version: "0.1"}, opts)

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	session, err := client.Connect(dialCtx, transport, nil)
	if err != nil {
		h.markDown(st, err)
		killGroup(cmd)
		return err
	}

	st.mu.Lock()
	st.session, st.cmd = session, cmd
	st.mu.Unlock()

	if err := h.relist(ctx, st); err != nil {
		h.markDown(st, err)
		return err
	}
	return nil
}

// relist re-reads a connected server's tools and swaps in a fresh snapshot.
func (h *Host) relist(ctx context.Context, st *serverState) error {
	st.mu.RLock()
	session := st.session
	st.mu.RUnlock()
	if session == nil {
		return nil
	}

	listCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var tools []*sdk.Tool
	for t, err := range session.Tools(listCtx, nil) {
		if err != nil {
			return err
		}
		tools = append(tools, t)
	}
	h.swap(st, newSnapshot(st.cfg.Name, session, tools, st.cfg.CallTimeout()))
	return nil
}

// DialAll connects every configured server concurrently and returns once each
// has either connected or failed. A failure is logged, never fatal: spore's
// own tools and its web UI must keep working when someone's MCP server does
// not.
func (h *Host) DialAll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, st := range h.servers {
		wg.Add(1)
		go func(st *serverState) {
			defer wg.Done()
			if err := h.connect(ctx, st); err != nil {
				h.log.Warn("mcp server did not start", "server", st.cfg.Name, "err", err)
			}
		}(st)
	}
	wg.Wait()
}

// ServerStatus is one row of "spore mcp list".
type ServerStatus struct {
	Name      string
	Transport string
	State     string
	Tools     []string
	Skipped   []skip
	LastErr   string
}

func (h *Host) Status() []ServerStatus {
	out := make([]ServerStatus, 0, len(h.servers))
	for _, st := range h.servers {
		st.mu.RLock()
		row := ServerStatus{Name: st.cfg.Name, Transport: st.cfg.Transport, State: st.state}
		if st.lastErr != nil {
			row.LastErr = st.lastErr.Error()
		}
		if st.snap != nil {
			row.Tools = st.snap.names()
			row.Skipped = st.snap.skipped
		}
		st.mu.RUnlock()
		sort.Strings(row.Tools)
		out = append(out, row)
	}
	return out
}

// Close disconnects every server and kills any child that outlives its
// session, so a wedged server cannot outlive the daemon.
func (h *Host) Close() {
	for _, st := range h.servers {
		h.markDown(st, nil)
	}
}
