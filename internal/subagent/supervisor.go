// Package subagent runs one agent on behalf of another. It is the single
// owner of what is running: the tools launch through it, the daemon endpoint
// reads it, and the startup sweep reconciles it with the store.
package subagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// Event is the part of agent.Event the supervisor consumes. Depending on the
// narrow shape rather than on *agent.Agent is what lets the tests drive a
// supervisor without a provider.
type Event = agent.Event

// Runner is the agent seen from here. agent.Agent satisfies it.
type Runner interface {
	RunSite(ctx context.Context, sessionID, input, site string) (<-chan Event, error)
}

// Status is one child, live or finished. It is what agent_result returns to
// the model and what the endpoint renders.
type Status struct {
	ID      string    `json:"id"`
	Prompt  string    `json:"prompt"`
	State   string    `json:"state"`
	Result  string    `json:"result,omitempty"`
	Error   string    `json:"error,omitempty"`
	Depth   int       `json:"depth"`
	CostUSD float64   `json:"cost_usd"`
	Started time.Time `json:"started_at"`
	Ended   time.Time `json:"ended_at,omitempty"`
}

type child struct {
	cancel context.CancelFunc
	prompt string
	depth  int
	start  time.Time
	root   string
}

type Supervisor struct {
	mu      sync.Mutex
	running map[string]*child
	runner  Runner
	store   *store.Store
	cfg     config.SubagentConfig
}

func New(st *store.Store, cfg config.SubagentConfig) *Supervisor {
	return &Supervisor{running: map[string]*child{}, store: st, cfg: cfg}
}

// Attach supplies the agent. buildAgent constructs the registry before the
// Agent, so the supervisor is built empty, the tools are registered against
// it, and the agent arrives last.
func (s *Supervisor) Attach(a *agent.Agent) { s.AttachRunner(a) }

// AttachRunner is Attach against the narrow interface, which is what the
// tests use.
func (s *Supervisor) AttachRunner(r Runner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runner = r
}

// admit decides whether one more child may start under this parent, and
// returns the depth it would run at and the root session ID. Every refusal is
// an ordinary error: the caller turns it into a tool error the model can read
// and route around. The concurrency cap is not checked here; it is enforced
// atomically by tryTrack to close the check-then-insert race.
func (s *Supervisor) admit(ctx context.Context, parentID string) (int, string, error) {
	ancestors, err := s.store.SessionAncestors(ctx, parentID)
	if err != nil {
		return 0, "", err
	}
	depth := len(ancestors) + 1
	if depth >= s.cfg.MaxDepth {
		return 0, "", fmt.Errorf("refusing to launch: this would be depth %d and the configured max_depth is %d. Do the work in this agent instead", depth, s.cfg.MaxDepth)
	}

	root := parentID
	if len(ancestors) > 0 {
		root = ancestors[len(ancestors)-1]
	}
	spent, err := s.store.TreeCost(ctx, root)
	if err != nil {
		return 0, "", err
	}
	if spent >= s.cfg.MaxCostUSD {
		return 0, "", fmt.Errorf("refusing to launch: this agent tree has spent $%.4f of its $%.2f max_cost_usd. Do the work in this agent instead", spent, s.cfg.MaxCostUSD)
	}

	return depth, root, nil
}

// start creates the child session and its run row, and returns the context
// the child turn runs under. The child inherits the parent's profile and
// workspace: inheriting the profile is what stops a sub-agent reaching
// further than whoever launched it, and inheriting the workspace keeps its
// filesystem calls inside the same ceiling.
func (s *Supervisor) start(ctx context.Context, parentID, prompt string, depth int) (string, context.Context, error) {
	parent := policy.SessionFrom(ctx)
	childID, err := s.store.CreateChildSession(ctx, prompt, parent.Workspace, parentID)
	if err != nil {
		return "", nil, err
	}
	err = s.store.StartSubagentRun(ctx, store.SubagentRun{
		SessionID: childID, ParentID: parentID, Prompt: prompt, Depth: depth,
	})
	if err != nil {
		return "", nil, err
	}
	childCtx := policy.WithSession(ctx, policy.Session{
		ID: childID, Profile: parent.Profile, Workspace: parent.Workspace,
	})
	return childID, childCtx, nil
}

