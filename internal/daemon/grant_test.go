package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func startSession(t *testing.T, url string) string {
	t.Helper()
	res := postJSON(t, url+"/api/sessions", map[string]string{"title": "grant"})
	defer res.Body.Close()
	var sess SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&sess); err != nil {
		t.Fatal(err)
	}
	return sess.ID
}

func decodeExpires(t *testing.T, body string) time.Time {
	t.Helper()
	var out struct {
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	at, err := time.Parse(time.RFC3339, out.ExpiresAt)
	if err != nil {
		t.Fatalf("expires_at %q is not RFC3339: %v", out.ExpiresAt, err)
	}
	return at
}

func nextGrantEvent(t *testing.T, events <-chan WireEvent) WireEvent {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Type == WireGrant {
				return ev
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no grant event published")
		}
	}
}

func TestGrantStart(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	root := startSession(t, ts.URL)
	events, stop := s.hub.Subscribe(root)
	defer stop()

	status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "15m"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	at := decodeExpires(t, body)
	if d := time.Until(at); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("expires_at is %v from now, want about 15m", d)
	}
	if _, ok := s.guard.GrantUntil(root); !ok {
		t.Error("no live grant after POST")
	}
	ev := nextGrantEvent(t, events)
	if ev.ExpiresAt != at.Format(time.RFC3339) {
		t.Errorf("grant event expires_at = %q, response said %q", ev.ExpiresAt, at.Format(time.RFC3339))
	}
}

func TestGrantRejectsDuration(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	root := startSession(t, ts.URL)
	for _, d := range []string{"2h", "60m"} {
		status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": d})
		if status != http.StatusBadRequest {
			t.Fatalf("duration %q: status = %d, want 400", d, status)
		}
		if !strings.Contains(body, "5m, 15m, 30m, 1h") {
			t.Errorf("duration %q: error does not list the allowed choices: %s", d, body)
		}
	}
	if _, ok := s.guard.GrantUntil(root); ok {
		t.Error("a refused duration started a grant")
	}
}

func TestGrantDisabled(t *testing.T) {
	s, ts := newTestServer(t)
	s.cfg.Policy.GrantDurations = nil
	attachPolicy(t, s)
	root := startSession(t, ts.URL)
	status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "15m"})
	if status != http.StatusBadRequest || !strings.Contains(body, "off") {
		t.Fatalf("status = %d body %s, want 400 saying grants are off", status, body)
	}
}

