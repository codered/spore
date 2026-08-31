package daemon

import (
	"testing"
	"time"
)

func drain(t *testing.T, ch <-chan WireEvent, want int) []WireEvent {
	t.Helper()
	var got []WireEvent
	deadline := time.After(2 * time.Second)
	for len(got) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d of %d events", len(got), want)
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("timed out after %d of %d events", len(got), want)
		}
	}
	return got
}

func TestHubBroadcastsToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, closeA := h.Subscribe("s1")
	defer closeA()
	b, closeB := h.Subscribe("s1")
	defer closeB()
	other, closeOther := h.Subscribe("s2")
	defer closeOther()

	h.Publish("s1", WireEvent{Type: WireText, Text: "hello"})

	if got := drain(t, a, 1); got[0].Text != "hello" {
		t.Errorf("subscriber A got %+v", got[0])
	}
	if got := drain(t, b, 1); got[0].Text != "hello" {
		t.Errorf("subscriber B got %+v", got[0])
	}
	select {
	case ev := <-other:
		t.Errorf("session s2 received s1's event: %+v", ev)
	default:
	}
}

func TestHubUnsubscribeStopsDelivery(t *testing.T) {
	h := NewHub()
	ch, stop := h.Subscribe("s1")
	stop()
	// The channel is closed by stop, and a later publish must not panic on a
	// send to a closed channel — a disconnected browser tab must not be able
	// to kill the process.
	h.Publish("s1", WireEvent{Type: WireText, Text: "after"})
	if _, open := <-ch; open {
		t.Error("subscribing channel still delivered after unsubscribe")
	}
}

func TestHubDropsForASubscriberThatStoppedReading(t *testing.T) {
	h := NewHub()
	slow, stopSlow := h.Subscribe("s1")
	defer stopSlow()
	fast, stopFast := h.Subscribe("s1")
	defer stopFast()

	// More events than any buffer: a client that wandered off must not block
	// the turn or the other client.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			h.Publish("s1", WireEvent{Type: WireText, Text: "x"})
		}
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Publish blocked on a subscriber that is not reading")
	}
	_ = slow
	if got := drain(t, fast, 1); got[0].Text != "x" {
		t.Errorf("fast subscriber got %+v", got[0])
	}
}

func TestHubAllowsOneTurnPerSession(t *testing.T) {
	h := NewHub()
	if !h.Begin("s1") {
		t.Fatal("first Begin was refused")
	}
	if h.Begin("s1") {
		t.Error("second Begin succeeded; one session must run one turn at a time")
	}
	if !h.Begin("s2") {
		t.Error("a different session was blocked by s1's turn")
	}
	h.End("s1")
	if !h.Begin("s1") {
		t.Error("Begin refused after End")
	}
}
