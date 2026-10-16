package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

func TestBrokerDeliversAnAnswerToTheWaitingAsk(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	type result struct {
		ans policy.Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := b.Ask(context.Background(), policy.Ask{
			SessionID: "s1", Tool: "shell_exec", PendingID: 7,
			Args: json.RawMessage(`{"cmd":"ls"}`), Rule: "shell_exec", Pattern: "",
		})
		done <- result{ans, err}
	}()

	// The ask reaches every attached client as an approval event.
	select {
	case ev := <-events:
		if ev.Type != WireApproval || ev.PendingID != 7 || ev.Tool != "shell_exec" {
			t.Fatalf("published %+v, want an approval for pending 7", ev)
		}
		if ev.Args != `{"cmd":"ls"}` || ev.Rule != "shell_exec" {
			t.Errorf("approval event lost its detail: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask published no approval event")
	}

	if !b.Answer("s1", 7, policy.Answer{Allow: true, Scope: policy.ScopeSession}) {
		t.Fatal("Answer reported no waiter, but Ask is waiting")
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Ask returned %v", got.err)
		}
		if !got.ans.Allow || got.ans.Scope != policy.ScopeSession {
			t.Errorf("Ask returned %+v, want the answer that was posted", got.ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after Answer")
	}
}

func TestBrokerReportsNoWaiterForAnUnknownPendingID(t *testing.T) {
	b := NewBroker(NewHub())
	// This is the case that must fall through to Guard.Resolve rather than
	// silently succeeding: nothing is waiting, so nothing was answered.
	if b.Answer("any", 99, policy.Answer{Allow: true}) {
		t.Error("Answer claimed to deliver to a waiter that does not exist")
	}
}

func TestBrokerOnlyOneAnswerWins(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	go b.Ask(context.Background(), policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 3})

	deadline := time.After(2 * time.Second)
	for !b.Answer("s1", 3, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		select {
		case <-deadline:
			t.Fatal("the waiter never registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	// A second client answering the same approval must be told it lost, so
	// the handler can report "already answered" instead of recording twice.
	if b.Answer("s1", 3, policy.Answer{Allow: false, Scope: policy.ScopeOnce}) {
		t.Error("a second Answer for the same pending id also won")
	}
}

func TestBrokerAskHonoursCancellation(t *testing.T) {
	b := NewBroker(NewHub())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(ctx, policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 5})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Ask returned no error after its context was cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask ignored cancellation; an unanswered approval would wait forever")
	}
	// The waiter must be gone, or the map grows without bound.
	if b.Answer("s1", 5, policy.Answer{Allow: true}) {
		t.Error("a cancelled Ask left its waiter registered")
	}
}

func TestResolveEndpointDeliversToTheWaiter(t *testing.T) {
	s, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "approve"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	done := make(chan policy.Answer, 1)
	go func() {
		ans, _ := s.Approver().Ask(context.Background(), policy.Ask{
			SessionID: sess.ID, Tool: "fs_write", PendingID: 42, Rule: "fs_write",
		})
		done <- ans
	}()
	waitForWaiter(t, s, 42)

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/42",
		map[string]any{"allow": true, "scope": "once"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("resolve status = %d, want 200", post.StatusCode)
	}
	select {
	case ans := <-done:
		if !ans.Allow {
			t.Error("the waiter received a denial")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting Ask never returned")
	}
}

func TestResolveEndpointRejectsABadScope(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "approve"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	post := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/1",
		map[string]any{"allow": true, "scope": "forever"})
	defer post.Body.Close()
	if post.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an unknown scope", post.StatusCode)
	}
}

func TestSecondResolvePostForAnsweredSuspensionReturns409(t *testing.T) {
	s, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "retry"})
	var sess SessionJSON
	json.NewDecoder(res.Body).Decode(&sess)
	res.Body.Close()

	done := make(chan policy.Answer, 1)
	go func() {
		ans, _ := s.Approver().Ask(context.Background(), policy.Ask{
			SessionID: sess.ID, Tool: "fs_write", PendingID: 100, Rule: "fs_write",
		})
		done <- ans
	}()
	waitForWaiter(t, s, 100)

	// First answer succeeds.
	first := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/100",
		map[string]any{"allow": true, "scope": "once"})
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first resolve status = %d, want 200", first.StatusCode)
	}

	// Second answer for the same approval must be rejected with 409,
	// not recorded via Guard.Resolve.
	second := postJSON(t, ts.URL+"/api/sessions/"+sess.ID+"/approvals/100",
		map[string]any{"allow": false, "scope": "once"})
	defer second.Body.Close()
	if second.StatusCode != http.StatusConflict {
		t.Fatalf("second resolve status = %d, want 409", second.StatusCode)
	}

	// The first answer should have reached the waiter.
	select {
	case ans := <-done:
		if !ans.Allow {
			t.Error("the waiter received the second answer (denial) instead of the first")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiting Ask never returned")
	}
}