func TestGrantSweepsWaiting(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	ctx := context.Background()
	root := startSession(t, ts.URL)
	child, err := s.store.CreateChildSession(ctx, "child", "", root)
	if err != nil {
		t.Fatal(err)
	}

	rootRow, err := s.store.AddPendingCall(ctx, store.PendingCall{SessionID: root, Tool: "fs_write", ArgsJSON: []byte(`{}`), Profile: "local", Rule: "fs_write"})
	if err != nil {
		t.Fatal(err)
	}
	childRow, err := s.store.AddPendingCall(ctx, store.PendingCall{SessionID: child, Tool: "shell_exec", ArgsJSON: []byte(`{}`), Profile: "local", Rule: "shell_exec"})
	if err != nil {
		t.Fatal(err)
	}
	// A suspension whose turn is gone: a row with no live waiter.
	orphanRow, err := s.store.AddPendingCall(ctx, store.PendingCall{SessionID: child, Tool: "fs_write", ArgsJSON: []byte(`{}`), Profile: "local", Rule: "fs_write"})
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		ans policy.Answer
		err error
	}
	asks := make(chan result, 2)
	go func() {
		ans, err := s.Approver().Ask(ctx, policy.Ask{SessionID: root, RootID: root, Tool: "fs_write", PendingID: rootRow, Rule: "fs_write"})
		asks <- result{ans, err}
	}()
	go func() {
		ans, err := s.Approver().Ask(ctx, policy.Ask{SessionID: child, RootID: root, Tool: "shell_exec", PendingID: childRow, Rule: "shell_exec"})
		asks <- result{ans, err}
	}()
	waitForWaiter(t, s, rootRow)
	waitForWaiter(t, s, childRow)

	status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "5m"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, body %s", status, body)
	}
	for i := 0; i < 2; i++ {
		select {
		case r := <-asks:
			if r.err != nil || !r.ans.Allow {
				t.Errorf("waiting ask got %+v err %v, want allow", r.ans, r.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a waiting ask was not answered by the grant")
		}
	}
	state, err := s.store.PendingCallState(ctx, orphanRow)
	if err != nil {
		t.Fatal(err)
	}
	if state != "allow" {
		t.Errorf("orphan suspension state = %q, want allow", state)
	}
}

func TestGrantRejectsChild(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	ctx := context.Background()
	root := startSession(t, ts.URL)
	child, err := s.store.CreateChildSession(ctx, "child", "", root)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := send(t, "POST", ts.URL+"/api/sessions/"+child+"/grant", map[string]string{"duration": "15m"})
	if status != http.StatusBadRequest {
		t.Fatalf("POST to child: status = %d, want 400", status)
	}
	status, _ = send(t, "DELETE", ts.URL+"/api/sessions/"+child+"/grant", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("DELETE to child: status = %d, want 400", status)
	}
	if _, ok := s.guard.GrantUntil(child); ok {
		t.Error("a grant was started on the child")
	}
	if _, ok := s.guard.GrantUntil(root); ok {
		t.Error("a refused child request started a grant on the root")
	}
}

func TestGrantEnd(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	root := startSession(t, ts.URL)
	events, stop := s.hub.Subscribe(root)
	defer stop()

	if status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "15m"}); status != http.StatusOK {
		t.Fatalf("start: status = %d, body %s", status, body)
	}
	status, body := send(t, "DELETE", ts.URL+"/api/sessions/"+root+"/grant", nil)
	if status != http.StatusOK || !strings.Contains(body, `"status":"ended"`) {
		t.Fatalf("end: status = %d, body %s", status, body)
	}
	if _, ok := s.guard.GrantUntil(root); ok {
		t.Error("grant still live after DELETE")
	}
	nextGrantEvent(t, events) // the start
	if ev := nextGrantEvent(t, events); ev.ExpiresAt != "" {
		t.Errorf("end event expires_at = %q, want empty", ev.ExpiresAt)
	}
}

func TestGrantReplayedOnAttach(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	granted := startSession(t, ts.URL)
	plain := startSession(t, ts.URL)
	status, body := send(t, "POST", ts.URL+"/api/sessions/"+granted+"/grant", map[string]string{"duration": "15m"})
	if status != http.StatusOK {
		t.Fatalf("start: status = %d, body %s", status, body)
	}
	at := decodeExpires(t, body)

	// Per-session stream: the grant comes first, with its expiry.
	res, err := http.Get(ts.URL + "/api/sessions/" + granted + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	got := readSSE(t, bufio.NewReader(res.Body), 1)
	if got[0].Type != WireGrant || got[0].ExpiresAt != at.Format(time.RFC3339) {
		t.Errorf("session replay = %+v, want a grant expiring %v", got[0], at)
	}

	// Global feed: the grant is replayed tagged with its root.
	all, err := http.Get(ts.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer all.Body.Close()
	gotAll := readSSE(t, bufio.NewReader(all.Body), 1)
	if gotAll[0].Type != WireGrant || gotAll[0].Session != granted || gotAll[0].ExpiresAt != at.Format(time.RFC3339) {
		t.Errorf("global replay = %+v, want a grant on %s", gotAll[0], granted)
	}

	// No grant, no grant event: the first event is the one published now.
	plainRes, err := http.Get(ts.URL + "/api/sessions/" + plain + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer plainRes.Body.Close()
	s.hub.Publish(plain, WireEvent{Type: WireText, Text: "hello"})
	first := readSSE(t, bufio.NewReader(plainRes.Body), 1)
	if first[0].Type != WireText {
		t.Errorf("a session with no grant replayed %+v", first[0])
	}
}

func TestGrantDurationFromCurrentEngine(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	root := startSession(t, ts.URL)
	if status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "1h"}); status != http.StatusOK {
		t.Fatalf("start 1h: status = %d, body %s", status, body)
	}

	pc := s.cfg.Policy
	pc.GrantDurations = []string{"5m", "15m"}
	e, err := policy.NewEngine(pc)
	if err != nil {
		t.Fatal(err)
	}
	s.guard.SetEngine(e)

	if _, ok := s.guard.GrantUntil(root); !ok {
		t.Error("a config reload ended a live grant")
	}
	status, body := send(t, "POST", ts.URL+"/api/sessions/"+root+"/grant", map[string]string{"duration": "1h"})
	if status != http.StatusBadRequest {
		t.Fatalf("1h after reload: status = %d body %s, want 400", status, body)
	}
}

func TestGrantUnknownSessionIs404(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	status, _ := send(t, "POST", ts.URL+"/api/sessions/nope/grant", map[string]string{"duration": "15m"})
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", status)
	}
}

