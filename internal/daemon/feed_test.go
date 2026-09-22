package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// attachAll opens the global feed and returns once the daemon has accepted it.
func attachAll(t *testing.T, ts *httptest.Server) *bufio.Reader {
	t.Helper()
	res, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/events: %s", res.Status)
	}
	t.Cleanup(func() { res.Body.Close() })
	return bufio.NewReader(res.Body)
}

func TestGlobalFeedAnnouncesSessionsAndTagsTheirEvents(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{Text: "hi"})
	body := attachAll(t, ts)

	id := createTestSession(t, ts.URL)
	created := readSSE(t, body, 1)[0]
	if created.Type != WireSession || created.Session != id || created.Source != store.SourceChat || created.Workspace == "" {
		t.Fatalf("creation event = %+v, want a session event for %s from chat with a workspace", created, id)
	}

	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
	res.Body.Close()
	evs := readSSE(t, body, 3)
	for _, ev := range evs {
		if ev.Session != id {
			t.Fatalf("event %+v is not tagged with %s", ev, id)
		}
	}
	if evs[0].Type != WireTurnStarted || evs[2].Type != WireTurnDone {
		t.Fatalf("events = %+v, want turn_started … turn_done", evs)
	}
}

// A child's pending approval is replayed on connect, tagged with the root it
// is answered through and with the child as its origin.
func TestGlobalFeedReplaysPendingApprovalsThroughTheRoot(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	root, err := srv.CreateSession(ctx, "root", "", store.SourceChat, policy.ProfileLocal)
	if err != nil {
		t.Fatal(err)
	}
	child, err := srv.store.CreateChildSession(ctx, "child", "", root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.AddPendingCall(ctx, store.PendingCall{
		SessionID: child, ToolUseID: "t1", Tool: "shell", Profile: "local", Rule: "ask", ArgsJSON: []byte(`{"cmd":"ls"}`),
	}); err != nil {
		t.Fatal(err)
	}

	ev := readSSE(t, attachAll(t, ts), 1)[0]
	if ev.Type != WireApproval || ev.Session != root || ev.Origin != child || ev.Tool != "shell" {
		t.Fatalf("replayed approval = %+v, want session %s origin %s", ev, root, child)
	}
}

func TestSessionListReportsSourceStateAndPending(t *testing.T) {
	srv, ts := newTestServer(t)
	ctx := context.Background()
	root := createTestSession(t, ts.URL)
	child, err := srv.store.CreateChildSession(ctx, "child", "", root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.AddPendingCall(ctx, store.PendingCall{
		SessionID: child, ToolUseID: "t1", Tool: "shell", Profile: "local", Rule: "ask", ArgsJSON: []byte(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	list := func(q string) map[string]SessionJSON {
		res, err := http.Get(ts.URL + "/api/sessions" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out []SessionJSON
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		m := map[string]SessionJSON{}
		for _, s := range out {
			m[s.ID] = s
		}
		return m
	}

	all := list("?children=1")
	if r := all[root]; r.Source != store.SourceChat || r.State != SessionBlocked || r.Pending != 1 {
		t.Errorf("root = %+v, want chat, blocked, 1 pending", r)
	}
	if c := all[child]; c.Source != store.SourceSubagent || c.ParentID != root || c.State != SessionBlocked || c.Pending != 1 {
		t.Errorf("child = %+v, want subagent under root, blocked, 1 pending", c)
	}
	if _, ok := list("")[child]; ok {
		t.Error("the default listing includes a sub-agent session")
	}
}

func TestEverySessionCreationSiteRecordsItsSource(t *testing.T) {
	srv, ts := newTestServer(t, provider.ScriptTurn{Text: "done"})
	ctx := context.Background()

	chat := createTestSession(t, ts.URL)
	job, err := srv.StartJob(ctx, store.Job{Prompt: "nightly report"})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{chat: store.SourceChat, job: store.SourceJob} {
		sess, ok, err := srv.store.Session(ctx, id)
		if err != nil || !ok {
			t.Fatalf("Session(%s): ok=%v err=%v", id, ok, err)
		}
		if sess.Source != want {
			t.Errorf("Source(%s) = %q, want %q", id, sess.Source, want)
		}
	}
}
