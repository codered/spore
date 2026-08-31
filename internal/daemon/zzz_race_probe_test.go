package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
)

// Probe: can Answer() return true (claiming delivery) in the same window
// where Ask's ctx is cancelled, such that Ask takes the ctx.Done() branch
// and the delivered answer is never observed by anyone?
func TestZZZProbeLostAnswerOnCancelRace(t *testing.T) {
	lost := 0
	const iters = 20000
	for i := 0; i < iters; i++ {
		b := NewBroker(NewHub())
		ctx, cancel := context.WithCancel(context.Background())

		type result struct {
			ans policy.Answer
			err error
		}
		done := make(chan result, 1)
		go func() {
			ans, err := b.Ask(ctx, policy.Ask{SessionID: "s", Tool: "t", PendingID: int64(i)})
			done <- result{ans, err}
		}()

		// Busy-wait for the waiter to register, then fire Answer and cancel
		// as close to simultaneously as we can from two goroutines.
		for {
			b.mu.Lock()
			_, ok := b.waiters[int64(i)]
			b.mu.Unlock()
			if ok {
				break
			}
		}

		answerOK := make(chan bool, 1)
		go func() { answerOK <- b.Answer("s", int64(i), policy.Answer{Allow: true}) }()
		cancel()

		delivered := <-answerOK
		res := <-done

		if delivered && res.err != nil {
			lost++
		}
	}
	t.Logf("lost answers: %d / %d", lost, iters)
	if lost > 0 {
		t.Errorf("BUG CONFIRMED: Answer() reported delivered=true %d/%d times while Ask returned an error (answer lost, never reaches Guard.Run)", lost, iters)
	}
	_ = time.Second
}