// drain consumes a child's turn to completion and returns its final text.
func drain(ch <-chan Event) (string, error) {
	var text string
	var turnErr error
	for ev := range ch {
		switch {
		case ev.Err != nil:
			turnErr = ev.Err
		case ev.Text != "":
			text += ev.Text
		}
	}
	return text, turnErr
}

// Run launches a child and blocks until it settles.
func (s *Supervisor) Run(ctx context.Context, parentID, prompt string) (Status, error) {
	depth, root, err := s.admit(ctx, parentID)
	if err != nil {
		return Status{}, err
	}
	s.mu.Lock()
	runner := s.runner
	s.mu.Unlock()
	if runner == nil {
		return Status{}, fmt.Errorf("sub-agents are not available: no agent is attached")
	}

	childID, childCtx, err := s.start(ctx, parentID, prompt, depth)
	if err != nil {
		return Status{}, err
	}
	childCtx, cancel := context.WithCancel(childCtx)

	// Try to reserve a slot under the root. If the cap is exceeded, finish the
	// run row before returning the error so it reaches a terminal state.
	err = s.tryTrack(childID, root, &child{cancel: cancel, prompt: prompt, depth: depth, start: time.Now().UTC(), root: root})
	if err != nil {
		s.finish(ctx, childID, store.RunFailed, "", err.Error())
		cancel()
		return Status{}, err
	}
	defer s.untrack(childID)
	defer cancel()

	ch, err := runner.RunSite(childCtx, childID, prompt, router.SiteSubagent)
	if err != nil {
		s.finish(ctx, childID, store.RunFailed, "", err.Error())
		return Status{}, err
	}
	text, turnErr := drain(ch)
	if turnErr != nil {
		s.finish(ctx, childID, store.RunFailed, text, turnErr.Error())
		return Status{}, turnErr
	}
	s.finish(ctx, childID, store.RunDone, text, "")
	return s.Result(ctx, childID)
}

func (s *Supervisor) track(id string, c *child) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running[id] = c
}

func (s *Supervisor) untrack(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.running, id)
}

// tryTrack atomically counts children under the given root and reserves a slot
// if one is available under the max_concurrent cap. It acquires the mutex once
// for the count-and-insert to close the race between admit's check and track's
// insert. If the cap is reached under this root, it returns an error naming
// max_concurrent and the child is not inserted.
func (s *Supervisor) tryTrack(id string, root string, c *child) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Count how many children are already running under this root.
	live := 0
	for _, ch := range s.running {
		if ch.root == root {
			live++
		}
	}

	if live >= s.cfg.MaxConcurrent {
		return fmt.Errorf("refusing to launch: %d sub-agents are already running under this root and max_concurrent is %d. Wait for one to finish", live, s.cfg.MaxConcurrent)
	}

	s.running[id] = c
	return nil
}

// finish records the terminal state. It uses a context detached from the
// child's own, because a cancelled child must still record that it stopped.
func (s *Supervisor) finish(ctx context.Context, childID, state, result, errText string) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.FinishSubagentRun(writeCtx, childID, state, result, errText); err != nil {
		// The transcript is the record; a lost bookkeeping row costs the
		// listing accuracy, never the work.
		_ = err
	}
}

// Result reads one child's status, live or finished.
func (s *Supervisor) Result(ctx context.Context, childID string) (Status, error) {
	run, ok, err := s.store.SubagentRun(ctx, childID)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, fmt.Errorf("no sub-agent %s", childID)
	}
	return s.statusOf(ctx, run)
}

func (s *Supervisor) statusOf(ctx context.Context, run store.SubagentRun) (Status, error) {
	cost, err := s.store.TreeCost(ctx, run.SessionID)
	if err != nil {
		return Status{}, err
	}
	return Status{
		ID: run.SessionID, Prompt: run.Prompt, State: run.State,
		Result: run.Result, Error: run.Error, Depth: run.Depth,
		CostUSD: cost, Started: run.StartedAt, Ended: run.EndedAt,
	}, nil
}

// List reports one parent's children, running and finished.
func (s *Supervisor) List(ctx context.Context, parentID string) ([]Status, error) {
	runs, err := s.store.SubagentRunsByParent(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make([]Status, 0, len(runs))
	for _, r := range runs {
		st, err := s.statusOf(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}
