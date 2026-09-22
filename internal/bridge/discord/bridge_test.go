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

// publish feeds one event to whoever is subscribed to sessionID, the way
// Hub.Publish does. Tests that need a turn to progress past its first event
// (tool calls, a turn that ends) drive it through here.
func (f *fakeTurns) publish(sessionID string, ev daemon.WireEvent) {
	f.mu.Lock()
	ch, ok := f.events[sessionID]
	f.mu.Unlock()
	if ok {
		ch <- ev
	}
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

// allStarts returns a copy of every StartTurn call recorded so far, in call
// order.
func (f *fakeTurns) allStarts() []startedTurn {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]startedTurn, len(f.starts))
	copy(out, f.starts)
	return out
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

// fakeSessions creates sessions directly against a real store, standing in
// for the daemon's CreateSession. These tests exercise the bridge's own
// routing (threads, bindings, dedupe), not workspace confinement -- that is
// covered where the ceiling and ProfileRemote actually matter, so this fake
// ignores both and just opens a row.
type fakeSessions struct{ store *store.Store }

func (f *fakeSessions) CreateSession(ctx context.Context, title, requested, source string, profile policy.Profile) (string, error) {
	return f.store.CreateSessionFrom(ctx, title, requested, source)
}

// bridgeWithStore wires a bridge over the given client and store, with a
// fresh fakeTurns and broker. Every other constructor in this file is a thin
// wrapper over this one.
func bridgeWithStore(t *testing.T, f *fakeClient, st *store.Store) (*Bridge, *fakeTurns, *daemon.Broker) {
	t.Helper()
	turns := newFakeTurns()
	broker := daemon.NewBroker(daemon.NewHub())
	b, err := New(Options{
		Cfg: testDiscordConfig(), Client: f, Turns: turns, Sessions: &fakeSessions{store: st}, Store: st,
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
	if sess, _, _ := st.Session(context.Background(), sid); sess.Source != store.SourceDiscord {
		t.Fatalf("thread session source = %q, want %q", sess.Source, store.SourceDiscord)
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
	sid, _ := st.CreateSession(context.Background(), "s", "")

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
	fresh, _, _ := st.SessionForExternal(context.Background(), bridgeName, "DM1")
	if sess, _, _ := st.Session(context.Background(), fresh); sess.Source != store.SourceDiscord {
		t.Fatalf("/new session source = %q, want %q", sess.Source, store.SourceDiscord)
	}
}

func TestSlashNewInAChannelDoesNotDisableThreadPerSession(t *testing.T) {
	// /new in a plain guild channel must not bind the channel: resolveSession
	// deliberately leaves the channel unbound so every top-level message
	// opens its own thread. If /new bound it anyway, the very next ordinary
	// message in the channel would silently resolve to the /new session
	// instead of opening a new thread.
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	b.Start(context.Background())

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "/new"})
	waitFor(t, func() bool { return len(f.sentTo("C1")) > 0 })
	if turns.startCount() != 0 {
		t.Fatalf("/new started %d turns, want 0", turns.startCount())
	}
	if _, found, _ := st.SessionForExternal(context.Background(), bridgeName, "C1"); found {
		t.Fatal("/new bound the guild channel itself; resolveSession must be free to open a thread for the next message")
	}

	f.deliver(Inbound{MessageID: "m2", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "ordinary message"})
	turns.waitForTurn(t)

	threads := f.allThreads()
	if len(threads) != 1 {
		t.Fatalf("created %d threads, want 1", len(threads))
	}
	threadSID, found, err := st.SessionForExternal(context.Background(), bridgeName, threads[0].ThreadID)
	if err != nil || !found {
		t.Fatalf("the thread was not bound to a session: (found=%v, err=%v)", found, err)
	}
	if got := turns.lastStart(); got.sessionID != threadSID || got.text != "ordinary message" {
		t.Fatalf("started %+v, want the thread's own new session", got)
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

func TestConcurrentFirstDMsResolveToOneSession(t *testing.T) {
	// A DM is one rolling session per admitted user (design spec, §8
	// Bridges) — the opposite of a guild channel, where every top-level
	// message gets its own thread. Two first DMs from the same user landing
	// on separate discordgo dispatch goroutines is exactly the race
	// resolveMu exists to prevent: without it, both can pass the "no
	// session yet" check before either writes a binding, and each creates
	// its own session, with the second BindExternal silently winning and
	// orphaning the first session from anything that will ever look it up
	// again.
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.deliver(Inbound{MessageID: "d1", UserID: "U", ChannelID: "DM1", Content: "one"})
	}()
	go func() {
		defer wg.Done()
		f.deliver(Inbound{MessageID: "d2", UserID: "U", ChannelID: "DM1", Content: "two"})
	}()
	wg.Wait()

	turns.waitForTurn(t)
	turns.waitForTurn(t)

	if n := len(f.allThreads()); n != 0 {
		t.Fatalf("a DM created %d threads, want 0 (DMs have no threads)", n)
	}
	sid, found, err := st.SessionForExternal(context.Background(), bridgeName, "DM1")
	if err != nil || !found {
		t.Fatalf("DM1 was not bound to a session: (found=%v, err=%v)", found, err)
	}
	if turns.startCount() != 2 {
		t.Fatalf("started %d turns for two DMs, want 2 (both still get a turn)", turns.startCount())
	}
	for _, s := range turns.allStarts() {
		if s.sessionID != sid {
			t.Fatalf("a turn started for session %q, want every turn on %q — both DMs must share the one rolling session", s.sessionID, sid)
		}
	}
}

func TestConcurrentChannelMessagesEachOpenTheirOwnThread(t *testing.T) {
	// The opposite of the DM case above, and the behaviour that must NOT
	// change: per the design spec, a top-level message in an admitted
	// channel always opens its own session and thread, so two of them
	// landing together are two conversations, not a race to collapse into
	// one. This pins that so a future "fix" doesn't quietly turn the
	// channel into a second rolling surface.
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		f.deliver(Inbound{MessageID: "a1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "one"})
	}()
	go func() {
		defer wg.Done()
		f.deliver(Inbound{MessageID: "a2", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "two"})
	}()
	wg.Wait()

	turns.waitForTurn(t)
	turns.waitForTurn(t)

	threads := f.allThreads()
	if len(threads) != 2 {
		t.Fatalf("created %d threads for two concurrent channel messages, want 2 (each opens its own)", len(threads))
	}
	sid1, found1, err1 := st.SessionForExternal(context.Background(), bridgeName, threads[0].ThreadID)
	sid2, found2, err2 := st.SessionForExternal(context.Background(), bridgeName, threads[1].ThreadID)
	if err1 != nil || !found1 || err2 != nil || !found2 {
		t.Fatalf("a thread was not bound to a session: (found1=%v err1=%v, found2=%v err2=%v)", found1, err1, found2, err2)
	}
	if sid1 == sid2 {
		t.Fatalf("both channel threads bound to the same session %q, want two distinct sessions", sid1)
	}
	if turns.startCount() != 2 {
		t.Fatalf("started %d turns for two messages, want 2", turns.startCount())
	}
}

func TestAnAddressPingIsNotTheThreadNameOrThePrompt(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// How a real channel message arrives: the ping that addresses the bot is
	// a raw mention token in the content, not the bot's name.
	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "<@1418> what time is it?"})
	turns.waitForTurn(t)

	threads := f.allThreads()
	if len(threads) != 1 {
		t.Fatalf("created %d threads, want 1", len(threads))
	}
	// A thread name is a channel name: Discord renders no mentions there, so
	// a token left in it shows up as a literal "<@1418>".
	if strings.Contains(threads[0].Name, "<@") {
		t.Fatalf("thread name %q still carries a raw mention", threads[0].Name)
	}
	if !strings.Contains(threads[0].Name, "what time is it") {
		t.Fatalf("thread name %q does not come from the prompt", threads[0].Name)
	}
	if got := turns.lastStart(); got.text != "what time is it?" {
		t.Fatalf("turn text = %q, want the prompt without the address ping", got.text)
	}
	if _, found, err := st.SessionForExternal(context.Background(), bridgeName, threads[0].ThreadID); err != nil || !found {
		t.Fatalf("the thread was not bound to a session: (found=%v, err=%v)", found, err)
	}
}

func TestThreadNameDropsMentionsAnywhereInTheLine(t *testing.T) {
	if got := threadName("<@1418> ask <@!99> about <@&7> the deploy"); strings.Contains(got, "<@") {
		t.Fatalf("threadName kept a raw mention: %q", got)
	}
	if got := threadName("<@1418>"); got != "spore session" {
		t.Fatalf("threadName(bare ping) = %q, want the fallback name", got)
	}
}

func TestAnInboundMessageGetsEyesThenACheckWhenTheTurnEnds(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "what time is it?"})
	turns.waitForTurn(t)

	// The eyes go on the message the user sent, in the channel they sent it
	// in — not in the thread, which they are not looking at yet.
	waitFor(t, func() bool { return len(f.allReacts()) >= 1 })
	first := f.allReacts()[0]
	if first.ChannelID != "C1" || first.MessageID != "m1" || first.Emoji != emojiEyes || first.Removed {
		t.Fatalf("first reaction = %+v, want the eyes added to C1/m1", first)
	}

	// Typing shows in the thread, where the answer is being written.
	thread := f.allThreads()[0].ThreadID
	waitFor(t, func() bool { return f.typingCount(thread) >= 1 })

	sid, _, err := st.SessionForExternal(context.Background(), bridgeName, thread)
	if err != nil {
		t.Fatal(err)
	}
	turns.publish(sid, daemon.WireEvent{Type: daemon.WireTurnDone})

	// Once the turn is answered the eyes are traded for a check: both calls
	// land, and the eyes are the one that is removed.
	waitFor(t, func() bool { return len(f.allReacts()) >= 3 })
	var addedDone, removedEyes bool
	for _, r := range f.allReacts() {
		if r.Emoji == emojiDone && !r.Removed && r.MessageID == "m1" {
			addedDone = true
		}
		if r.Emoji == emojiEyes && r.Removed && r.MessageID == "m1" {
			removedEyes = true
		}
	}
	if !addedDone || !removedEyes {
		t.Fatalf("reactions %+v, want the check added and the eyes removed", f.allReacts())
	}
}

