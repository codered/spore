package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// A failed turn leaves a note in the transcript. The error event alone
// reaches only the clients watching at that moment; anyone who opens the
// session later saw it end in silence.
func TestFailedTurnLeavesANote(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Err: errors.New("llama-server error: No user query found in messages")})
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "chat"})
	sid := decodeSession(t, res, 201).ID

	events, unsub := s.hub.Subscribe(sid)
	defer unsub()
	res = postJSON(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, sid), map[string]string{"text": "review the code"})
	res.Body.Close()

	// The note is written before the error is published, so a client that
	// reloads the transcript on the error event finds it there.
	for ev := range events {
		if ev.Type != WireError {
			continue
		}
		msgs, err := s.store.Messages(context.Background(), sid)
		if err != nil {
			t.Fatal(err)
		}
		last := msgs[len(msgs)-1]
		if last.Role != store.RoleNote || !strings.Contains(string(last.BlocksJSON), "turn failed: llama-server error: No user query found") {
			t.Fatalf("last message when the error was published = %s %s; want the failure note", last.Role, last.BlocksJSON)
		}
		return
	}
	t.Fatal("event stream closed without an error event")
}

// A turn that succeeds leaves no note.
func TestSuccessfulTurnLeavesNoNote(t *testing.T) {
	_, ts := newTestServer(t, provider.ScriptTurn{Text: "done"})
	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "chat"})
	sid := decodeSession(t, res, 201).ID
	res = postJSON(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, sid), map[string]string{"text": "hi"})
	res.Body.Close()
	waitUntil(t, "the turn to end", func() bool {
		tr := transcript(t, ts.URL, sid)
		return len(tr.Messages) == 2 && !tr.Running
	})
	for _, m := range transcript(t, ts.URL, sid).Messages {
		if m.Role == store.RoleNote {
			t.Errorf("unexpected note: %+v", m)
		}
	}
}
