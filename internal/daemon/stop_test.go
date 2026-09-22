package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
)

func createTestSession(t *testing.T, url string) string {
	t.Helper()
	return decodeSession(t, postJSON(t, url+"/api/sessions", map[string]string{"title": "t"}), http.StatusCreated).ID
}

func waitIdle(t *testing.T, s *Server, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for s.hub.Running(id) {
		if time.Now().After(deadline) {
			t.Fatal("the turn slot was never released")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStopEndsTheTurnAndTheNextOneRuns(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	srv, ts := newTestServer(t,
		provider.ScriptTurn{Text: "partial", Hold: hold},
		provider.ScriptTurn{Text: "second"},
	)
	id := createTestSession(t, ts.URL)
	body := attachStream(t, ts, id)

	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
	res.Body.Close()
	evs := readSSE(t, body, 2)
	if evs[0].Type != WireTurnStarted || evs[1].Type != WireText {
		t.Fatalf("events = %+v, want turn_started then text", evs)
	}

	res = postJSON(t, ts.URL+"/api/sessions/"+id+"/stop", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202", res.StatusCode)
	}
	if ev := readSSE(t, body, 1)[0]; ev.Type != WireStopped {
		t.Fatalf("after stop got %+v, want stopped", ev)
	}
	waitIdle(t, srv, id)

	res = postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "again"})
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("second message status = %d, want 202", res.StatusCode)
	}
	evs = readSSE(t, body, 3)
	if evs[0].Type != WireTurnStarted || evs[1].Text != "second" || evs[2].Type != WireTurnDone {
		t.Fatalf("second turn events = %+v", evs)
	}

	got, err := http.Get(ts.URL + "/api/sessions/" + id)
	if err != nil {
		t.Fatal(err)
	}
	defer got.Body.Close()
	var tr TranscriptJSON
	if err := json.NewDecoder(got.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range tr.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "partial") && strings.Contains(b.Text, "[stopped by you]") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the partial reply was not persisted with the stop marker")
	}
}

func TestStopWithNothingRunningIsAConflict(t *testing.T) {
	_, ts := newTestServer(t)
	id := createTestSession(t, ts.URL)
	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/stop", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", res.StatusCode)
	}
	res = postJSON(t, ts.URL+"/api/sessions/nope/stop", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown session status = %d, want 404", res.StatusCode)
	}
}

// Shutdown cancels every turn through the server's base context. That is an
// error, never a stop.
func TestShutdownEndsATurnWithAnErrorNotAStop(t *testing.T) {
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	srv, ts := newTestServer(t, provider.ScriptTurn{Text: "partial", Hold: hold})
	id := createTestSession(t, ts.URL)
	body := attachStream(t, ts, id)
	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
	res.Body.Close()
	readSSE(t, body, 2) // turn_started, text

	srv.Close()
	if ev := readSSE(t, body, 1)[0]; ev.Type != WireError {
		t.Fatalf("after shutdown got %+v, want error", ev)
	}
}

func TestTurnDoneCarriesCacheTokens(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{
		Text:  "hi",
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 300, CacheWriteTokens: 40},
	})
	id := createTestSession(t, ts.URL)
	body := attachStream(t, ts, id)
	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
	res.Body.Close()
	done := readSSE(t, body, 3)[2]
	if done.Type != WireTurnDone || done.TokensCacheRead != 300 || done.TokensCacheWrite != 40 {
		t.Fatalf("turn_done = %+v, want cache read 300 and write 40", done)
	}
}

// A turn suspended on an approval must still stop: the approver returns when
// the turn's context is cancelled, the call is denied, and the tool never runs.
func TestStopWhileAnApprovalWaitsEndsTheTurnWithoutRunningTheTool(t *testing.T) {
	srv, ts, workspace := newFullServer(t,
		provider.ScriptTurn{ToolCalls: []provider.Block{{
			Type: provider.BlockToolUse, ID: "call-1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"out.txt","content":"never"}`),
		}}},
	)
	id, err := srv.Store().CreateSession(t.Context(), "approve", workspace)
	if err != nil {
		t.Fatal(err)
	}
	body := attachStream(t, ts, id)
	res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "write out.txt"})
	res.Body.Close()
	evs := readSSE(t, body, 3)
	var foundApproval bool
	for _, ev := range evs {
		if ev.Type == WireApproval {
			foundApproval = true
			break
		}
	}
	if !foundApproval {
		t.Fatalf("events = %+v, want an approval among turn_started/tool_call", evs)
	}

	res = postJSON(t, ts.URL+"/api/sessions/"+id+"/stop", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("stop status = %d, want 202", res.StatusCode)
	}
	stoppedSeen := false
	for i := 0; i < 5 && !stoppedSeen; i++ {
		stoppedSeen = readSSE(t, body, 1)[0].Type == WireStopped
	}
	if !stoppedSeen {
		t.Fatal("the suspended turn never reported stopped")
	}
	waitIdle(t, srv, id)
	if _, err := os.Stat(filepath.Join(workspace, "out.txt")); !os.IsNotExist(err) {
		t.Fatal("the tool ran although its approval was abandoned by the stop")
	}
}
