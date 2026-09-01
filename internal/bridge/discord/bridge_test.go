package discord

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

// waitFor polls cond every 10ms until it reports true or 5s elapses, failing
// the test on timeout. Tests in this package are driven by goroutines (the
// render loop, the bridge's own handlers), so a fixed sleep would either
// flake under load or make the suite needlessly slow; polling is both fast
// and reliable.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cond() {
		t.Fatal("condition not met within 5s")
	}
}

// startedTurn is one call the bridge made to Turns.StartTurn.
type startedTurn struct {
	sessionID, text, client string
	profile                 policy.Profile
}

// fakeTurns records what the bridge asked the daemon to run. It does not run
// anything: turn behaviour is the daemon's, tested elsewhere, and mixing the
// two would make these tests about the agent loop instead of the bridge.
type fakeTurns struct {
	mu      sync.Mutex
	starts  []startedTurn
	events  map[string]chan daemon.WireEvent
	started chan struct{}

	// nextError is returned by the next StartTurn, then cleared. Lets a test
	// exercise the "session already busy" path without a real hub.
	nextError error
}

func newFakeTurns() *fakeTurns {
	return &fakeTurns{
		events:  make(map[string]chan daemon.WireEvent),
		started: make(chan struct{}, 64),
	}
}

func (f *fakeTurns) StartTurn(sessionID, text, client string, profile policy.Profile) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nextError != nil {
		err := f.nextError
		f.nextError = nil
		return err
	}
	f.starts = append(f.starts, startedTurn{sessionID: sessionID, text: text, client: client, profile: profile})
	select {
	case f.started <- struct{}{}:
	default:
	}
	// Publish the turn's first event the way Hub.Publish does: to whoever is
	// subscribed right now. A bridge that starts the turn before subscribing
	// loses it, exactly as it would in production, which is what makes that
	// ordering regression visible to a test. Never create the channel here —
	// only Subscribe does that — so a StartTurn that races ahead of Subscribe
	// finds nobody listening and drops the event, same as the real hub.
	if ch, ok := f.events[sessionID]; ok {
		select {
		case ch <- daemon.WireEvent{Type: daemon.WireText, Text: "first"}:
		default:
		}
	}
	return nil
}

// Subscribe returns a per-session buffered channel a test can publish into,
// plus a no-op cancel: fakeTurns has no hub to detach from.
func (f *fakeTurns) Subscribe(sessionID string) (<-chan daemon.WireEvent, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch, ok := f.events[sessionID]
	if !ok {
		ch = make(chan daemon.WireEvent, 64)
		f.events[sessionID] = ch
	}
	return ch, func() {}
}

// waitForTurn blocks until a StartTurn call has been recorded, failing the
// test after 5s.
func (f *fakeTurns) waitForTurn(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("no turn started within 5s")
	}
}

// expectNoFurtherTurn asserts no StartTurn call arrives within d. Used to
// prove a dedupe or admission check actually suppressed work, not merely
// that the test raced ahead of it.
func (f *fakeTurns) expectNoFurtherTurn(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-f.started:
		t.Fatal("a turn started when none was expected")
	case <-time.After(d):
	}
}

func (f *fakeTurns) lastStart() startedTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.starts) == 0 {
		return startedTurn{}
	}
	return f.starts[len(f.starts)-1]
}

func (f *fakeTurns) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

// testDiscordConfig is the allowlist every bridge test is built on: one
// guild, one channel, one user, DMs open.
func testDiscordConfig() config.DiscordConfig {
	return config.DiscordConfig{
		Enabled: true, Token: "test-token",
		GuildID: "G", ChannelIDs: []string{"C1"}, UserIDs: []string{"U"},
		AllowDMs: true,
	}
}

