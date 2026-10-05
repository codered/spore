package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
)

// fakeOnceDaemon serves the four calls cmdOnce makes, and ends the turn with
// the given event once the message has been posted.
func fakeOnceDaemon(t *testing.T, end daemon.WireEvent) *httptest.Server {
	t.Helper()
	posted := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"id":"s1"}`)
	})
	mux.HandleFunc("POST /api/sessions/s1/messages", func(w http.ResponseWriter, r *http.Request) {
		close(posted)
		_, _ = fmt.Fprint(w, `{}`)
	})
	mux.HandleFunc("GET /api/sessions/s1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		select {
		case <-posted:
		case <-r.Context().Done():
			return
		}
		b, _ := json.Marshal(end)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestOnceExitStatusFollowsHowTheTurnEnded(t *testing.T) {
	cases := []struct {
		name string
		end  daemon.WireEvent
		want error
	}{
		{"finished", daemon.WireEvent{Type: daemon.WireTurnDone, Model: "m"}, nil},
		{"failed", daemon.WireEvent{Type: daemon.WireError, Error: "anthropic 401 Unauthorized"}, errTurnFailed},
		{"stopped", daemon.WireEvent{Type: daemon.WireStopped}, errTurnStopped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := fakeOnceDaemon(t, tc.end)
			cfg := &config.Config{DataDir: t.TempDir()}
			cfg.Daemon.Addr = strings.TrimPrefix(ts.URL, "http://")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := cmdOnce(ctx, cfg, "hi", t.TempDir())
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("cmdOnce = %v, want %v", err, tc.want)
			}
		})
	}
}

// main prints "spore: <err>" for an ordinary error. A failed or stopped turn
// has already been printed by printEvent, so it must not be printed again.
func TestTurnEndErrorsAreAlreadyReported(t *testing.T) {
	for _, err := range []error{errTurnFailed, errTurnStopped} {
		if !alreadyReported(err) {
			t.Errorf("alreadyReported(%v) = false, want true", err)
		}
	}
	if alreadyReported(errors.New("dial tcp: connection refused")) {
		t.Error("alreadyReported(ordinary error) = true, want false")
	}
}