func TestToolCallsCollapseToOneActivityMessageWithADetailsButton(t *testing.T) {
	b, f, turns, st := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "m1", UserID: "U", GuildID: "G", ChannelID: "C1", Content: "look around"})
	turns.waitForTurn(t)
	thread := f.allThreads()[0].ThreadID
	sid, _, err := st.SessionForExternal(context.Background(), bridgeName, thread)
	if err != nil {
		t.Fatal(err)
	}

	for _, ev := range []daemon.WireEvent{
		{Type: daemon.WireToolCall, ToolUseID: "t1", Tool: "fs_read", Args: `{"path":"go.mod"}`},
		{Type: daemon.WireToolResult, ToolUseID: "t1", Content: "module spore"},
		{Type: daemon.WireToolCall, ToolUseID: "t2", Tool: "shell_exec", Args: `{"cmd":"ls"}`},
		{Type: daemon.WireToolResult, ToolUseID: "t2", Content: "boom", IsError: true},
	} {
		turns.publish(sid, ev)
	}

	// One activity message for the whole turn, naming both tools, with the
	// failed one marked so a collapse never hides a failure.
	var activity sentMessage
	waitFor(t, func() bool {
		for _, m := range f.sentTo(thread) {
			if len(m.Message.Buttons) > 0 && strings.HasPrefix(m.Message.Buttons[0].CustomID, detailsPrefix) {
				activity = m
				return true
			}
		}
		return false
	})
	waitFor(t, func() bool {
		return strings.Contains(currentContentOf(f, thread, activity.MessageID), "shell_exec")
	})
	line := currentContentOf(f, thread, activity.MessageID)
	if !strings.Contains(line, "fs_read") || !strings.Contains(line, "shell_exec") {
		t.Fatalf("activity line %q does not name both tools", line)
	}
	if !strings.Contains(line, "⚠") {
		t.Fatalf("activity line %q does not mark the failed tool", line)
	}
	// The collapse is the point: no separate embed per call or per result.
	for _, m := range f.sentTo(thread) {
		if m.MessageID == activity.MessageID {
			continue
		}
		for _, e := range m.Message.Embeds {
			if strings.Contains(e.Title, "fs_read") || strings.Contains(e.Title, "shell_exec") {
				t.Fatalf("tool %q still got its own embed", e.Title)
			}
		}
	}

	// Pressing the button answers privately with the full transcript.
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G", ChannelID: thread, ParentID: "C1", CustomID: activity.Message.Buttons[0].CustomID})
	waitFor(t, func() bool { return len(f.allResponds()) >= 1 })
	detail := f.allResponds()[0].Content
	for _, want := range []string{"fs_read", "go.mod", "module spore", "shell_exec", "boom"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("details %q missing %q", detail, want)
		}
	}
}

