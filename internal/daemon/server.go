package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/workspace"
)

// Options are the daemon's collaborators. Guard may be nil in tests that do
// not exercise tools; everything else is required.
type Options struct {
	Agent *agent.Agent
	Store *store.Store
	Cfg   *config.Config
	Guard *policy.Guard
}

type Server struct {
	agent  *agent.Agent
	store  *store.Store
	cfg    *config.Config
	guard  *policy.Guard
	hub    *Hub
	broker *Broker

	// base bounds every turn's lifetime. It is the SERVER's context, never a
	// request's: a turn survives the client that started it (spec invariant
	// 2), so a handler must never hand its own context to agent.Run.
	base   context.Context
	cancel context.CancelFunc
}

func New(o Options) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		agent: o.Agent, store: o.Store, cfg: o.Cfg, guard: o.Guard,
		hub: NewHub(), base: ctx, cancel: cancel,
	}
	s.broker = NewBroker(s.hub)
	return s
}

func (s *Server) Hub() *Hub { return s.hub }

// Subscribe attaches a non-HTTP client to a session's event stream. It is the
// same subscription the SSE handler uses; a bridge is not a special case.
func (s *Server) Subscribe(sessionID string) (<-chan WireEvent, func()) {
	return s.hub.Subscribe(sessionID)
}

// Store, Guard and Broker are the collaborators a bridge needs. They are
// accessors rather than constructor arguments because the daemon owns the
// approver the guard is built with, so a bridge cannot be wired before the
// server exists.
func (s *Server) Store() *store.Store  { return s.store }
func (s *Server) Guard() *policy.Guard { return s.guard }
func (s *Server) Broker() *Broker      { return s.broker }

// Approver is the policy.Approver the guard must be built with. The daemon
// creates it because it owns the hub the approval events travel over.
func (s *Server) Approver() policy.Approver { return s.broker }

// CreateSession is the one place a session's root is decided: the HTTP
// handler, the scheduler and the bridge all come through here, so the ceiling
// is checked once rather than in three places that can drift.
func (s *Server) CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error) {
	root, err := workspace.Root(workspace.Request{
		Requested:  requested,
		Ceiling:    s.cfg.Policy.Workspace,
		RemoteRoot: s.cfg.Policy.Profiles[string(policy.ProfileRemote)].Workspace,
		Remote:     profile == policy.ProfileRemote,
	})
	if err != nil {
		return "", err
	}
	return s.store.CreateSession(ctx, title, root)
}

// Attach supplies the agent and guard after construction. The daemon owns
// the approver the guard is built with, so the two cannot both be passed to
// New; this is the seam where the cycle is broken.
func (s *Server) Attach(a *agent.Agent, g *policy.Guard) {
	s.agent = a
	s.guard = g
}

// Close cancels every in-flight turn. Run calls it on shutdown.
func (s *Server) Close() { s.cancel() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleShowSession)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
	mux.HandleFunc("GET /api/sessions/{id}/approvals", s.handleListApprovals)
	mux.HandleFunc("POST /api/sessions/{id}/approvals/{pending}", s.handleResolveApproval)
	mux.HandleFunc("GET /api/jobs", s.handleListJobs)
	mux.HandleFunc("POST /api/jobs", s.handleCreateJob)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)
	mux.HandleFunc("GET /static/{file}", s.handleStatic)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

// Run serves until ctx is cancelled, then drains with a short grace period.
func (s *Server) Run(ctx context.Context, addr string) error {
	if err := config.ValidateDaemonAddr(addr); err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		s.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]string{"error": fmt.Sprintf(format, args...)})
}

var errSessionBusy = errors.New("the freshly created session already has a turn running")