func TestAnswerRejectsWrongSession(t *testing.T) {
	h := NewHub()
	b := NewBrokerWithTTL(h, 100*time.Millisecond)

	// Session s1 asks for approval.
	go b.Ask(context.Background(), policy.Ask{
		SessionID: "s1", Tool: "fs_write", PendingID: 200,
	})
	deadline := time.After(2 * time.Second)
	for {
		b.mu.Lock()
		_, ok := b.waiters[200]
		b.mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("waiter never registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	// Session s2 tries to answer. This must fail and leave s1's waiter intact.
	if b.Answer("s2", 200, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Error("Answer succeeded for wrong session")
	}

	// s1 can still answer with the correct session id.
	if !b.Answer("s1", 200, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Error("Answer failed for correct session after wrong session tried")
	}
}

func TestAnswerDeliveredConcurrentWithCancellationIsNotLost(t *testing.T) {
	// Invariant: if Answer(sessionID, pendingID, ans) returns true, then the
	// corresponding Ask must receive that answer (not a timeout), regardless
	// of timing or concurrent cancellation.
	for i := 0; i < 100; i++ {
		h := NewHub()
		b := NewBroker(h)
		pendingID := int64(300 + i)

		ctx, cancel := context.WithCancel(context.Background())
		type result struct {
			ans policy.Answer
			err error
		}
		done := make(chan result, 1)
		go func() {
			ans, err := b.Ask(ctx, policy.Ask{
				SessionID: "s1", Tool: "fs_write", PendingID: pendingID,
			})
			done <- result{ans, err}
		}()

		// Wait for the Ask to register.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			b.mu.Lock()
			_, ok := b.waiters[pendingID]
			b.mu.Unlock()
			if ok {
				break
			}
			time.Sleep(time.Microsecond)
		}

		// Race: cancel the context and answer at the same time from different
		// goroutines. The answer must not be lost.
		answerCh := make(chan bool, 1)
		go func() {
			answerCh <- b.Answer("s1", pendingID, policy.Answer{Allow: true, Scope: policy.ScopeOnce})
		}()
		cancel()

		delivered := <-answerCh
		res := <-done

		if delivered && res.err != nil {
			t.Errorf("iteration %d: Answer reported delivered but Ask returned error %v", i, res.err)
		}
		if delivered && !res.ans.Allow {
			t.Errorf("iteration %d: Answer reported delivered but Ask got denial", i)
		}
	}
}

// Fix 1: Broker.Ask publishes to RootID topic so child approvals reach the human
func TestBrokerAskPublishesToRootTopic(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)

	// Subscribe to root session
	rootEvents, stopRoot := h.Subscribe("root")
	defer stopRoot()
	// Also subscribe to child session
	childEvents, stopChild := h.Subscribe("child")
	defer stopChild()

	// Child's ask with RootID set to root
	go func() {
		_, _ = b.Ask(context.Background(), policy.Ask{
			SessionID: "child",
			Tool:      "shell_exec",
			PendingID: 1,
			Args:      json.RawMessage(`{"cmd":"ls"}`),
			Rule:      "*",
			RootID:    "root", // The root of the parent chain
		})
	}()

	// The approval should be published to the ROOT topic, not the child's
	select {
	case ev := <-rootEvents:
		if ev.Type != WireApproval || ev.PendingID != 1 {
			t.Fatalf("root received %+v, want an approval for pending 1", ev)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("approval not published to root topic")
	}

	// Child should NOT receive the approval event (it subscribes to child topic)
	select {
	case <-childEvents:
		t.Fatal("approval was published to child topic, should only be at root")
	case <-time.After(100 * time.Millisecond):
		// Expected - no event should arrive on child topic
	}
}

// Fix 2: Broker.Answer authorization checks RootID - child cannot answer its own, root can
func TestBrokerAnswerAuthorizesByRootID(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	_, stop := h.Subscribe("root")
	defer stop()

	// Create a waiter with child's session but root's ID in rootID field
	type result struct {
		ans policy.Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := b.Ask(context.Background(), policy.Ask{
			SessionID: "child",
			Tool:      "shell_exec",
			PendingID: 1,
			Args:      json.RawMessage(`{"cmd":"ls"}`),
			Rule:      "*",
			RootID:    "root", // Only root can answer this
		})
		done <- result{ans, err}
	}()

	// Give Ask time to register the waiter
	time.Sleep(50 * time.Millisecond)

	// Child tries to answer its own approval - should fail
	if b.Answer("child", 1, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Fatal("child was able to answer its own approval (should be blocked)")
	}

	// Root answers - should succeed
	if !b.Answer("root", 1, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Fatal("root could not answer the approval")
	}

	// Ask should have returned successfully
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Ask returned error: %v", res.err)
		}
		if !res.ans.Allow {
			t.Fatal("Ask got denial but should have gotten allow")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Ask did not return after Answer")
	}
}

