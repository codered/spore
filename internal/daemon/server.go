package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/refine"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/subagent"
)

// Options are the daemon's collaborators. Guard may be nil in tests that do
// not exercise tools; everything else is required.
type Options struct {
	Agent *agent.Agent
	Store *store.Store
	Cfg   *config.Config
	Guard *policy.Guard
	Token string // Token is the daemon credential every /api route requires. Empty refuses every /api request.
}

type Server struct {
	agent *agent.Agent
	store *store.Store
	cfg   *config.Config
	guard *policy.Guard
	hub   *Hub
	// cleaner is the chat surface a delete may ask to clean up; nil when no
	// bridge is connected.
	cleaner Cleaner
	// notifier is the chat surface job reports reach besides the TUI; nil
	// when no bridge is connected.
	notifier Notifier
	broker   *Broker

	// subagents is the supervisor the sub-agent tools launch through. It is
	// nil in tests that do not exercise sub-agents.
	subagents *subagent.Supervisor

	// refiner runs continual refinement. Nil means the routes answer 503.
	refiner *refine.Refiner

	// op holds the operator views' subsystems. Its nil fields make their
	// routes answer 503.
	op Operator

	// titler names sessions from their first message. Nil means sessions
	// are named from the message's first line instead.
	titler Titler
	// naming holds the sessions a name is being made for, so two quick
	// turns do not make two calls.
	naming sync.Map

	// base bounds every turn's lifetime. It is the SERVER's context, never a
	// request's: a turn survives the client that started it (spec invariant
	// 2), so a handler must never hand its own context to agent.Run.
	base   context.Context
	cancel context.CancelFunc

	token string
	// apiPatterns records every route registered through api(), for the test that walks them.
	apiPatterns []string
}

func New(o Options) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		agent: o.Agent, store: o.Store, cfg: o.Cfg, guard: o.Guard,
		hub: NewHub(), base: ctx, cancel: cancel,
		token: o.Token,
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

// Token is the credential in-process tests hand to a client.
func (s *Server) Token() string { return s.token }

// Attach supplies the agent and guard after construction. The daemon owns
// the approver the guard is built with, so the two cannot both be passed to
// New; this is the seam where the cycle is broken.
func (s *Server) Attach(a *agent.Agent, g *policy.Guard) {
	s.agent = a
	s.guard = g
}

// AttachSubagents supplies the sub-agent supervisor. It arrives with the
// agent, after New, for the same reason Attach exists. Attaching is also what
// makes children visible: the server observes the supervisor and publishes
// each child onto the hub.
func (s *Server) AttachSubagents(sup *subagent.Supervisor) {
	s.subagents = sup
	if sup != nil {
		sup.SetObserver(childPublisher{s})
	}
}

// Subagents is the supervisor the daemon serves /agents from.
func (s *Server) Subagents() *subagent.Supervisor { return s.subagents }

// AttachTitler supplies the session titler. Like Attach, it arrives after New.
func (s *Server) AttachTitler(t Titler) { s.titler = t }

// AttachRefiner supplies the Refiner. Like Attach, it arrives after New.
func (s *Server) AttachRefiner(r *refine.Refiner) { s.refiner = r }

// Refiner is the attached Refiner, or nil.
func (s *Server) Refiner() *refine.Refiner { return s.refiner }

// TurnRunning reports whether a session has a turn in flight. The idle
// sweeper skips those.
func (s *Server) TurnRunning(id string) bool { return s.hub.Running(id) }

// PublishNote shows a note row live in any view of the session. Refinement
// uses the job-note event: the TUI renders both the same way.
func (s *Server) PublishNote(sessionID, text string) {
	s.hub.Publish(sessionID, WireEvent{Type: WireJobNote, Text: text})
}

// Close cancels every in-flight turn. Run calls it on shutdown.
func (s *Server) Close() { s.cancel() }

// buildMux creates the HTTP routes for the daemon. It's a separate method so
// tests can access the raw mux without the host check wrapper.
func (s *Server) buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	s.apiPatterns = nil
	// api registers a route behind the token. Every /api route goes through
	// it; a test fails if one is registered with mux.HandleFunc directly.
	api := func(pattern string, h http.HandlerFunc) {
		s.apiPatterns = append(s.apiPatterns, pattern)
		mux.HandleFunc(pattern, s.requireToken(h))
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	api("GET /api/sessions", s.handleListSessions)
	api("POST /api/sessions", s.handleCreateSession)
	api("PATCH /api/sessions/{id}", s.handlePatchSession)
	api("POST /api/sessions/delete", s.handleDeleteSessions)
	api("POST /api/sessions/{id}/seen", s.handleSeen)
	api("GET /api/sessions/{id}", s.handleShowSession)
	api("POST /api/sessions/{id}/messages", s.handlePostMessage)
	api("POST /api/sessions/{id}/stop", s.handleStop)
	api("GET /api/events", s.handleAllEvents)
	api("GET /api/sessions/{id}/events", s.handleEvents)
	api("POST /api/sessions/{id}/compact", s.handleCompact)
	api("POST /api/sessions/{id}/clear", s.handleClear)
	api("POST /api/sessions/{id}/refine", s.handleRefine)
	api("POST /api/sessions/{id}/refine/rollback", s.handleRefineRollback)
	api("GET /api/refinements", s.handleListRefinements)
	api("POST /api/refinements/{id}/accept", s.handleAcceptRefinement)
	api("POST /api/refinements/{id}/reject", s.handleRejectRefinement)
	api("GET /api/sessions/{id}/skills", s.handleSkills)
	api("GET /api/sessions/{id}/agents", s.handleAgents)
	api("DELETE /api/sessions/{id}/agents/{child}", s.handleCancelAgent)
	api("GET /api/sessions/{id}/approvals", s.handleListApprovals)
	api("POST /api/sessions/{id}/approvals/{pending}", s.handleResolveApproval)
	api("GET /api/jobs", s.handleListJobs)
	api("POST /api/jobs", s.handleCreateJob)
	api("DELETE /api/jobs/{id}", s.handleCancelJob)
	api("GET /api/jobs/{id}/runs", s.handleJobRuns)
	api("GET /api/usage", s.handleUsage)
	api("GET /api/policy", s.handlePolicy)
	api("DELETE /api/policy/learned", s.handleRevoke)
	api("GET /api/memory", s.handleMemory)
	api("DELETE /api/memory/{name}", s.handleDeleteFact)
	api("GET /api/mcp", s.handleMCP)
	api("POST /api/mcp/{server}/reconnect", s.handleReconnect)
	mux.HandleFunc("GET /static/{file}", s.handleStatic)
	mux.HandleFunc("GET /", s.handleIndex)
	return mux
}

func (s *Server) Handler() http.Handler {
	return s.checkHost(s.buildMux())
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
