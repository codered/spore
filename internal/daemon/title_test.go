package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type stubTitler struct {
	mu    sync.Mutex
	name  string
	err   error
	calls []string
}

func (s *stubTitler) Title(_ context.Context, msg string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, msg)
	return s.name, s.err
}

func (s *stubTitler) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func sendText(t *testing.T, ts *httptest.Server, id, text string) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"text": text})
	res, err := http.Post(ts.URL+"/api/sessions/"+id+"/messages", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusAccepted {
		t.Fatalf("post message status = %d", res.StatusCode)
	}
}

// titleOf polls until the session's title is want or the deadline passes,
// and returns what it last saw.
func titleOf(t *testing.T, s *Server, id, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sess, _, err := s.store.Session(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Title == want || time.Now().After(deadline) {
			return sess.Title
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPlaceholderSessionIsNamedAfterItsFirstTurn(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "sure"})
	tt := &stubTitler{name: "Weather in Fremont"}
	s.AttachTitler(tt)
	feed, stop := s.hub.SubscribeAll()
	defer stop()

	id, err := s.CreateSession(context.Background(), "chat", "", store.SourceChat, policy.ProfileLocal)
	if err != nil {
		t.Fatal(err)
	}
	sendText(t, ts, id, "what's the weather in fremont today?")
	if got := titleOf(t, s, id, "Weather in Fremont"); got != "Weather in Fremont" {
		t.Fatalf("title = %q, want the titler's name", got)
	}
	if calls := tt.seen(); len(calls) != 1 || calls[0] != "what's the weather in fremont today?" {
		t.Errorf("titler calls = %q, want one with the opening message", calls)
	}

	// Clients learn the name from the feed, with every field filled in:
	// a client replaces the session's details with the event's.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-feed:
			if ev.Type == WireSession && ev.Session == id && ev.Title == "Weather in Fremont" {
				if ev.Workspace == "" || ev.Source != store.SourceChat {
					t.Errorf("session event lost its details: %+v", ev)
				}
				return
			}
		case <-deadline:
			t.Fatal("no session event announced the name")
		}
	}
}

func TestNamingFallsBackToTheFirstLine(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "ok"})
	s.AttachTitler(&stubTitler{err: errors.New("model offline")})
	id, err := s.CreateSession(context.Background(), "", "", store.SourceDiscord, policy.ProfileRemote)
	if err != nil {
		t.Fatal(err)
	}
	sendText(t, ts, id, "plan a trip to tahoe\nwith the kids")
	if got := titleOf(t, s, id, "plan a trip to tahoe"); got != "plan a trip to tahoe" {
		t.Errorf("title = %q, want the first line", got)
	}
}

func TestNamingLeavesRealTitlesAndJobsAlone(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "a"}, provider.ScriptTurn{Text: "b"})
	tt := &stubTitler{name: "Should not appear"}
	s.AttachTitler(tt)
	ctx := context.Background()
	named, _ := s.CreateSession(ctx, "Fix login bug", "", store.SourceChat, policy.ProfileLocal)
	job, _ := s.CreateSession(ctx, "chat", "", store.SourceJob, policy.ProfileLocal)
	sendText(t, ts, named, "hello")
	waitIdle(t, s, named)
	sendText(t, ts, job, "hello")
	waitIdle(t, s, job)
	time.Sleep(200 * time.Millisecond)
	if calls := tt.seen(); len(calls) != 0 {
		t.Errorf("titler called %d times, want none", len(calls))
	}
	if got := titleOf(t, s, named, "Fix login bug"); got != "Fix login bug" {
		t.Errorf("a real title changed to %q", got)
	}
}