// Fix 2 (top-level session case): Broker.Answer still works for top-level sessions
// where RootID equals SessionID
func TestBrokerAnswerStillWorksForTopLevelSessions(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	_, stop := h.Subscribe("toplevel")
	defer stop()

	type result struct {
		ans policy.Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := b.Ask(context.Background(), policy.Ask{
			SessionID: "toplevel",
			Tool:      "shell_exec",
			PendingID: 1,
			Args:      json.RawMessage(`{"cmd":"ls"}`),
			Rule:      "*",
			RootID:    "toplevel", // For top-level, root IS the session itself
		})
		done <- result{ans, err}
	}()

	// Give Ask time to register the waiter
	time.Sleep(50 * time.Millisecond)

	// Top-level session answers its own approval - should succeed
	if !b.Answer("toplevel", 1, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Fatal("top-level session could not answer its own approval (backward compat)")
	}

	// Ask should have returned successfully
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Ask returned error: %v", res.err)
		}
		if !res.ans.Allow {
			t.Fatal("Ask got denial but should have gotten allow")
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Ask did not return after Answer")
	}
}

// The web card shows who is asking under which profile and counts down to
// the auto-deny, so the live ask must carry both.
func TestApprovalEventCarriesProfileAndExpiry(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	deadline := time.Now().Add(5 * time.Minute).UTC().Truncate(time.Second)
	go b.Ask(context.Background(), policy.Ask{
		SessionID: "s1", Tool: "shell_exec", PendingID: 11,
		Profile: "remote", Deadline: deadline,
	})
	select {
	case ev := <-events:
		if ev.Profile != "remote" {
			t.Errorf("Profile = %q, want remote", ev.Profile)
		}
		got, err := time.Parse(time.RFC3339, ev.ExpiresAt)
		if err != nil || !got.Equal(deadline) {
			t.Errorf("ExpiresAt = %q (%v), want %s", ev.ExpiresAt, err, deadline.Format(time.RFC3339))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask published no approval event")
	}
	if d, ok := b.deadline(11); !ok || !d.Equal(deadline) {
		t.Errorf("broker deadline = %v, %v; want %v while the ask waits", d, ok, deadline)
	}
	b.Answer("s1", 11, policy.Answer{Allow: false})
}

// A suspension replayed after a restart has no waiter and so no timer. A
// countdown on it would be a lie.
func TestReplayWithoutWaiterHasNoExpiry(t *testing.T) {
	b := NewBroker(NewHub())
	ev := b.replayEvent(store.PendingCall{
		ID: 5, SessionID: "child", Tool: "fs_write", Profile: "local", Rule: "fs_write",
		ArgsJSON: []byte(`{"path":"a"}`),
	}, "root")
	if ev.ExpiresAt != "" {
		t.Errorf("ExpiresAt = %q, want empty with no live waiter", ev.ExpiresAt)
	}
	if ev.Profile != "local" || ev.Origin != "child" || ev.Type != WireApproval || ev.PendingID != 5 {
		t.Errorf("replayed %+v, want profile local, origin child, pending 5", ev)
	}
}

// The prompt a reconnecting client sees must offer the pattern the guard
// will learn: the one stored when the call was suspended, never one
// re-derived from the arguments against a filesystem that may have changed.
func TestReplayedApprovalOffersTheStoredPattern(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	ws, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := s.Store().CreateSession(ctx, "t", ws)
	if err != nil {
		t.Fatal(err)
	}
	const shown = "fs_write(path matches /ws/notes/**)"
	for _, p := range []store.PendingCall{
		{SessionID: sid, ToolUseID: "c1", Tool: "fs_write", Profile: "local", Rule: "fs_write", ArgsJSON: []byte(`{"path":"src/a.go"}`), Pattern: shown},
		{SessionID: sid, ToolUseID: "c2", Tool: "fs_write", Profile: "local", Rule: "fs_write", ArgsJSON: []byte(`{"path":"src/b.go"}`)},
	} {
		if _, err := s.Store().AddPendingCall(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	evs := s.allPendingApprovalEvents(ctx)
	if len(evs) != 2 {
		t.Fatalf("events = %+v, want two", evs)
	}
	if evs[0].Pattern != shown {
		t.Errorf("pattern = %q, want the stored %q", evs[0].Pattern, shown)
	}
	if evs[1].Pattern != "" {
		t.Errorf("a call stored with no pattern replayed %q; the option must stay hidden", evs[1].Pattern)
	}
}

// A timed-out ask tells the root's clients it expired, so the approval
// leaves their queue. The decision carries no allow or deny.
func TestAskTimeoutPublishesExpired(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := b.Ask(ctx, policy.Ask{SessionID: "s1", Tool: "shell_exec", PendingID: 11})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ask returned %v, want context.DeadlineExceeded", err)
	}
	assertExpiredResolved(t, events, 11, "shell_exec")
}

func TestAskCancelPublishesExpired(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(ctx, policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 12})
		done <- err
	}()
	// Wait for the ask to publish itself before cancelling it.
	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("Ask published no approval event")
	}
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask returned %v, want context.Canceled", err)
	}
	assertExpiredResolved(t, events, 12, "fs_write")
}

