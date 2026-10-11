package companion_test

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type fakeReflector struct {
	replies []string
	err     error
	inputs  []string
}

func (f *fakeReflector) Reflect(_ context.Context, _, input string) (string, error) {
	f.inputs = append(f.inputs, input)
	if f.err != nil {
		return "", f.err
	}
	if len(f.replies) == 0 {
		return `{"action":"none"}`, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	return r, nil
}

type fakeDeliverer struct {
	st   *store.Store
	sid  string
	sent []store.Outreach
	err  error
}

func (f *fakeDeliverer) EnsureSession(ctx context.Context) (string, error) {
	if f.sid == "" {
		id, err := f.st.CreateSessionFrom(ctx, "spore", "", store.SourceCompanion)
		if err != nil {
			return "", err
		}
		f.sid = id
		if err := f.st.SetCompanionState(ctx, companion.StateSessionID, id); err != nil {
			return "", err
		}
	}
	return f.sid, nil
}

func (f *fakeDeliverer) Deliver(ctx context.Context, o store.Outreach) (store.Outreach, error) {
	if f.err != nil {
		return store.Outreach{}, f.err
	}
	if _, err := f.EnsureSession(ctx); err != nil {
		return store.Outreach{}, err
	}
	o.Channel = store.OutreachChannelTerminal
	id, err := f.st.AddOutreach(ctx, o)
	if err != nil {
		return store.Outreach{}, err
	}
	o.ID = id
	f.sent = append(f.sent, o)
	return o, nil
}

type hbFixture struct {
	h          *companion.Heartbeat
	st         *store.Store
	cfg        *config.Config
	refl       *fakeReflector
	del        *fakeDeliverer
	now        time.Time
	selfWrites []string
}

// newHeartbeat is a heartbeat at 15:00 UTC on a Monday: outside the default
// 22:00-08:00 quiet hours, with the companion on and days counted in UTC.
func newHeartbeat(t *testing.T) *hbFixture {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Companion.Enabled = true
	cfg.Companion.Timezone = "UTC"
	st, err := store.Open(filepath.Join(cfg.DataDir, "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fx := &hbFixture{st: st, cfg: cfg, refl: &fakeReflector{}, del: &fakeDeliverer{st: st},
		now: time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)}
	fx.h = &companion.Heartbeat{
		Store: st, Cfg: cfg, Recorder: companion.NewRecorder(st, cfg),
		Reflector: fx.refl, Deliverer: fx.del,
		WriteSelf: func(_ context.Context, _, body string) error {
			fx.selfWrites = append(fx.selfWrites, body)
			return nil
		},
		Now: func() time.Time { return fx.now },
	}
	return fx
}

// candidate seeds an interest seen on the three days before now and sweeps
// it to candidate.
func (fx *hbFixture) candidate(t *testing.T, key string) store.Interest {
	t.Helper()
	ctx := context.Background()
	sid, _ := fx.st.CreateSessionFrom(ctx, "t", "", store.SourceChat)
	in, err := fx.st.TouchInterest(ctx, key, "label for "+key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		d := fx.now.AddDate(0, 0, -i)
		if err := fx.st.AddInterestSignal(ctx, in.ID, sid, "asked", d.Format("2006-01-02"), d); err != nil {
			t.Fatal(err)
		}
	}
	if err := fx.h.Recorder.Sweep(ctx, fx.now); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, key)
	if got.State != store.InterestCandidate {
		t.Fatalf("seeded %s is %s, want candidate", key, got.State)
	}
	return got
}

func (fx *hbFixture) outcome(t *testing.T, id int64) store.Outreach {
	t.Helper()
	o, ok, err := fx.st.OutreachByID(context.Background(), id)
	if err != nil || !ok {
		t.Fatalf("outreach %d: %v %v", id, ok, err)
	}
	return o
}

func (fx *hbFixture) budget(t *testing.T) float64 {
	t.Helper()
	b, err := companion.LoadBudget(context.Background(), fx.st, fx.cfg.Companion.StartBudget)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const proposeZS = `{"action":"propose","interest":"stock:zs","message":"You've checked ZS three days running. Want me to watch it?","watch":{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a daily move over 5%."}}`

func TestTickDisabledDoesNothing(t *testing.T) {
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.cfg.Companion.Enabled = false
	if res, err := fx.h.Tick(context.Background()); err != nil || res != "disabled" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatal("a disabled companion reflected")
	}
}

