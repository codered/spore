package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

// newTestServer wires a real store, a real agent and a scripted provider.
// Only the model is fake — the goal is to test the transport against the
// real core, not against a mock of it.
func newTestServer(t *testing.T, turns ...provider.ScriptTurn) (*Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := config.Default()
	cfg.DefaultModel = "script/fake"
	cfg.DataDir = t.TempDir()

	preg := provider.NewRegistry()
	preg.Register("script", provider.NewScript(turns...), provider.ProviderPrice{In: 1, Out: 2})
	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	a := agent.New(st, preg, rt, cfg, nil)

	s := New(Options{Agent: a, Store: st, Cfg: cfg})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return res
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", res.StatusCode)
	}
}

func TestCreateListAndShowSession(t *testing.T) {
	_, ts := newTestServer(t)

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "first"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", res.StatusCode)
	}
	var created SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" {
		t.Fatal("created session has no id")
	}

	listRes, err := http.Get(ts.URL + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer listRes.Body.Close()
	var list []SessionJSON
	if err := json.NewDecoder(listRes.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the one created session", list)
	}

	showRes, err := http.Get(ts.URL + "/api/sessions/" + created.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer showRes.Body.Close()
	var tr TranscriptJSON
	if err := json.NewDecoder(showRes.Body).Decode(&tr); err != nil {
		t.Fatalf("decode transcript: %v", err)
	}
	if tr.Session.ID != created.ID || len(tr.Messages) != 0 {
		t.Errorf("transcript = %+v, want the empty session", tr)
	}

	missing, err := http.Get(ts.URL + "/api/sessions/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", missing.StatusCode)
	}
}

// readSSE reads server-sent events off a live response until it has n of
// them or the deadline passes.
func readSSE(t *testing.T, body *bufio.Reader, n int) []WireEvent {
	t.Helper()
	var out []WireEvent
	for len(out) < n {
		line, err := body.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE after %d events: %v", len(out), err)
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev WireEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("decode SSE payload %q: %v", line, err)
		}
		out = append(out, ev)
	}
	return out
}

func TestPostMessageStreamsToTwoAttachedClients(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{
		Text:  "hello from the model",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
	})

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "stream"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Two clients attach BEFORE the turn starts.
	var readers []*bufio.Reader
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", ts.URL+"/api/sessions/"+sess.ID+"/events", nil)
		streamRes, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("attach %d: %v", i, err)
		}
		t.Cleanup(func() { streamRes.Body.Close() })
		if ct := streamRes.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("content type = %q, want text/event-stream", ct)
		}
		readers = append(readers, bufio.NewReader(streamRes.Body))
	}

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "hi"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusAccepted {
		t.Fatalf("post message status = %d, want 202", post.StatusCode)
	}

	for i, r := range readers {
		events := readSSE(t, r, 2)
		if events[0].Type != WireText || events[0].Text != "hello from the model" {
			t.Errorf("client %d first event = %+v", i, events[0])
		}
		if events[1].Type != WireTurnDone || events[1].Model != "script/fake" {
			t.Errorf("client %d second event = %+v", i, events[1])
		}
	}
}

func TestTurnSurvivesTheClientThatStartedItDisconnecting(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{Text: "persisted anyway"})

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "abandoned"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Post with a context that is cancelled the instant the response returns:
	// the turn's lifetime must belong to the daemon, not to this request.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST",
		ts.URL+"/api/sessions/"+sess.ID+"/messages", strings.NewReader(`{"text":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	post, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	post.Body.Close()
	cancel()

	// The reply must land in the transcript despite nobody listening.
	deadline := time.Now().Add(3 * time.Second)
	for {
		show, err := http.Get(ts.URL + "/api/sessions/" + sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		var tr TranscriptJSON
		json.NewDecoder(show.Body).Decode(&tr)
		show.Body.Close()
		// user message plus assistant reply
		if len(tr.Messages) >= 2 {
			if tr.Messages[1].Role != "assistant" {
				t.Fatalf("second message role = %q, want assistant", tr.Messages[1].Role)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn did not complete after the client disconnected; transcript has %d messages", len(tr.Messages))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSecondTurnIsRejectedWhileOneIsRunning(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "one"})
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "busy"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	// Claim the slot directly so the state is unambiguous.
	if !s.Hub().Begin(sess.ID) {
		t.Fatal("could not claim the turn slot")
	}
	defer s.Hub().End(sess.ID)

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "hi"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409 while a turn is running", post.StatusCode)
	}
}

func TestPostMessageRejectsEmptyTextAndUnknownSession(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "validate"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	empty := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/messages", map[string]string{"text": "  "})
	defer empty.Body.Close()
	if empty.StatusCode != http.StatusBadRequest {
		t.Errorf("empty text status = %d, want 400", empty.StatusCode)
	}

	unknown := postJSON(t, ts.URL+"/api/sessions/nope/messages", map[string]string{"text": "hi"})
	defer unknown.Body.Close()
	if unknown.StatusCode != http.StatusNotFound {
		t.Errorf("unknown session status = %d, want 404", unknown.StatusCode)
	}
}

// waitForWaiter blocks until the broker has registered a waiter for id, so a
// test never races the goroutine that calls Ask.
func waitForWaiter(t *testing.T, s *Server, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.broker.mu.Lock()
		_, ok := s.broker.waiters[id]
		s.broker.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no waiter registered for pending %d", id)
}
