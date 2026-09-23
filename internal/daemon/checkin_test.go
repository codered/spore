package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
)

// fakeNotifier records what the daemon asked a chat surface to do.
type fakeNotifier struct {
	mu        sync.Mutex
	delivered []string // "session: text"
	followed  []string
}

func (f *fakeNotifier) Deliver(_ context.Context, sessionID, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delivered = append(f.delivered, sessionID+": "+text)
}

func (f *fakeNotifier) Follow(sessionID string) func() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.followed = append(f.followed, sessionID)
	return func() {}
}

func (f *fakeNotifier) snapshot() (delivered, followed []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.delivered...), append([]string(nil), f.followed...)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func messages(t *testing.T, s *Server, sessionID string) []store.Message {
	t.Helper()
	msgs, err := s.store.Messages(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func text(m store.Message) string { return string(m.BlocksJSON) }

// checkInFixture is a server with a chat session and a job created from it.
func checkInFixture(t *testing.T, turns ...provider.ScriptTurn) (*Server, *fakeNotifier, string, store.Job) {
	t.Helper()
	s, _ := newTestServer(t, turns...)
	n := &fakeNotifier{}
	s.SetNotifier(n)
	ctx := context.Background()
	origin, err := s.CreateSession(ctx, "chat", "", store.SourceChat, policy.ProfileLocal)
	if err != nil {
		t.Fatal(err)
	}
	job, err := scheduler.CreateJob(ctx, s.store, "*/5 * * * *", "Send me a joke", origin, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return s, n, origin, job
}

// runJob fires the job once and waits for the run and everything it reports.
func runJob(t *testing.T, s *Server, job store.Job) string {
	t.Helper()
	sid, err := s.StartJob(context.Background(), job)
	if err != nil {
		t.Fatalf("StartJob: %v", err)
	}
	waitUntil(t, "the job run to end", func() bool { return !s.hub.Running(sid) })
	return sid
}

// startHeld starts a turn in the session and returns once it holds the
// scripted provider's first turn, so a job run cannot take that turn first.
func startHeld(t *testing.T, s *Server, sessionID string) {
	t.Helper()
	events, stop := s.hub.Subscribe(sessionID)
	defer stop()
	if err := s.StartTurn(sessionID, "a long question", "http", policy.ProfileLocal); err != nil {
		t.Fatal(err)
	}
	for ev := range events {
		if ev.Type == WireText {
			return
		}
	}
}

func jobRow(t *testing.T, s *Server, id int64) store.Job {
	t.Helper()
	j, ok, err := s.store.Job(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("Job(%d): %v %v", id, ok, err)
	}
	return j
}

func TestFirstSuccessfulRunChecksInOnceInTheOrigin(t *testing.T) {
	s, n, origin, job := checkInFixture(t,
		provider.ScriptTurn{Text: "My grandfather died leaving me his stress."},
		provider.ScriptTurn{Text: "Job 1 ran fine. Want each run posted here, only failures, or nothing?"},
		provider.ScriptTurn{Text: "another joke"},
	)
	runJob(t, s, job)
	waitUntil(t, "the check-in turn", func() bool { return len(messages(t, s, origin)) == 2 })
	waitUntil(t, "the origin to go idle", func() bool { return !s.hub.Running(origin) })

	msgs := messages(t, s, origin)
	if msgs[0].Role != "user" || !strings.Contains(text(msgs[0]), SchedulerTag) ||
		!strings.Contains(text(msgs[0]), "My grandfather died") || !strings.Contains(text(msgs[0]), "schedule_notify") {
		t.Errorf("check-in event = %s %s", msgs[0].Role, text(msgs[0]))
	}
	if msgs[1].Role != "assistant" || !strings.Contains(text(msgs[1]), "only failures") {
		t.Errorf("check-in reply = %s %s", msgs[1].Role, text(msgs[1]))
	}
	if !jobRow(t, s, job.ID).CheckedIn {
		t.Error("the job is not marked checked in")
	}
	if _, followed := n.snapshot(); len(followed) != 1 || followed[0] != origin {
		t.Errorf("Follow calls = %v; want one for the origin", followed)
	}

	// A second success, still undecided: nothing more in the chat.
	runJob(t, s, job)
	time.Sleep(50 * time.Millisecond)
	if got := len(messages(t, s, origin)); got != 2 {
		t.Errorf("a second run added %d messages to the origin; want none", got-2)
	}
	if delivered, _ := n.snapshot(); len(delivered) != 0 {
		t.Errorf("an undecided job delivered notes: %v", delivered)
	}
}

func TestFailedFirstRunDoesNotCheckIn(t *testing.T) {
	s, _, origin, job := checkInFixture(t, provider.ScriptTurn{Err: errors.New("provider timeout")})
	runJob(t, s, job)
	time.Sleep(50 * time.Millisecond)
	if got := len(messages(t, s, origin)); got != 0 {
		t.Errorf("a failed first run wrote %d messages to the origin", got)
	}
	if jobRow(t, s, job.ID).CheckedIn {
		t.Error("a failed run claimed the check-in")
	}
}

func TestJobWithoutOriginReportsNowhere(t *testing.T) {
	s, n, _, _ := checkInFixture(t, provider.ScriptTurn{Text: "ok"})
	job, err := scheduler.CreateJob(context.Background(), s.store, "*/5 * * * *", "no chat", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	runJob(t, s, job)
	time.Sleep(50 * time.Millisecond)
	if jobRow(t, s, job.ID).CheckedIn {
		t.Error("a job with no origin checked in")
	}
	if d, f := n.snapshot(); len(d)+len(f) != 0 {
		t.Errorf("a job with no origin reached the notifier: %v %v", d, f)
	}
}

// A check-in never interleaves with a turn already running in the origin:
// it waits for that turn to end, then runs.
func TestCheckInWaitsForTheOriginsTurn(t *testing.T) {
	hold := make(chan struct{})
	s, _, origin, job := checkInFixture(t,
		provider.ScriptTurn{Text: "thinking", Hold: hold},
		provider.ScriptTurn{Text: "the joke"},
		provider.ScriptTurn{Text: "it ran; how should I tell you?"},
	)
	startHeld(t, s, origin)
	runJob(t, s, job)
	time.Sleep(50 * time.Millisecond)
	for _, m := range messages(t, s, origin) {
		if strings.Contains(text(m), SchedulerTag) {
			t.Fatal("the check-in started while the origin's turn was running")
		}
	}
	if !jobRow(t, s, job.ID).CheckedIn {
		t.Error("the check-in was not claimed while it waited")
	}

	close(hold)
	waitUntil(t, "the check-in turn", func() bool { return len(messages(t, s, origin)) == 4 })
	waitUntil(t, "the origin to go idle", func() bool { return !s.hub.Running(origin) })
	msgs := messages(t, s, origin)
	roles := []string{msgs[0].Role, msgs[1].Role, msgs[2].Role, msgs[3].Role}
	if strings.Join(roles, ",") != "user,assistant,user,assistant" {
		t.Fatalf("origin roles = %v", roles)
	}
	if !strings.Contains(text(msgs[1]), "thinking") || !strings.Contains(text(msgs[2]), SchedulerTag) {
		t.Errorf("the held turn's reply must precede the check-in: %s / %s", text(msgs[1]), text(msgs[2]))
	}
}

func TestCheckInForADeletedOriginReleasesTheSlot(t *testing.T) {
	hold := make(chan struct{})
	s, _, origin, job := checkInFixture(t,
		provider.ScriptTurn{Text: "thinking", Hold: hold},
		provider.ScriptTurn{Text: "the joke"},
	)
	startHeld(t, s, origin)
	runJob(t, s, job)
	waitUntil(t, "the check-in to be queued", func() bool { return jobRow(t, s, job.ID).CheckedIn })
	// The row goes while the check-in waits; the turn still ends normally.
	if _, err := s.store.DeleteSessions(context.Background(), []string{origin}); err != nil {
		t.Fatal(err)
	}
	close(hold)
	waitUntil(t, "the slot to be released", func() bool { return !s.hub.Running(origin) })
}

// After the check-in, later runs follow the job's mode with a plain note:
// persisted as a note, published as job_note, and handed to the notifier.
func TestLaterRunsFollowTheNotifyMode(t *testing.T) {
	for _, tc := range []struct {
		mode        string
		ok, failed  bool // whether a note is expected for a good and a failed run
		failureText string
	}{
		{mode: store.NotifyEach, ok: true, failed: true},
		{mode: store.NotifyFailures, ok: false, failed: true},
		{mode: store.NotifyNone, ok: false, failed: false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s, n, origin, job := checkInFixture(t,
				provider.ScriptTurn{Text: "ok run"},
				provider.ScriptTurn{Err: errors.New("provider timeout")},
			)
			ctx := context.Background()
			if claimed, err := s.store.ClaimJobCheckIn(ctx, job.ID); err != nil || !claimed {
				t.Fatal("could not mark the job checked in")
			}
			if err := s.store.SetJobNotify(ctx, job.ID, tc.mode); err != nil {
				t.Fatal(err)
			}
			feed, stop := s.hub.SubscribeAll()
			defer stop()

			runJob(t, s, job) // ok
			runJob(t, s, job) // failed
			time.Sleep(50 * time.Millisecond)

			var notes []string
			for _, m := range messages(t, s, origin) {
				if m.Role != store.RoleNote {
					t.Errorf("a later run wrote a %s message into the origin", m.Role)
					continue
				}
				notes = append(notes, text(m))
			}
			var want int
			if tc.ok {
				want++
			}
			if tc.failed {
				want++
			}
			if len(notes) != want {
				t.Fatalf("notes = %v; want %d", notes, want)
			}
			if tc.ok && !strings.Contains(notes[0], "— ok") {
				t.Errorf("ok note = %s", notes[0])
			}
			if tc.failed && !strings.Contains(notes[len(notes)-1], "failed at") ||
				tc.failed && !strings.Contains(notes[len(notes)-1], "provider timeout") {
				t.Errorf("failure note = %s", notes[len(notes)-1])
			}
			for _, note := range notes {
				if strings.Contains(note, "ok run") {
					t.Errorf("a note carried the run's reply: %s", note)
				}
			}
			delivered, _ := n.snapshot()
			if len(delivered) != want {
				t.Errorf("notifier got %v; want %d notes", delivered, want)
			}
			for _, d := range delivered {
				if !strings.HasPrefix(d, origin+": ⏰ job") {
					t.Errorf("delivered %q; want a note for the origin", d)
				}
			}
			var published int
		drain:
			for {
				select {
				case ev := <-feed:
					if ev.Type == WireJobNote {
						published++
						if ev.Session != origin {
							t.Errorf("job_note published on %s, want the origin", ev.Session)
						}
					}
				default:
					break drain
				}
			}
			if published != want {
				t.Errorf("published %d job_note events; want %d", published, want)
			}
		})
	}
}

func TestHubEnqueueRunsNowWhenIdleAndAfterTheTurnOtherwise(t *testing.T) {
	h := NewHub()
	ran := make(chan string, 4)
	h.Enqueue("s", func() { ran <- "now"; h.End("s") })
	if got := <-ran; got != "now" {
		t.Fatal(got)
	}
	if h.Running("s") {
		t.Fatal("the slot stayed claimed after End")
	}

	if !h.Begin("s") {
		t.Fatal("Begin on an idle session failed")
	}
	h.Enqueue("s", func() { ran <- "first"; h.End("s") })
	h.Enqueue("s", func() { ran <- "second"; h.End("s") })
	select {
	case got := <-ran:
		t.Fatalf("%s ran while the turn was running", got)
	case <-time.After(20 * time.Millisecond):
	}
	// The slot passes straight to the queue: a client cannot claim it.
	h.End("s")
	if h.Begin("s") {
		t.Fatal("a client claimed the slot handed to queued work")
	}
	if got := <-ran; got != "first" {
		t.Fatalf("got %s, want first", got)
	}
	if got := <-ran; got != "second" {
		t.Fatalf("got %s, want second", got)
	}
	waitUntil(t, "the slot to be free", func() bool { return !h.Running("s") })
}

func TestCreateJobOverHTTPRecordsTheSession(t *testing.T) {
	s, ts := newTestServer(t)
	origin, err := s.CreateSession(context.Background(), "chat", "", store.SourceChat, policy.ProfileLocal)
	if err != nil {
		t.Fatal(err)
	}
	res := postJSON(t, ts.URL+"/api/jobs", map[string]string{"spec": "0 9 * * *", "prompt": "p", "session": origin})
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", res.StatusCode)
	}
	jobs, _ := s.store.ListJobs(context.Background())
	if len(jobs) != 1 || jobs[0].OriginSessionID != origin {
		t.Fatalf("jobs = %+v; want one with the origin", jobs)
	}
	res = postJSON(t, ts.URL+"/api/jobs", map[string]string{"spec": "0 9 * * *", "prompt": "p", "session": "nope"})
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown session gave %d, want 404", res.StatusCode)
	}
}

func TestProfileForFollowsTheSessionsSource(t *testing.T) {
	for src, want := range map[string]policy.Profile{
		store.SourceChat: policy.ProfileLocal, store.SourceJob: policy.ProfileLocal,
		store.SourceDiscord: policy.ProfileRemote, store.SourceUnknown: policy.ProfileRemote, "": policy.ProfileRemote,
	} {
		if got := profileFor(store.Session{Source: src}); got != want {
			t.Errorf("profileFor(%q) = %s, want %s", src, got, want)
		}
	}
}