func TestTickQuietHoursWrapMidnight(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 10, 12, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 10, 13, 7, 30, 0, 0, time.UTC),
	} {
		fx := newHeartbeat(t)
		fx.candidate(t, "stock:zs")
		fx.now = at
		if res, err := fx.h.Tick(context.Background()); err != nil || res != "quiet hours" {
			t.Fatalf("tick at %s = %q %v", at.Format("15:04"), res, err)
		}
		if len(fx.refl.inputs) != 0 {
			t.Fatal("reflected during quiet hours")
		}
	}
}

func TestTickNothingEligibleMakesNoCall(t *testing.T) {
	fx := newHeartbeat(t)
	if res, err := fx.h.Tick(context.Background()); err != nil || res != "nothing to decide" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatal("reflected with nothing to decide")
	}
}

func TestTickProposes(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	res, err := fx.h.Tick(ctx)
	if err != nil || res != "propose stock:zs" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 {
		t.Fatalf("delivered %d, want 1", len(fx.del.sent))
	}
	o := fx.del.sent[0]
	if o.Kind != store.OutreachProposal || o.InterestID != in.ID || o.Payload != `{"cron":"0 14 * * 1-5","goal":"Check ZS; alert on a daily move over 5%."}` {
		t.Fatalf("outreach = %+v", o)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("interest = %s, want proposed", got.State)
	}
	// Nothing is left to decide: the proposal is pending, not unanswered.
	if res, _ := fx.h.Tick(ctx); res != "nothing to decide" {
		t.Fatalf("second tick = %q", res)
	}
	if len(fx.refl.inputs) != 1 {
		t.Fatalf("reflected %d times, want 1", len(fx.refl.inputs))
	}
}

func TestTickInvalidOutputWritesNothingAndCountsFailures(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	bad := `{"action":"propose","interest":"stock:nope","message":"hi","watch":{"cron":"0 14 * * *","goal":"g"}}`
	fx.refl.replies = []string{bad, bad, bad}
	for i := 1; i <= 3; i++ {
		if _, err := fx.h.Tick(ctx); err == nil {
			t.Fatalf("tick %d accepted invalid output", i)
		}
	}
	if len(fx.del.sent) != 0 {
		t.Fatal("invalid output was delivered")
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "3" {
		t.Fatalf("failures = %q, want 3", v)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate untouched", got.State)
	}
	fx.refl.replies = []string{`{"action":"none"}`}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "0" {
		t.Fatalf("failures after a success = %q, want 0", v)
	}
}

func TestTickCallErrorCountsAFailure(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.err = errors.New("provider down")
	if _, err := fx.h.Tick(ctx); err == nil {
		t.Fatal("a failed call must fail the tick")
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v != "1" {
		t.Fatalf("failures = %q, want 1", v)
	}
}

func TestTickBusyIsNotAFailure(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	fx.del.err = companion.ErrBusy
	res, err := fx.h.Tick(ctx)
	if err != nil || res != "companion session busy" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StateFailures); v == "1" {
		t.Fatal("a busy session counted as a failed reflection")
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate (nothing was sent)", got.State)
	}
}

func TestTickBudgetSpent(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.cfg.Companion.StartBudget = 1
	fx.candidate(t, "stock:zs")
	if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", SentAt: fx.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.Tick(ctx); err != nil || res != "budget spent" {
		t.Fatalf("tick = %q %v", res, err)
	}
}

