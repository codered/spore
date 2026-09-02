package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
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
	for {
		if b.Answer("s1", 3, policy.Answer{Allow: true, Scope: policy.ScopeOnce}) {
			break
		}
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
