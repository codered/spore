package companion_test

import (
	"context"
	"errors"
	"fmt"
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
	// A fresh store is due its digest on every tick. Mark today's as sent so
	// the tests from before the digest stay outside its slot.
	if err := st.SetCompanionState(context.Background(), companion.StateLastDigestDay, fx.now.In(time.UTC).Format("2006-01-02")); err != nil {
		t.Fatal(err)
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
	if err != nil || res != "companion session busy (decision saved)" {
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

// watchRun finishes one run of job with reply text, as the daemon does when
// the run ends. ok=false is a run that failed.
func (fx *hbFixture) watchRun(t *testing.T, job store.Job, reply string, ok bool) {
	t.Helper()
	sid := fx.runSession(t)
	if reply != "" {
		fx.reply(t, sid, reply)
	}
	if handled, _ := fx.h.RunFinished(context.Background(), job, sid, ok); !handled {
		t.Fatal("RunFinished did not handle the watch run")
	}
}

// digestDue puts the clock at 08:30 UTC with yesterday's digest as the last
// one sent: the morning digest slot is open.
func (fx *hbFixture) digestDue(t *testing.T) {
	t.Helper()
	fx.now = time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
	if err := fx.st.SetCompanionState(context.Background(), companion.StateLastDigestDay, "2026-10-11"); err != nil {
		t.Fatal(err)
	}
}

func (fx *hbFixture) lastDigestDay(t *testing.T) string {
	t.Helper()
	v, _, err := fx.st.CompanionState(context.Background(), companion.StateLastDigestDay)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// lastInput is the reflection input of the most recent call.
func (fx *hbFixture) lastInput(t *testing.T) string {
	t.Helper()
	if len(fx.refl.inputs) == 0 {
		t.Fatal("no reflection call was made")
	}
	return fx.refl.inputs[len(fx.refl.inputs)-1]
}

func (fx *hbFixture) runByID(t *testing.T, id int64) store.CompanionRun {
	t.Helper()
	for _, r := range fx.watchRuns(t) {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("run %d not found", id)
	return store.CompanionRun{}
}

func TestTickResearchCreatesAJobAndRow(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, job := fx.activeWatch(t, "stock:zs")
	fx.watchRun(t, job, "NOTHING: flat", true) // an unseen run makes the tick eligible
	fx.refl.replies = []string{`{"action":"research","interest":"stock:zs","research":{"question":"  what moved ZS?  "}}`}
	if res, err := fx.h.Tick(ctx); err != nil || res != "research stock:zs" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 0 {
		t.Fatalf("research sent %+v, want nothing", fx.del.sent)
	}
	var row store.CompanionRun
	for _, r := range fx.watchRuns(t) {
		if r.Kind == store.RunResearch {
			row = r
		}
	}
	if row.ID == 0 || row.Verdict != store.VerdictPending || row.Question != "what moved ZS?" || row.InterestID != in.ID {
		t.Fatalf("research row = %+v", row)
	}
	j, ok, err := fx.st.Job(ctx, row.JobID)
	if err != nil || !ok {
		t.Fatalf("job %d: %v %v", row.JobID, ok, err)
	}
	if j.Kind != "once" || j.Profile != config.CompanionProfile || j.OriginSessionID != "" ||
		j.Notify != store.NotifyNone || !j.CheckedIn || !j.Enabled {
		t.Fatalf("research job = %+v", j)
	}
	if j.Prompt != companion.ResearchPrompt(in.Label, "what moved ZS?") {
		t.Fatalf("research prompt = %q", j.Prompt)
	}
}

func TestStuckResearchFailsAndLateFinishIsIgnored(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, _ := fx.activeWatch(t, "stock:zs")
	job := fx.plainJob(t, "research the thing")
	id, err := fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, JobID: job.ID, Kind: store.RunResearch,
		Question: "q", Verdict: store.VerdictPending, StartedAt: fx.now.Add(-3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	sid := fx.runSession(t)
	fx.reply(t, sid, "a late answer")
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if r := fx.runByID(t, id); r.Verdict != store.VerdictFailed {
		t.Fatalf("stuck run = %s, want failed", r.Verdict)
	}
	if handled, failNote := fx.h.RunFinished(ctx, job, sid, true); !handled || failNote {
		t.Fatalf("late RunFinished = %v %v, want handled, no fail note", handled, failNote)
	}
	if r := fx.runByID(t, id); r.Verdict != store.VerdictFailed {
		t.Fatalf("late finish overwrote the stuck run: %s", r.Verdict)
	}
}

func TestTickFindingMarksRunsReported(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, job := fx.activeWatch(t, "stock:zs")
	fx.watchRun(t, job, "NOTHING: flat", true)
	runID := fx.watchRuns(t)[0].ID
	fx.refl.replies = []string{`{"action":"finding","interest":"stock:zs","message":"ZS moved.","runs":[` +
		strconv.FormatInt(runID, 10) + `,` + strconv.FormatInt(runID, 10) + `]}`}
	if res, err := fx.h.Tick(ctx); err != nil || res != "finding" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachFinding || fx.del.sent[0].InterestID != in.ID {
		t.Fatalf("sent = %+v, want one finding about the interest", fx.del.sent)
	}
	if got := fx.runByID(t, runID).ReportedOutreachID; got != fx.del.sent[0].ID {
		t.Fatalf("run reported by %d, want %d", got, fx.del.sent[0].ID)
	}
	if n, err := companion.SpentToday(ctx, fx.st, fx.now, time.UTC); err != nil || n != 1 {
		t.Fatalf("spent today = %d %v, want 1", n, err)
	}
}

// Follow-ups and findings spend the budget, so a follow-up is refused when
// none is left. The gate stops the call before reflection is asked.
func TestFollowupCountsAgainstTheBudget(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	_, job := fx.activeWatch(t, "stock:zs")
	for range 3 {
		fx.watchRun(t, job, "", false)
	}
	fx.cfg.Companion.StartBudget = 1
	if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", SentAt: fx.now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if res, err := fx.h.Tick(ctx); err != nil || res != "budget spent" {
		t.Fatalf("tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 0 || len(fx.del.sent) != 0 {
		t.Fatalf("spent budget reflected %d times and sent %d", len(fx.refl.inputs), len(fx.del.sent))
	}
}

func TestDigest(t *testing.T) {
	ctx := context.Background()

	t.Run("slot due at 08:30 allows only the digest", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		fx.refl.replies = []string{`{"action":"none"}`, `{"action":"digest","message":"Morning."}`}
		if _, err := fx.h.Tick(ctx); err == nil {
			t.Fatal("a none reply in the digest slot was accepted")
		}
		if fx.lastDigestDay(t) != "2026-10-11" || len(fx.del.sent) != 0 {
			t.Fatal("a failed digest moved the slot or sent something")
		}
		if res, err := fx.h.Tick(ctx); err != nil || res != "digest" {
			t.Fatalf("tick = %q %v", res, err)
		}
		if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachDigest {
			t.Fatalf("sent = %+v, want one digest", fx.del.sent)
		}
		if fx.lastDigestDay(t) != "2026-10-12" {
			t.Fatalf("last_digest_day = %q, want today", fx.lastDigestDay(t))
		}
	})

	t.Run("sent with the budget spent", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		fx.cfg.Companion.StartBudget = 1
		if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", SentAt: fx.now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		fx.refl.replies = []string{`{"action":"digest","message":"Morning."}`}
		if res, err := fx.h.Tick(ctx); err != nil || res != "digest" {
			t.Fatalf("tick = %q %v", res, err)
		}
		if len(fx.del.sent) != 1 {
			t.Fatalf("sent %d, want the digest", len(fx.del.sent))
		}
	})

	t.Run("last_digest_day is written only after delivery", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		fx.del.err = errors.New("channel down")
		fx.refl.replies = []string{`{"action":"digest","message":"Morning."}`}
		if _, err := fx.h.Tick(ctx); err == nil {
			t.Fatal("a failed delivery must fail the tick")
		}
		if fx.lastDigestDay(t) != "2026-10-11" {
			t.Fatalf("last_digest_day = %q after a failed send, want unchanged", fx.lastDigestDay(t))
		}
	})

	t.Run("none while paused", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		if err := companion.SetPausedUntil(ctx, fx.st, fx.now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		if res, err := fx.h.Tick(ctx); err != nil || res != "paused" {
			t.Fatalf("tick = %q %v", res, err)
		}
		if len(fx.refl.inputs) != 0 {
			t.Fatal("a paused companion reflected on the digest")
		}
	})

	t.Run("the previous digest is ignored with the reason and no budget step", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		fx.refl.replies = []string{`{"action":"digest","message":"First."}`, `{"action":"digest","message":"Second."}`}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fx.del.sent) != 1 {
			t.Fatalf("first digest: sent %d, want 1", len(fx.del.sent))
		}
		first := fx.del.sent[0].ID
		before := fx.budget(t)
		fx.now = time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if o := fx.outcome(t, first); o.Outcome != store.OutcomeIgnored || o.DropReason != "superseded by the next digest" {
			t.Fatalf("previous digest = %s %q", o.Outcome, o.DropReason)
		}
		if after := fx.budget(t); after != before {
			t.Fatalf("budget %v -> %v, want unchanged", before, after)
		}
	})

	t.Run("nudge_of folds into two nudges that count as nudged", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		a, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "a", SentAt: fx.now.Add(-13 * time.Hour)})
		b, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "b", SentAt: fx.now.Add(-13 * time.Hour)})
		fx.refl.replies = []string{`{"action":"digest","message":"Morning.","nudge_of":[` +
			strconv.FormatInt(a, 10) + `,` + strconv.FormatInt(b, 10) + `,` + strconv.FormatInt(a, 10) + `]}`}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{a, b} {
			n, ok, err := fx.st.NudgeFor(ctx, id)
			if err != nil || !ok || n.Kind != store.OutreachNudge || n.Channel != store.OutreachChannelDigest {
				t.Fatalf("folded nudge of %d = %+v %v %v", id, n, ok, err)
			}
		}
		fx.now = fx.now.Add(time.Hour)
		fx.refl.replies = nil
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(fx.lastInput(t), "already nudged"); got != 2 {
			t.Fatalf("next input marks %d rows as nudged, want 2", got)
		}
	})

	t.Run("a held alert is covered by the digest and not retried after", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.digestDue(t)
		_, job := fx.activeWatch(t, "stock:zs")
		fx.cfg.Companion.AlertBudget = 1
		if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachAlert, Text: "earlier", SentAt: fx.now.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
		fx.watchRun(t, job, "ALERT: ZS -6%", true)
		held := fx.watchRuns(t)[0]
		if held.ReportedOutreachID != 0 || len(fx.del.sent) != 0 {
			t.Fatalf("alert over budget was sent: %+v", fx.del.sent)
		}
		fx.refl.replies = []string{`{"action":"digest","message":"Morning."}`}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachDigest {
			t.Fatalf("sent = %+v, want the digest only", fx.del.sent)
		}
		if got := fx.runByID(t, held.ID).ReportedOutreachID; got != fx.del.sent[0].ID {
			t.Fatalf("held run reported by %d, want the digest %d", got, fx.del.sent[0].ID)
		}
		if err := fx.h.RetryHeldAlerts(ctx, fx.now); err != nil {
			t.Fatal(err)
		}
		if len(fx.del.sent) != 1 {
			t.Fatalf("the covered alert was retried: %+v", fx.del.sent)
		}
	})
}