func TestTickPaused(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	if err := companion.SetPausedUntil(ctx, fx.st, fx.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.Tick(ctx); err != nil || res != "paused" {
		t.Fatalf("tick = %q %v", res, err)
	}
}

// A pending message becomes unanswered at 12 h -- never ignored -- and stays
// unanswered while reflection leaves it alone.
func TestPendingBecomesUnansweredNotIgnored(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeUnanswered {
		t.Fatalf("outcome = %s, want unanswered", o.Outcome)
	}
	if len(fx.refl.inputs) != 1 {
		t.Fatal("an unanswered message must reach reflection")
	}
	fx.now = fx.now.Add(30 * time.Minute)
	if res, _ := fx.h.Tick(ctx); res != "nothing changed" {
		t.Fatalf("second tick = %q, want nothing changed", res)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeUnanswered {
		t.Fatalf("outcome = %s, want still unanswered", o.Outcome)
	}
	young, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "y", SentAt: fx.now.Add(-11 * time.Hour)})
	_, _ = fx.h.Tick(ctx)
	if o := fx.outcome(t, young); o.Outcome != store.OutcomePending {
		t.Fatalf("an 11 h message = %s, want pending", o.Outcome)
	}
}

func TestTickNudges(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	fx.refl.replies = []string{`{"action":"nudge","nudge_of":` + strconv.FormatInt(id, 10) + `,"message":"Still interested? A no is fine too."}`}
	if res, err := fx.h.Tick(ctx); err != nil || res != "nudge #"+strconv.FormatInt(id, 10) {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachNudge || fx.del.sent[0].NudgeOf != id {
		t.Fatalf("sent = %+v", fx.del.sent)
	}
}

// A nudge unanswered for a day ends both messages, steps the governor down
// once, and returns the proposal's interest to candidate with a cooldown.
func TestIgnoredNudgeEndsBothAndReturnsTheProposal(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	_, _ = fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed)
	orig, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-40 * time.Hour)})
	_, _ = fx.st.ResolveOutreach(ctx, orig, []string{store.OutcomePending}, store.OutcomeUnanswered, "", fx.now.Add(-28*time.Hour))
	nudge, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachNudge, Text: "y", NudgeOf: orig, SentAt: fx.now.Add(-25 * time.Hour)})
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, nudge); o.Outcome != store.OutcomeIgnored {
		t.Fatalf("nudge = %s, want ignored", o.Outcome)
	}
	if o := fx.outcome(t, orig); o.Outcome != store.OutcomeIgnored {
		t.Fatalf("original = %s, want ignored", o.Outcome)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestCandidate || !got.CooldownUntil.Equal(fx.now.Add(companion.ReturnCooldown)) {
		t.Fatalf("interest = %s until %v, want candidate cooling down 7d", got.State, got.CooldownUntil)
	}
	if b := fx.budget(t); b != 3.5 {
		t.Fatalf("budget = %v, want 3.5 (one step down)", b)
	}
}

func TestTickDropsWithAReason(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.candidate(t, "stock:zs")
	_, _ = fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed)
	id, _ := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-13 * time.Hour)})
	fx.refl.replies = []string{`{"action":"none","drops":[{"outreach":` + strconv.FormatInt(id, 10) + `,"reason":"the earnings moment passed"}]}`}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	o := fx.outcome(t, id)
	if o.Outcome != store.OutcomeIgnored || o.DropReason != "the earnings moment passed" {
		t.Fatalf("dropped = %+v", o)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("interest = %s, want candidate (a drop is not a no)", got.State)
	}
}

func TestTickRewritesSelf(t *testing.T) {
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	body := "## What I'm curious about\n\n- ZS earnings\n\n## Threads with you\n\n## How you like to be talked to\n\n## Opinions I've formed\n"
	fx.refl.replies = []string{`{"action":"none","self_md":` + quote(body) + `}`}
	if _, err := fx.h.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fx.selfWrites) != 1 || fx.selfWrites[0] != body {
		t.Fatalf("self writes = %q", fx.selfWrites)
	}
}

// observed seeds an interest seen once, today: still observing.
func (fx *hbFixture) observed(t *testing.T, key string) store.Interest {
	t.Helper()
	ctx := context.Background()
	sid, _ := fx.st.CreateSessionFrom(ctx, "t", "", store.SourceChat)
	in, err := fx.st.TouchInterest(ctx, key, "label for "+key)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.AddInterestSignal(ctx, in.ID, sid, "asked", fx.now.Format("2006-01-02"), fx.now); err != nil {
		t.Fatal(err)
	}
	return in
}