// An answered ask publishes exactly one resolved event, and it is the
// answer, never "expired".
func TestAnsweredAskPublishesOnce(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("s1")
	defer stop()

	done := make(chan error, 1)
	go func() {
		_, err := b.Ask(context.Background(), policy.Ask{SessionID: "s1", Tool: "fs_write", PendingID: 13})
		done <- err
	}()
	waitForBrokerWaiter(t, b, 13)
	if !b.Answer("s1", 13, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
		t.Fatal("Answer reported no waiter")
	}
	if err := <-done; err != nil {
		t.Fatalf("Ask returned %v", err)
	}

	var resolved []WireEvent
	for len(events) > 0 {
		if ev := <-events; ev.Type == WireResolved {
			resolved = append(resolved, ev)
		}
	}
	if len(resolved) != 1 {
		t.Fatalf("got %d resolved events, want exactly 1: %+v", len(resolved), resolved)
	}
	if resolved[0].Decision != "allow" {
		t.Errorf("resolved decision = %q, want \"allow\"", resolved[0].Decision)
	}
}

func assertExpiredResolved(t *testing.T, events <-chan WireEvent, pendingID int64, tool string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type != WireResolved {
				continue
			}
			want := WireEvent{Type: WireResolved, PendingID: pendingID, Tool: tool, Decision: DecisionExpired}
			if ev.PendingID != want.PendingID || ev.Tool != want.Tool || ev.Decision != want.Decision {
				t.Fatalf("resolved event = %+v, want %+v", ev, want)
			}
			return
		case <-deadline:
			t.Fatal("no resolved event was published for the expired ask")
		}
	}
}

func waitForBrokerWaiter(t *testing.T, b *Broker, id int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		_, ok := b.waiters[id]
		b.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no waiter registered for pending %d", id)
}

