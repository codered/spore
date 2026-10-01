package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// Through the real wiring: a "pattern" answer makes the next matching call
// run without asking, and a revoke over HTTP makes it ask again. No restart.
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

	// A learned allow never outranks a hand-written ask, so fs_write must
	// reach the profile default for a "pattern" answer to apply.
	cfg.Policy.Ask = slices.DeleteFunc(slices.Clone(cfg.Policy.Ask), func(r string) bool { return r == "fs_write" })

	srv, host, _, err := buildServer(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	defer host.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ws := filepath.Join(dir, "ws")
	_ = os.MkdirAll(ws, 0o700)
	sid, _ := st.CreateSession(context.Background(), "t", ws)
	ctx := policy.WithSession(context.Background(), policy.Session{ID: sid, Profile: policy.ProfileLocal, Workspace: ws})
	g := srv.Guard()
	call := func(id string) provider.Block {
		args, _ := json.Marshal(map[string]string{"path": filepath.Join(ws, "a.txt"), "content": "x"})
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

	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if res := g.Run(quick, call("c2")); res.IsError {
		t.Fatalf("the learned rule did not apply without a restart: %s", res.Content)
	}

	learned, _ := config.ReadLearned(path)
	if len(learned.Allow) != 1 {
		t.Fatalf("learned = %+v", learned)
	}
	body, _ := json.Marshal(map[string]string{"decision": "allow", "rule": learned.Allow[0]})
	req, _ := http.NewRequest("DELETE", ts.URL+"/api/policy/learned", strings.NewReader(string(body)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("revoke: %v %v", resp, err)
	}
	resp.Body.Close()

	go func() { done <- g.Run(ctx, call("c3")) }()
	answer(false, "once") // it asked again: the revoke is live
	<-done
}
