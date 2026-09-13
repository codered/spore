package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/subagent"
)

// heldRunner keeps a child's turn open until release is closed or the child
// is cancelled, so a test can act on a sub-agent that is still running.
type heldRunner struct{ release chan struct{} }

func (h heldRunner) RunSite(ctx context.Context, sessionID, input, site string) (<-chan subagent.Event, error) {
	ch := make(chan subagent.Event, 1)
	go func() {
		defer close(ch)
		select {
		case <-h.release:
			ch <- subagent.Event{Text: "ok"}
		case <-ctx.Done():
			ch <- subagent.Event{Err: ctx.Err()}
		}
	}()
	return ch, nil
}

func withSupervisor(t *testing.T, s *Server, r subagent.Runner) *subagent.Supervisor {
	t.Helper()
	sup := subagent.New(s.store, config.SubagentConfig{MaxDepth: 2, MaxCostUSD: 1, MaxConcurrent: 4})
	sup.AttachRunner(r)
	sup.AllowDetached(true)
	s.AttachSubagents(sup)
	return sup
}

func agentRequest(s *Server, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func agentSession(t *testing.T, s *Server, title string) string {
	t.Helper()
	id, err := s.store.CreateSession(context.Background(), title, "/tmp/ws")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestAgentsEndpointListsChildren(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	withSupervisor(t, s, heldRunner{release: make(chan struct{})})
	parent := agentSession(t, s, "parent")

	child, err := s.store.CreateChildSession(ctx, "child", "/tmp/ws", parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.StartSubagentRun(ctx, store.SubagentRun{
		SessionID: child, ParentID: parent, Prompt: "audit the tests", Depth: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.FinishSubagentRun(ctx, child, store.RunDone, "no findings", ""); err != nil {
		t.Fatal(err)
	}

	rec := agentRequest(s, "GET", "/api/sessions/"+parent+"/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Agents []subagent.Status `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(got.Agents))
	}
	a := got.Agents[0]
	if a.ID != child || a.State != store.RunDone || a.Result != "no findings" || a.Prompt != "audit the tests" {
		t.Errorf("agent = %+v, want the finished child %s", a, child)
	}
}

func TestAgentsEndpointWithoutASupervisorIsEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	parent := agentSession(t, s, "parent")

	rec := agentRequest(s, "GET", "/api/sessions/"+parent+"/agents")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"agents":[]`) {
		t.Errorf("body = %s, want an empty agents array rather than null", rec.Body.String())
	}
}

func TestCancelUnknownAgentIsNotFound(t *testing.T) {
	s, _ := newTestServer(t)
	withSupervisor(t, s, heldRunner{release: make(chan struct{})})
	parent := agentSession(t, s, "parent")

	rec := agentRequest(s, "DELETE", "/api/sessions/"+parent+"/agents/nope")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestCancelStopsOnlyTheSessionsOwnChild(t *testing.T) {
	ctx := context.Background()
	release := make(chan struct{})
	defer close(release)
	s, _ := newTestServer(t)
	sup := withSupervisor(t, s, heldRunner{release: release})
	parent := agentSession(t, s, "parent")
	other := agentSession(t, s, "other")

	pctx := policy.WithSession(ctx, policy.Session{ID: parent, Profile: policy.ProfileLocal, Workspace: "/tmp/ws"})
	id, err := sup.Spawn(pctx, parent, "long job")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Another session may not stop this one's child.
	if rec := agentRequest(s, "DELETE", "/api/sessions/"+other+"/agents/"+id); rec.Code != http.StatusNotFound {
		t.Errorf("cancel from another session: status = %d, want 404", rec.Code)
	}
	if got, _ := sup.Result(ctx, id); got.State != store.RunRunning {
		t.Fatalf("state = %q after a refused cancel, want running", got.State)
	}

	if rec := agentRequest(s, "DELETE", "/api/sessions/"+parent+"/agents/"+id); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel from the parent: status = %d, want 204: %s", rec.Code, rec.Body.String())
	}
	if got, _ := sup.Result(ctx, id); got.State != store.RunInterrupted {
		t.Errorf("state = %q after cancel, want interrupted", got.State)
	}

	if rec := agentRequest(s, "DELETE", "/api/sessions/"+parent+"/agents/"+id); rec.Code != http.StatusConflict {
		t.Errorf("second cancel: status = %d, want 409", rec.Code)
	}
}
