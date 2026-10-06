package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// authedHandler adds the daemon token to every request that has no Authorization header.
func authedHandler(h http.Handler, token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		h.ServeHTTP(w, r)
	})
}

// Through the real wiring: a "pattern" answer queues a proposal, accepting
// it through HTTP makes the next matching call run without asking, and a
// revoke over HTTP makes it ask again. No restart.
func TestLearnAndRevokeAreLiveThroughTheDaemon(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Path = path
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}

	srv, host, _, err := buildServer(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ts := httptest.NewServer(authedHandler(srv.Handler(), srv.Token()))
	defer ts.Close()

	ws := filepath.Join(dir, "ws")
	_ = os.MkdirAll(filepath.Join(ws, "notes"), 0o700)
	sid, _ := st.CreateSession(context.Background(), "t", ws)
	ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Profile: policy.ProfileLocal, Workspace: ws})
	g := srv.Guard()
	call := func(id string) provider.Block {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, "notes", "a.txt"), "content": "x"})
		return provider.Block{Type: provider.BlockToolUse, ID: id, Name: "fs_write", Input: args}
	}
	pending := func() []store.PendingCall {
		p, _ := g.Pending(context.Background(), sid)
		return p
	}
	answer := func(allow bool, scope string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for len(pending()) == 0 {
			if time.Now().After(deadline) {
				t.Fatal("no approval appeared")
			}
			time.Sleep(10 * time.Millisecond)
		}
		body, _ := json.Marshal(map[string]any{"allow": allow, "scope": scope})
		url := ts.URL + "/api/sessions/" + sid + "/approvals/" + strconv.FormatInt(pending()[0].ID, 10)
		resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
		if err != nil || resp.StatusCode >= 300 {
			t.Fatalf("answer: %v %v", resp, err)
		}
		resp.Body.Close()
	}

	done := make(chan provider.Block, 1)
	go func() { done <- g.Run(ctx, call("c1")) }()
	answer(true, "pattern")
	<-done

	// The "p" answer queued a proposal. Accept it through HTTP.
	proposals, _ := st.Refinements(context.Background(), store.RefineProposed, 10)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %d, want 1", len(proposals))
	}
	body, _ := json.Marshal(map[string]bool{"accept": true})
	url := ts.URL + "/api/refinements/" + strconv.FormatInt(proposals[0].ID, 10) + "/accept"
	resp, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil || resp.StatusCode >= 300 {
		t.Fatalf("accept proposal: %v %v", resp, err)
	}
	resp.Body.Close()

	// The accepted rule should now apply.
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if res := g.Run(quick, call("c2")); res.IsError {
		t.Fatalf("the accepted rule did not apply: %s", res.Content)
	}

	learned, _ := config.ReadLearned(path)
	if len(learned.Allow) != 1 {
		t.Fatalf("learned = %+v", learned)
	}
	body, _ = json.Marshal(map[string]string{"decision": "allow", "rule": learned.Allow[0]})
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/policy/learned", strings.NewReader(string(body)))
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", resp, err)
	}
	resp.Body.Close()

	go func() { done <- g.Run(ctx, call("c3")) }()
	answer(false, "once") // it asked again: the revoke is live
	<-done
}

// Rolling back an accepted proposal's approval round removes the rule from
// the managed block and from the running engine. No restart.
func TestRollingBackAnAcceptedProposalRemovesTheRule(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Path = path
	cfg.DataDir = dir
	cfg.DefaultModel = "anthropic/claude-opus-5"
	cfg.Providers = map[string]config.ProviderConfig{"anthropic": {Kind: "anthropic", APIKey: "sk-x"}}
	srv, host, _, err := buildServer(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ts := httptest.NewServer(authedHandler(srv.Handler(), srv.Token()))
	defer ts.Close()

	ctx := context.Background()
	ws := filepath.Join(dir, "ws")
	sid, err := st.CreateSession(ctx, "t", ws)
	if err != nil {
		t.Fatal(err)
	}
	rule := "fs_write(path matches " + filepath.Join(ws, "notes") + "/**)"
	after := rule
	id, err := st.AddRefinement(ctx, store.Refinement{
		RoundID: "approval-1", SessionID: sid, Trigger: store.RefineTriggerApproval,
		Kind: store.KindPolicyAllow, Target: rule, After: &after, Rationale: "r", Status: store.RefineProposed,
	})
	if err != nil {
		t.Fatal(err)
	}
	post := func(url, body string) {
		t.Helper()
		resp, err := http.Post(ts.URL+url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: %d", url, resp.StatusCode)
		}
	}
	decide := func() policy.Decision {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, "notes", "a.txt")})
		return srv.Guard().Engine().Evaluate(policy.Session{Profile: policy.ProfileLocal, Workspace: ws},
			policy.Call{Tool: "fs_write", Args: args}).Decision
	}

	post("/api/refinements/"+strconv.FormatInt(id, 10)+"/accept", "")
	if got := decide(); got != policy.DecisionAllow {
		t.Fatalf("after accept: %s, want allow", got)
	}
	post("/api/sessions/"+sid+"/refine/rollback", `{"round_id":"approval-1"}`)
	learned, err := config.ReadLearned(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(learned.Allow) != 0 {
		t.Errorf("learned allow after rollback = %v, want none", learned.Allow)
	}
	if got := decide(); got != policy.DecisionAsk {
		t.Errorf("after rollback: %s, want ask", got)
	}
	row, _, _ := st.Refinement(ctx, id)
	if row.Status != store.RefineRolledBack {
		t.Errorf("status = %s, want rolled_back", row.Status)
	}
}