// bridgeWithStore wires a bridge over the given client and store, with a
// fresh fakeTurns and broker. Every other constructor in this file is a thin
// wrapper over this one.
func bridgeWithStore(t *testing.T, f *fakeClient, st *store.Store) (*Bridge, *fakeTurns, *daemon.Broker) {
	t.Helper()
	turns := newFakeTurns()
	broker := daemon.NewBroker(daemon.NewHub())
	b, err := New(Options{
		Cfg: testDiscordConfig(), Client: f, Turns: turns, Store: st,
		Broker: broker, Guard: nil, // no tools run in these tests
		Throttle: -1, // flush on every event; never wait on a clock
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, turns, broker
}

// newBridgeOver builds a bridge on a caller-supplied client and a fresh
// store. Supervise's tests use it; they care about the connection, not the
// routing.
func newBridgeOver(t *testing.T, f *fakeClient) *Bridge {
	t.Helper()
	b, _, _ := bridgeWithStore(t, f, openTestStore(t))
	return b
}

// newTestBridge wires a bridge over the fake client and a fake Turns, with a
// REAL store so bindings and dedupe are exercised for real rather than
// against a map that cannot survive a restart.
func newTestBridge(t *testing.T) (*Bridge, *fakeClient, *fakeTurns, *store.Store) {
	t.Helper()
	st := openTestStore(t)
	f := newFakeClient()
	b, turns, _ := bridgeWithStore(t, f, st)
	return b, f, turns, st
}

// restartBridge builds a second bridge over the SAME store and a fresh
// client, which is what a daemon restart looks like from the store's side.
func restartBridge(t *testing.T, st *store.Store) (*Bridge, *fakeClient, *fakeTurns) {
	t.Helper()
	f := newFakeClient()
	b, turns, _ := bridgeWithStore(t, f, st)
	return b, f, turns
}

func TestAMessageInAChannelOpensAThreadAndASession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "what time is it?"})
	turns.waitForTurn(t)

	threads := f.allThreads()
	if len(threads) != 1 {
		t.Fatalf("created %d threads, want 1", len(threads))
	}
	// The thread's name comes from the prompt, so the channel reads as an
	// index of what you asked.
	if !strings.Contains(threads[0].Name, "what time is it") {
		t.Fatalf("thread name %q does not come from the prompt", threads[0].Name)
	}

	sid, found, err := st.SessionForExternal(context.Background(), bridgeName, threads[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("the thread was not bound to a session: (found=%v, err=%v)", found, err)
	}
	if got := turns.lastStart(); got.sessionID != sid || got.text != "what time is it?" {
		t.Fatalf("started %+v, want session %s with the prompt", got, sid)
	}
	// The bridge is the untrusted surface. This is the assertion that keeps
	// it that way.
	if got := turns.lastStart(); got.profile != policy.ProfileRemote {
		t.Fatalf("turn profile = %q, want %q", got.profile, policy.ProfileRemote)
	}
}

func TestAReplyInAThreadContinuesItsSession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "first"})
	turns.waitForTurn(t)
	thread := f.allThreads()[0].ThreadID
	want, _, _ := st.SessionForExternal(context.Background(), bridgeName, thread)

	f.deliver(Inbound{MessageID: "m2", UserID: "U", GuildID: "G", ChannelID: thread, ParentID: "C1", Content: "second"})
	turns.waitForTurn(t)

	if len(f.allThreads()) != 1 {
		t.Fatal("a reply in a thread created a second thread")
	}
	if got := turns.lastStart(); got.sessionID != want || got.text != "second" {
		t.Fatalf("reply started %+v, want session %s", got, want)
	}
}

func TestABindingSurvivesARestart(t *testing.T) {
	// The binding lives in SQLite, not in memory, so a thread you replied in
	// yesterday is still that session after the daemon restarts.
	b, f, turns, st := newTestBridge(t)
	b.Start(context.Background())
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "first"})
	turns.waitForTurn(t)
	thread := f.allThreads()[0].ThreadID
	want, _, _ := st.SessionForExternal(context.Background(), bridgeName, thread)
	b.Close()

	// A brand-new Bridge over the SAME store, as a restart would build.
	b2, f2, turns2 := restartBridge(t, st)
	defer b2.Close()
	b2.Start(context.Background())
	f2.deliver(Inbound{MessageID: "m9", UserID: "U", GuildID: "G", ChannelID: thread, ParentID: "C1", Content: "after restart"})
	turns2.waitForTurn(t)

	if got := turns2.lastStart(); got.sessionID != want {
		t.Fatalf("after restart the thread mapped to %q, want %q", got.sessionID, want)
	}
	if len(f2.allThreads()) != 0 {
		t.Fatal("the restarted bridge opened a new thread for an existing one")
	}
}

