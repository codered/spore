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
	agent *agent.Agent
	store *store.Store
	cfg   *config.Config
	guard *policy.Guard
	hub   *Hub

	// base bounds every turn's lifetime. It is the SERVER's context, never a
	// request's: a turn survives the client that started it (spec invariant
	// 2), so a handler must never hand its own context to agent.Run.
	base   context.Context
	cancel context.CancelFunc
}

func New(o Options) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		agent: o.Agent, store: o.Store, cfg: o.Cfg, guard: o.Guard,
		hub: NewHub(), base: ctx, cancel: cancel,
	}
}

func (s *Server) Hub() *Hub { return s.hub }

// Close cancels every in-flight turn. Run calls it on shutdown.
func (s *Server) Close() { s.cancel() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleShowSession)
	mux.HandleFunc("POST /api/sessions/{id}/messages", s.handlePostMessage)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleEvents)
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