func TestCheckPromotesAnObservedInterest(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.observed(t, "stock:zs")
	if res, err := fx.h.TickWith(ctx, companion.TickOptions{Manual: true}); err != nil || res != "nothing to decide" {
		t.Fatalf("check without promote = %q %v", res, err)
	}
	fx.refl.replies = []string{proposeZS}
	res, err := fx.h.TickWith(ctx, companion.TickOptions{Manual: true, Promote: "stock:zs"})
	if err != nil || res != "propose stock:zs" {
		t.Fatalf("check = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 {
		t.Fatalf("delivered %d, want 1", len(fx.del.sent))
	}
	if strings.Contains(fx.refl.inputs[0], "verification run") {
		t.Fatal("an unforced check told the model to propose")
	}
}

func TestCheckForceTellsTheModelToPropose(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.observed(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	if _, err := fx.h.TickWith(ctx, companion.TickOptions{Manual: true, Promote: "stock:zs", Force: true}); err != nil {
		t.Fatal(err)
	}
	if len(fx.refl.inputs) != 1 || !strings.Contains(fx.refl.inputs[0], "verification run") ||
		!strings.Contains(fx.refl.inputs[0], `"propose"`) || !strings.Contains(fx.refl.inputs[0], "stock:zs") {
		t.Fatalf("input = %v", fx.refl.inputs)
	}
}

func TestCheckRejectsBadOptions(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in := fx.observed(t, "stock:zs")
	for name, o := range map[string]companion.TickOptions{
		"force without promote": {Manual: true, Force: true},
		"unknown key":           {Manual: true, Promote: "stock:nope"},
	} {
		if _, err := fx.h.TickWith(ctx, o); !errors.Is(err, companion.ErrBadCheck) {
			t.Errorf("%s: err = %v, want ErrBadCheck", name, err)
		}
	}
	_, _ = fx.st.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestDeclined)
	if _, err := fx.h.TickWith(ctx, companion.TickOptions{Manual: true, Promote: "stock:zs"}); err == nil ||
		!strings.Contains(err.Error(), "declined") {
		t.Fatalf("promoting a declined interest = %v", err)
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatal("a rejected check reflected")
	}
}

func TestCheckIgnoresPauseQuietAndUnchangedButNotBudget(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.now = time.Date(2026, 10, 12, 23, 30, 0, 0, time.UTC)
	if err := companion.SetPausedUntil(ctx, fx.st, fx.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	manual := companion.TickOptions{Manual: true}
	if res, err := fx.h.TickWith(ctx, manual); err != nil || res != "none" {
		t.Fatalf("first check = %q %v", res, err)
	}
	if res, err := fx.h.TickWith(ctx, manual); err != nil || res != "none" {
		t.Fatalf("second check (unchanged) = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 2 {
		t.Fatalf("reflected %d times, want 2", len(fx.refl.inputs))
	}
	fx.cfg.Companion.StartBudget = 1
	if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", SentAt: fx.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.TickWith(ctx, manual); err != nil || res != "budget spent" {
		t.Fatalf("check with no budget = %q %v", res, err)
	}
}

// blockingReflector holds every call until release closes, counting how
// many run at once.
type blockingReflector struct {
	mu      sync.Mutex
	running int
	peak    int
	entered chan struct{}
	release chan struct{}
}

func (b *blockingReflector) Reflect(context.Context, string, string) (string, error) {
	b.mu.Lock()
	b.running++
	b.peak = max(b.peak, b.running)
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.running--
	b.mu.Unlock()
	return `{"action":"none"}`, nil
}

func TestTicksDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	br := &blockingReflector{entered: make(chan struct{}, 2), release: make(chan struct{})}
	fx.h.Reflector = br
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fx.h.TickWith(ctx, companion.TickOptions{Manual: true})
		}()
	}
	<-br.entered
	select {
	case <-br.entered:
		t.Error("a second tick reflected while the first was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(br.release)
	wg.Wait()
	if br.peak != 1 {
		t.Fatalf("peak concurrent reflections = %d, want 1", br.peak)
	}
}