func TestSeenAtOnlyAfterSuccess(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	_, job := fx.activeWatch(t, "stock:zs")
	fx.watchRun(t, job, "NOTHING: flat", true)
	runID := fx.watchRuns(t)[0].ID

	fx.refl.replies = []string{`{"action":"finding","message":"x","runs":[999]}`}
	if _, err := fx.h.Tick(ctx); err == nil {
		t.Fatal("a finding on an unshown run was accepted")
	}
	if !fx.runByID(t, runID).SeenAt.IsZero() {
		t.Fatal("an invalid reply marked the run seen")
	}

	fx.refl.replies = []string{`{"action":"none"}`}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if fx.runByID(t, runID).SeenAt.IsZero() {
		t.Fatal("a valid reply did not mark the run seen")
	}

	fx.watchRun(t, job, "NOTHING: flat again", true)
	fx.refl.replies = nil
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if last := fx.lastInput(t); !strings.Contains(last, "(seen before)") {
		t.Fatalf("next input does not mark the seen run:\n%s", last)
	}
}

func TestRunsOlderThan30DaysAreSwept(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, job := fx.activeWatch(t, "stock:zs")
	old, err := fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, JobID: job.ID, Kind: store.RunWatch,
		Verdict: store.VerdictNothing, StartedAt: fx.now.AddDate(0, 0, -31), FinishedAt: fx.now.AddDate(0, 0, -31)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	for _, r := range fx.watchRuns(t) {
		if r.ID == old {
			t.Fatal("a run 31 days old survived the sweep")
		}
	}
}