func TestADetailsButtonFromBeforeARestartExpiresCleanly(t *testing.T) {
	st := openTestStore(t)
	f := newFakeClient()
	b, _, _ := bridgeWithStore(t, f, st)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// An id minted by a previous process: right shape, unknown run.
	f.press(Interaction{ID: "i1", Token: "tok", UserID: "U", GuildID: "G", ChannelID: "C1", CustomID: detailsPrefix + "deadbeef-7"})
	waitFor(t, func() bool { return len(f.allResponds()) >= 1 })
	if got := f.allResponds()[0].Content; !strings.Contains(got, "no longer") {
		t.Fatalf("expired details replied %q, want a clear expiry message", got)
	}
}

// currentContentOf returns what message id currently reads as: its latest
// edit if it has been edited, otherwise the content it was sent with.
func currentContentOf(f *fakeClient, channelID, messageID string) string {
	var out string
	for _, m := range f.allSent() {
		if m.MessageID == messageID {
			out = m.Message.Content
		}
	}
	for _, e := range f.editsTo(channelID) {
		if e.MessageID == messageID {
			out = e.Message.Content
		}
	}
	return out
}

func TestSlashNewSettlesItsOwnAcknowledgement(t *testing.T) {
	// /new never starts a turn, so the goroutine that normally trades the
	// eyes for a check never runs for it. Without its own settle the eyes
	// sit on the message forever, reading as "still working" on a command
	// that finished immediately.
	b, f, _, _ := newTestBridge(t)
	defer b.Close()
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.deliver(Inbound{MessageID: "d1", UserID: "U", ChannelID: "DM1", Content: "/new"})

	waitFor(t, func() bool {
		var done, eyesGone bool
		for _, r := range f.allReacts() {
			if r.MessageID != "d1" {
				continue
			}
			if r.Emoji == emojiDone && !r.Removed {
				done = true
			}
			if r.Emoji == emojiEyes && r.Removed {
				eyesGone = true
			}
		}
		return done && eyesGone
	})
}
