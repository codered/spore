package httpstream

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// A body that stops sending fails with the stall, not a bare cancellation,
// and the request is cancelled.
func TestWatchStallFailsASilentBody(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	// Stands in for the transport, which fails the body when the request's
	// context ends.
	go func() { <-ctx.Done(); _ = pw.CloseWithError(ctx.Err()) }()

	body := WatchStall(pr, cancel, 50*time.Millisecond)
	defer body.Close()
	start := time.Now()
	_, err := body.Read(make([]byte, 8))
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("Read error = %v, want a stall", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("stall reported after %s, want about 50ms", d)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Error("the request was not cancelled")
	}
}

// Steady data keeps a stream alive well past the stall limit: the limit is on
// silence, not on the whole reply.
func TestWatchStallLetsASteadyBodyRunLong(t *testing.T) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-ctx.Done(); _ = pw.CloseWithError(ctx.Err()) }()
	go func() {
		for i := 0; i < 12; i++ { // 12 x 20ms = 240ms, nearly 5x the limit
			time.Sleep(20 * time.Millisecond)
			if _, err := pw.Write([]byte("x")); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	body := WatchStall(pr, cancel, 50*time.Millisecond)
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 12 {
		t.Errorf("read %d bytes, want 12", len(got))
	}
}