func TestRedeliveredMessagesRunOneTurn(t *testing.T) {
	// The gateway redelivers on resume. Running the prompt twice is worse
	// than dropping it: it can repeat a side effect the user approved once.
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	in := Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "do it"}
	f.deliver(in)
	turns.waitForTurn(t)
	f.deliver(in)

	turns.expectNoFurtherTurn(t, 200*time.Millisecond)
	if n := turns.startCount(); n != 1 {
		t.Fatalf("started %d turns for one message, want 1", n)
	}
}

func TestUnadmittedTrafficIsDroppedSilently(t *testing.T) {
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "m1", UserID: "STRANGER", GuildID: "G", ChannelID: "C1", Content: "hello?"})
	turns.expectNoFurtherTurn(t, 200*time.Millisecond)

	// Silence is the point. A reply — even "you are not allowed" — confirms
	// to whoever probed that the bot is live and listening.
	if len(f.sentTo("C1")) != 0 || len(f.allThreads()) != 0 || len(f.allResponds()) != 0 {
		t.Fatal("the bridge answered an unadmitted message")
	}
}

func TestAnUnadmittedButtonPressIsDropped(t *testing.T) {
	b, f, _, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())
	sid, _ := st.CreateSession(context.Background(), "s")

	f.press(Interaction{
		ID: "i1", Token: "tok", UserID: "STRANGER", GuildID: "G", ChannelID: "C1",
		CustomID: encodeCustomID(sid, 1, true, policy.ScopeOnce),
	})
	if len(f.allResponds()) != 0 {
		t.Fatal("the bridge responded to a stranger's button press")
	}
}

func TestSlashNewStartsAFreshDMSession(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "d1", UserID: "U", ChannelID: "DM1", Content: "first"})
	turns.waitForTurn(t)
	first, _, _ := st.SessionForExternal(context.Background(), bridgeName, "DM1")

	f.deliver(Inbound{MessageID: "d2", UserID: "U", ChannelID: "DM1", Content: "/new"})
	// /new starts no turn — it only rebinds — so wait on the binding.
	waitFor(t, func() bool {
		got, _, _ := st.SessionForExternal(context.Background(), bridgeName, "DM1")
		return got != first
	})

	f.deliver(Inbound{MessageID: "d3", UserID: "U", ChannelID: "DM1", Content: "second"})
	turns.waitForTurn(t)
	if got := turns.lastStart(); got.sessionID == first {
		t.Fatal("/new did not start a fresh session")
	}
	if turns.startCount() != 2 {
		t.Fatalf("started %d turns, want 2 (/new is not a prompt)", turns.startCount())
	}
}

func TestABusySessionTellsTheUser(t *testing.T) {
	// A second prompt while a turn is running is refused by the hub. Saying
	// nothing would look like the bot had died.
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())
	turns.nextError = daemon.ErrTurnRunning

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "go"})
	waitFor(t, func() bool { return len(f.allSent()) > 0 })

	var joined strings.Builder
	for _, m := range f.allSent() {
		joined.WriteString(m.Message.Content)
	}
	if !strings.Contains(strings.ToLower(joined.String()), "already") {
		t.Fatalf("the user was not told the session is busy: %q", joined.String())
	}
}

func TestTheTurnsFirstEventReachesDiscord(t *testing.T) {
	// Subscribe must precede StartTurn. If it does not, the hub publishes the
	// turn's opening events to nobody and they are lost — the user sees a
	// thread that sits empty until some later event happens to arrive.
	b, f, turns, _ := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "hello"})
	turns.waitForTurn(t)
	thread := f.allThreads()[0].ThreadID
	waitFor(t, func() bool {
		for _, c := range f.finalContents(thread) {
			if strings.Contains(c, "first") {
				return true
			}
		}
		return false
	})
}
