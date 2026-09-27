package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/refine"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

func attachRefiner(t *testing.T, s *Server, replies ...string) *refine.Refiner {
	t.Helper()
	var turns []provider.ScriptTurn
	for _, r := range replies {
		turns = append(turns, provider.ScriptTurn{Text: r})
	}
	reg := provider.NewRegistry()
	reg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{})
	rt, _ := router.New(nil, s.cfg.DefaultModel)
	facts := memory.NewCache(filepath.Join(s.cfg.DataDir, "memory"))
	ref := refine.New(s.Store(), reg, rt, s.cfg, facts)
	t.Cleanup(ref.Close)
	s.AttachRefiner(ref)
	return ref
}

func post(t *testing.T, url string, body any, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

const tabs = `{"edits":[{"kind":"fact.create","name":"prefers-tabs","type":"feedback","description":"indent","body":"Use tabs.","rationale":"user said"}]}`

func appendUser(t *testing.T, s *Server, sid, text string) {
	t.Helper()
	raw, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: text}})
	if _, err := s.Store().AppendMessage(context.Background(), store.Message{SessionID: sid, Role: "user", BlocksJSON: raw}); err != nil {
		t.Fatal(err)
	}
}

func TestRefineRouteProposeAcceptRollback(t *testing.T) {
	s, ts := newTestServer(t)
	attachRefiner(t, s, tabs)
	ctx := context.Background()
	sid, _ := s.Store().CreateSessionFrom(ctx, "d", "", store.SourceDiscord)
	raw, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: "I like tabs"}})
	_, _ = s.Store().AppendMessage(ctx, store.Message{SessionID: sid, Role: "user", BlocksJSON: raw})

	var res RefineResultJSON
	if code := post(t, ts.URL+"/api/sessions/"+sid+"/refine", map[string]string{"instructions": ""}, &res); code != 200 {
		t.Fatalf("refine: %d", code)
	}
	if len(res.Proposed) != 1 || len(res.Applied) != 0 {
		t.Fatalf("result = %+v", res)
	}

	resp, err := http.Get(ts.URL + "/api/refinements?status=proposed")
	if err != nil {
		t.Fatal(err)
	}
	var list []RefinementJSON
	_ = json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list) != 1 || list[0].Kind != "fact.create" {
		t.Fatalf("list = %+v", list)
	}

	var accepted RefinementJSON
	if code := post(t, ts.URL+"/api/refinements/"+strconv.FormatInt(list[0].ID, 10)+"/accept", nil, &accepted); code != 200 || accepted.Status != "applied" {
		t.Fatalf("accept: %d %+v", code, accepted)
	}

	var rb RollbackJSON
	if code := post(t, ts.URL+"/api/sessions/"+sid+"/refine/rollback", map[string]string{}, &rb); code != 200 || len(rb.RolledBack) != 1 {
		t.Fatalf("rollback: %d %+v", code, rb)
	}
}

func TestRefineRouteRefusesSubagentsAndMissingRefiner(t *testing.T) {
	s, ts := newTestServer(t)
	ctx := context.Background()
	parent, _ := s.Store().CreateSessionFrom(ctx, "p", "", store.SourceChat)
	if code := post(t, ts.URL+"/api/sessions/"+parent+"/refine", map[string]string{}, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("no refiner attached: %d, want 503", code)
	}
	attachRefiner(t, s)
	child, _ := s.Store().CreateChildSession(ctx, "c", "", parent)
	if code := post(t, ts.URL+"/api/sessions/"+child+"/refine", map[string]string{}, nil); code != http.StatusBadRequest {
		t.Fatalf("sub-agent: %d, want 400", code)
	}
}
