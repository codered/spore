package discord

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

func TestSuperviseRetriesAFailedOpen(t *testing.T) {
	f := newFakeClient()
	f.setFailNext("Open", errors.New("gateway refused"))
	b := newBridgeOver(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); Supervise(ctx, b, slog.Default()) }()

	// The first Open fails; failNext is one-shot, so the retry succeeds.
	waitFor(t, func() bool { return f.openCount() >= 2 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise did not return after its context was cancelled")
	}
}

func TestSuperviseStopsOnContextCancel(t *testing.T) {
	f := newFakeClient()
	b := newBridgeOver(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); Supervise(ctx, b, slog.Default()) }()
	waitFor(t, func() bool { return f.openCount() >= 1 })
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Supervise ignored cancellation")
	}
	if !f.closed() {
		t.Fatal("Supervise did not close the client on the way out")
	}
}