func TestFollowupSubjects(t *testing.T) {
	ctx := context.Background()
	const idle = "watch stock:zs has run 10 times in 14 days with no reply or reaction to anything about it: ask whether to keep, change or retire it"
	const failed = "watch stock:zs failed its last 3 runs: tell the user, or suggest changing or retiring it"

	t.Run("idle", func(t *testing.T) {
		fx := newHeartbeat(t)
		_, job := fx.activeWatch(t, "stock:zs")
		for range 10 {
			fx.watchRun(t, job, "NOTHING: flat", true)
		}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fx.lastInput(t), idle) {
			t.Fatalf("input lacks the idle follow-up:\n%s", fx.lastInput(t))
		}
	})

	t.Run("failed", func(t *testing.T) {
		fx := newHeartbeat(t)
		_, job := fx.activeWatch(t, "stock:zs")
		for range 3 {
			fx.watchRun(t, job, "", false)
		}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(fx.lastInput(t), failed) {
			t.Fatalf("input lacks the failed follow-up:\n%s", fx.lastInput(t))
		}
	})

	t.Run("a reaction clears the idle line", func(t *testing.T) {
		fx := newHeartbeat(t)
		in, job := fx.activeWatch(t, "stock:zs")
		for range 10 {
			fx.watchRun(t, job, "NOTHING: flat", true)
		}
		id, err := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-24 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fx.st.ResolveOutreach(ctx, id, []string{store.OutcomePending}, store.OutcomeReacted, "", fx.now); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(fx.lastInput(t), "has run") {
			t.Fatal("a reacted message did not clear the idle line")
		}
	})
}