// A resolve for a suspension that is no longer pending is a conflict; a
// store failure is a server error, not a decision the client already made.
func TestResolveApprovalStatus(t *testing.T) {
	s, ts := newTestServer(t)
	engine, err := policy.NewEngine(config.Default().Policy)
	if err != nil {
		t.Fatal(err)
	}
	s.guard = policy.NewGuard(nil, engine, s.Approver(), s.Store())

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "status"})
	var sess SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&sess); err != nil {
		t.Fatalf("decode session: %v", err)
	}
	res.Body.Close()

	ctx := context.Background()
	pendingID, err := s.Store().AddPendingCall(ctx, store.PendingCall{
		SessionID: sess.ID, ToolUseID: "c1", Tool: "fs_write",
		ArgsJSON: []byte(`{"path":"x"}`), Profile: "local", Rule: "fs_write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store().ResolvePendingCall(ctx, pendingID, "allow"); err != nil {
		t.Fatal(err)
	}

	url := fmt.Sprintf("%s/api/sessions/%s/approvals/%d", ts.URL, sess.ID, pendingID)
	conflict := postJSON(t, url, map[string]any{"allow": true, "scope": "once"})
	defer conflict.Body.Close()
	if conflict.StatusCode != http.StatusConflict {
		t.Errorf("already-resolved status = %d, want 409", conflict.StatusCode)
	}

	// Breaking the approvals table makes the claim fail after the session and
	// the pending row are read, so the handler sees a store error, not a
	// missing suspension.
	pending2, err := s.Store().AddPendingCall(ctx, store.PendingCall{
		SessionID: sess.ID, ToolUseID: "c2", Tool: "fs_write",
		ArgsJSON: []byte(`{"path":"y"}`), Profile: "local", Rule: "fs_write",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store().DB().ExecContext(ctx, `DROP TABLE approvals`); err != nil {
		t.Fatal(err)
	}
	broken := postJSON(t, fmt.Sprintf("%s/api/sessions/%s/approvals/%d", ts.URL, sess.ID, pending2),
		map[string]any{"allow": true, "scope": "once"})
	defer broken.Body.Close()
	if broken.StatusCode != http.StatusInternalServerError {
		t.Errorf("store failure status = %d, want 500", broken.StatusCode)
	}
}

// A sub-agent may not answer an approval raised by its own session. The
// refusal is a 403, not a conflict and not a server error.
func TestSubAgentSelfApprovalIsForbidden(t *testing.T) {
	s, ts := newTestServer(t)
	engine, err := policy.NewEngine(config.Default().Policy)
	if err != nil {
		t.Fatal(err)
	}
	s.guard = policy.NewGuard(nil, engine, s.Approver(), s.Store())

	ctx := context.Background()
	parent, err := s.Store().CreateSession(ctx, "parent", "")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.Store().CreateChildSession(ctx, "child", "", parent)
	if err != nil {
		t.Fatal(err)
	}
	pendingID, err := s.Store().AddPendingCall(ctx, store.PendingCall{
		SessionID: child, ToolUseID: "c1", Tool: "fs_write",
		ArgsJSON: []byte(`{"path":"x"}`), Profile: "local", Rule: "fs_write",
	})
	if err != nil {
		t.Fatal(err)
	}

	res := postJSON(t, fmt.Sprintf("%s/api/sessions/%s/approvals/%d", ts.URL, child, pendingID),
		map[string]any{"allow": true, "scope": "once"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("sub-agent self-approval status = %d, want 403", res.StatusCode)
	}
}

func TestBrokerBlockedPublishes(t *testing.T) {
	h := NewHub()
	b := NewBroker(h)
	events, stop := h.Subscribe("root")
	defer stop()

	next := func() WireEvent {
		t.Helper()
		select {
		case ev := <-events:
			return ev
		case <-time.After(2 * time.Second):
			t.Fatal("Blocked published nothing")
			return WireEvent{}
		}
	}

	// A sub-agent's refusal names the agent as its origin.
	b.Blocked("root", "child", "shell_exec", "shell_exec(matches curl)", "")
	ev := next()
	if ev.Type != WireBlocked || ev.Tool != "shell_exec" || ev.Rule != "shell_exec(matches curl)" {
		t.Errorf("published %+v, want a blocked event for shell_exec", ev)
	}
	if ev.Origin != "child" || ev.Content != "" {
		t.Errorf("origin = %q content = %q, want child and no detail", ev.Origin, ev.Content)
	}

	// The root's own refusal has no origin, and its detail travels in Content.
	b.Blocked("root", "root", "mcp__srv__read", "mcp__*(any path outside workspace)", "path /etc/passwd is outside")
	ev = next()
	if ev.Origin != "" || ev.Content != "path /etc/passwd is outside" {
		t.Errorf("published %+v, want no origin and the detail in content", ev)
	}
}