// A waiter that registers after the sweep's first Answer missed, and before the
// guard commits, reads its row as pending and publishes a card. The post-commit
// Answer in settleSwept must deliver the allow to it, once.
func TestGrantSweepAnswersWaiterRegisteredDuringResolve(t *testing.T) {
	s, ts := newTestServer(t)
	attachPolicy(t, s)
	ctx := context.Background()
	root := startSession(t, ts.URL)
	row, err := s.store.AddPendingCall(ctx, store.PendingCall{SessionID: root, Tool: "fs_write", ArgsJSON: []byte(`{}`), Profile: "local", Rule: "fs_write"})
	if err != nil {
		t.Fatal(err)
	}
	events, stop := s.hub.Subscribe(root)
	defer stop()

	type result struct {
		ans policy.Answer
		err error
	}
	asks := make(chan result, 1)
	start := time.Now()
	go func() {
		// The Settled read lands before the commit, so it reports pending.
		ans, err := s.Approver().Ask(ctx, policy.Ask{
			SessionID: root, RootID: root, Tool: "fs_write", PendingID: row, Rule: "fs_write",
			Deadline: start.Add(time.Minute),
			Settled:  func() (string, bool) { return "", false },
		})
		asks <- result{ans, err}
	}()
	waitForWaiter(t, s, row)
	// The card is published only after the waiter is registered and the read
	// was taken, so the commit below happens inside that window.
	awaitEvent(t, events, WireApproval, row)

	if err := s.settleSwept(ctx, root, row, policy.Answer{Allow: true, Scope: policy.ScopeOnce}); err != nil {
		t.Fatalf("settleSwept: %v", err)
	}
	select {
	case r := <-asks:
		if r.err != nil || !r.ans.Allow {
			t.Errorf("waiter got %+v err %v, want allow", r.ans, r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter registered during the resolve was never answered")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("answer took %v, want well before the one-minute deadline", elapsed)
	}
	// The waiter took the one delivery, so a further Answer finds nothing.
	if s.broker.Answer(root, row, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Error("a second Answer delivered to an approval already answered")
	}
	state, err := s.store.PendingCallState(ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if state != "allow" {
		t.Errorf("row state = %q, want allow", state)
	}
	// Two resolved events go out, one from the sweep and one from the waiter.
	// The TUI and web client must treat the second as a no-op.
	for i := 0; i < 2; i++ {
		awaitEvent(t, events, WireResolved, row)
	}
}

// awaitEvent reads until an event of typ for pendingID arrives.
func awaitEvent(t *testing.T, events <-chan WireEvent, typ string, pendingID int64) WireEvent {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Type == typ && ev.PendingID == pendingID {
				return ev
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s event for pending %d", typ, pendingID)
		}
	}
}
