package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func transcript(t *testing.T, url, sessionID string) TranscriptJSON {
	t.Helper()
	res, err := http.Get(url + "/api/sessions/" + sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var tr TranscriptJSON
	if err := json.NewDecoder(res.Body).Decode(&tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

// The whole path on the real daemon: a job created from a chat, one tick,
// the check-in turn in that chat, the answer recorded through the real
// guard and the real schedule_notify, and a later failed run's note.
func TestEndToEndJobChecksInAndFollowsTheAnswer(t *testing.T) {
	srv, ts, _ := newFullServerWithPolicy(t, `[policy]
workspace = "%WORKSPACE%"
default = "deny"
allow = ["schedule_notify"]
`,
		provider.ScriptTurn{Text: "My grandfather died leaving me his stress."},           // the run
		provider.ScriptTurn{Text: "Job 1 ran fine. Each run, only failures, or nothing?"}, // the check-in
		provider.ScriptTurn{ToolCalls: []provider.Block{{ // the user's answer
			Type: provider.BlockToolUse, ID: "n1", Name: "schedule_notify", Input: json.RawMessage(`{"id":1,"mode":"failures"}`),
		}}},
		provider.ScriptTurn{Text: "Done: I'll only tell you when it fails."},
		provider.ScriptTurn{Err: errors.New("provider timeout")}, // a later, failed run
	)
	ctx := context.Background()

	res := postJSON(t, ts.URL+"/api/sessions", map[string]string{"title": "chat"})
	origin := decodeSession(t, res, http.StatusCreated).ID
	res = postJSON(t, ts.URL+"/api/jobs", map[string]string{"spec": "*/5 * * * *", "prompt": "Send me a joke", "session": origin})
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create job: %d", res.StatusCode)
	}

	clock := &testClock{now: time.Now().UTC().Add(10 * time.Minute)}
	sched := scheduler.New(srv.store, srv, clock.Now)
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the check-in in the origin", func() bool {
		tr := transcript(t, ts.URL, origin)
		return len(tr.Messages) == 2 && !tr.Running
	})
	tr := transcript(t, ts.URL, origin)
	if !strings.HasPrefix(tr.Messages[0].Blocks[0].Text, SchedulerTag) || !strings.Contains(tr.Messages[0].Blocks[0].Text, "grandfather") {
		t.Errorf("check-in event = %q", tr.Messages[0].Blocks[0].Text)
	}
	if tr.Messages[1].Role != "assistant" || !strings.Contains(tr.Messages[1].Blocks[0].Text, "only failures") {
		t.Errorf("check-in reply = %+v", tr.Messages[1])
	}

	res = postJSON(t, fmt.Sprintf("%s/api/sessions/%s/messages", ts.URL, origin), map[string]string{"text": "only failures please"})
	res.Body.Close()
	waitUntil(t, "the answer to be recorded", func() bool {
		j, _, _ := srv.store.Job(ctx, 1)
		return j.Notify == store.NotifyFailures && !srv.hub.Running(origin)
	})

	clock.Advance(10 * time.Minute)
	if err := sched.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the failure note", func() bool {
		tr := transcript(t, ts.URL, origin)
		last := tr.Messages[len(tr.Messages)-1]
		return last.Role == store.RoleNote && strings.Contains(last.Blocks[0].Text, "failed at")
	})
}