// markDigestSent records fx.now's day as the day the digest went out.
func markDigestSent(t *testing.T, fx *hbFixture) {
	t.Helper()
	if err := fx.st.SetCompanionState(context.Background(), companion.StateLastDigestDay, fx.now.In(time.UTC).Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
}

const nudgeOfZero = `{"action":"nudge","nudge_of":[%d],"message":"Still there? Last week's note is waiting."}`

func TestRepliedProposalReturnsToCandidateAfterADay(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	sent := fx.del.sent[0]
	if moved, err := fx.st.ResolveOutreach(ctx, sent.ID, []string{store.OutcomePending}, store.OutcomeReplied, "", fx.now); err != nil || !moved {
		t.Fatalf("resolve replied = %v %v", moved, err)
	}

	// Each step crosses into a new day, so mark that day's digest as sent:
	// the digest slot would otherwise take the tick.
	fx.now = fx.now.Add(23 * time.Hour)
	markDigestSent(t, fx)
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("after 23h interest = %s, want proposed", got.State)
	}

	fx.now = fx.now.Add(2 * time.Hour)
	markDigestSent(t, fx)
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestCandidate {
		t.Fatalf("after 25h interest = %s, want candidate", got.State)
	}
	if !got.CooldownUntil.Equal(fx.now.Add(companion.ReturnCooldown)) {
		t.Fatalf("cooldown = %s, want %s", got.CooldownUntil, fx.now.Add(companion.ReturnCooldown))
	}
}

func TestBusyDecisionIsAppliedNextTickWithoutAModelCall(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	fx.del.err = companion.ErrBusy
	if res, err := fx.h.Tick(ctx); err != nil || res != "companion session busy (decision saved)" {
		t.Fatalf("busy tick = %q %v", res, err)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StatePendingDecision); v == "" {
		t.Fatal("busy decision was not saved")
	}

	fx.del.err = nil
	if res, err := fx.h.Tick(ctx); err != nil || res != "propose stock:zs (saved decision)" {
		t.Fatalf("next tick = %q %v", res, err)
	}
	if len(fx.refl.inputs) != 1 {
		t.Fatalf("model called %d times, want 1 (the busy tick only)", len(fx.refl.inputs))
	}
	if len(fx.del.sent) != 1 || fx.del.sent[0].Kind != store.OutreachProposal {
		t.Fatalf("sent = %+v, want one proposal", fx.del.sent)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestProposed {
		t.Fatalf("interest = %s, want proposed", got.State)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StatePendingDecision); v != "" {
		t.Fatalf("saved decision still stored: %s", v)
	}
}

func TestSavedDecisionDiscardedWhenStaleOrInvalid(t *testing.T) {
	ctx := context.Background()

	t.Run("stale", func(t *testing.T) {
		fx := newHeartbeat(t)
		// 08:30 UTC: thirteen hours later is 21:30, outside quiet hours.
		fx.now = time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
		fx.candidate(t, "stock:zs")
		fx.refl.replies = []string{proposeZS}
		fx.del.err = companion.ErrBusy
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fx.now = fx.now.Add(13 * time.Hour)
		fx.del.err = nil
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fx.refl.inputs) != 2 {
			t.Fatalf("model called %d times, want 2: the stale decision must not be applied", len(fx.refl.inputs))
		}
		if len(fx.del.sent) != 0 {
			t.Fatal("a stale decision was sent")
		}
		if v, _, _ := fx.st.CompanionState(ctx, companion.StatePendingDecision); v != "" {
			t.Fatalf("stale decision still stored: %s", v)
		}
	})

	t.Run("target answered", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.now = time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
		fx.candidate(t, "stock:zs")
		// Sent 13 hours ago, so the busy tick times it out to unanswered.
		id, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachFinding, Text: "earlier",
			SentAt: fx.now.Add(-13 * time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		fx.refl.replies = []string{fmt.Sprintf(nudgeOfZero, id)}
		fx.del.err = companion.ErrBusy
		if res, err := fx.h.Tick(ctx); err != nil || res != "companion session busy (decision saved)" {
			t.Fatalf("busy tick = %q %v", res, err)
		}
		if moved, err := fx.st.ResolveOutreach(ctx, id, []string{store.OutcomeUnanswered}, store.OutcomeReplied, "", fx.now); err != nil || !moved {
			t.Fatalf("resolve replied = %v %v", moved, err)
		}
		fx.del.err = nil
		if _, err := fx.h.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if len(fx.refl.inputs) != 2 {
			t.Fatalf("model called %d times, want 2: the invalid decision must be discarded", len(fx.refl.inputs))
		}
		if len(fx.del.sent) != 0 {
			t.Fatal("an invalid decision was sent")
		}
		if v, _, _ := fx.st.CompanionState(ctx, companion.StatePendingDecision); v != "" {
			t.Fatalf("invalid decision still stored: %s", v)
		}
	})
}

func TestOperatorPromoteLeavesSavedDecisionAndReflects(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	fx.candidate(t, "stock:zs")
	fx.refl.replies = []string{proposeZS}
	fx.del.err = companion.ErrBusy
	if _, err := fx.h.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	fx.del.err = nil
	res, err := fx.h.TickWith(ctx, companion.TickOptions{Manual: true, Promote: "stock:zs", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fx.refl.inputs) != 2 {
		t.Fatalf("model called %d times, want 2: a forced check reflects", len(fx.refl.inputs))
	}
	if strings.Contains(res, "saved decision") {
		t.Fatalf("forced check applied the saved decision: %q", res)
	}
	if v, _, _ := fx.st.CompanionState(ctx, companion.StatePendingDecision); v == "" {
		t.Fatal("forced check cleared the saved decision")
	}
}
