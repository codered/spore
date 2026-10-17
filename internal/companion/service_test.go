package companion_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type svcFixture struct {
	*hbFixture
	svc *companion.Service
	sid string
}

func newService(t *testing.T) *svcFixture {
	t.Helper()
	fx := newHeartbeat(t)
	svc := companion.NewService(fx.st, fx.cfg)
	svc.Now = func() time.Time { return fx.now }
	sid, err := fx.del.EnsureSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return &svcFixture{hbFixture: fx, svc: svc, sid: sid}
}

// proposal seeds a proposed interest and its pending proposal.
func (fx *svcFixture) proposal(t *testing.T, key string) (store.Interest, int64) {
	t.Helper()
	ctx := context.Background()
	in := fx.candidate(t, key)
	if ok, err := fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed); err != nil || !ok {
		t.Fatalf("propose %s: %v %v", key, ok, err)
	}
	id, err := fx.st.AddOutreach(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal, Text: "watch?",
		Payload: `{"cron":"0 14 * * 1-5","goal":"Check it."}`, SentAt: fx.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return in, id
}

func TestAnswerProposalYesCreatesTheWatch(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_ = companion.SaveBudget(ctx, fx.st, 2)
	in, id := fx.proposal(t, "stock:zs")
	msg, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes)
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestActive || got.WatchJobID == 0 {
		t.Fatalf("interest = %+v", got)
	}
	job, ok, err := fx.st.Job(ctx, got.WatchJobID)
	if err != nil || !ok {
		t.Fatalf("job: %v %v", ok, err)
	}
	if job.Profile != config.CompanionProfile || job.Notify != store.NotifyNone || !job.CheckedIn ||
		job.OriginSessionID != fx.sid || job.Spec != "0 14 * * 1-5" || !job.Enabled ||
		!strings.Contains(job.Prompt, in.Label) || !strings.Contains(job.Prompt, "Check it.") {
		t.Fatalf("watch job = %+v", job)
	}
	if !strings.Contains(msg, in.Label) {
		t.Errorf("message = %q", msg)
	}
	if o := fx.outcome(t, id); o.Outcome != store.OutcomeReacted {
		t.Fatalf("outcome = %s, want reacted", o.Outcome)
	}
	if b := fx.budget(t); b != 2.25 {
		t.Fatalf("budget = %v, want 2.25", b)
	}
}

func TestAnswerProposalTwiceCreatesOneJob(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("second press err = %v, want ErrNotProposal", err)
	}
	jobs, _ := fx.st.ListJobs(ctx)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(jobs))
	}
}

func TestYesRefusesWhenTheCompanionSessionIsGone(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.st.DeleteSessions(ctx, []string{fx.sid}); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("yes with the session gone = %v, want ErrNotProposal", err)
	}
	if jobs, _ := fx.st.ListJobs(ctx); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0: a watch with no session would report nowhere", len(jobs))
	}
}

func TestAnswerProposalRefusesAResolvedProposal(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if ok, err := fx.st.ResolveOutreach(ctx, id, []string{store.OutcomePending}, store.OutcomeIgnored, "", fx.now); err != nil || !ok {
		t.Fatalf("resolve to ignored: %v %v", ok, err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("yes on an ignored proposal = %v, want ErrNotProposal", err)
	}
	if jobs, _ := fx.st.ListJobs(ctx); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0", len(jobs))
	}
}

func TestAnswerProposalNotNowAndNever(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, later := fx.proposal(t, "stock:zs")
	_, never := fx.proposal(t, "topic:go")
	if _, err := fx.svc.AnswerProposal(ctx, later, companion.ChoiceNotNow); err != nil {
		t.Fatal(err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	if got.State != store.InterestObserving || !got.CooldownUntil.Equal(fx.now.Add(companion.NotNowCooldown)) {
		t.Fatalf("not now = %s until %v", got.State, got.CooldownUntil)
	}
	if _, err := fx.svc.AnswerProposal(ctx, never, companion.ChoiceNever); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "topic:go"); got.State != store.InterestDeclined {
		t.Fatalf("never = %s", got.State)
	}
	if _, err := fx.svc.AnswerProposal(ctx, later, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("yes after not now = %v, want ErrNotProposal", err)
	}
	if jobs, _ := fx.st.ListJobs(ctx); len(jobs) != 0 {
		t.Fatalf("jobs = %d, want 0", len(jobs))
	}
}

func TestAnswerProposalRefusesOtherInput(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, "maybe"); err == nil {
		t.Fatal("an unknown choice must be refused")
	}
	nudge, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "x", NudgeOf: id, SentAt: fx.now})
	if _, err := fx.svc.AnswerProposal(ctx, nudge, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("answering a nudge = %v, want ErrNotProposal", err)
	}
	if _, err := fx.svc.AnswerProposal(ctx, 9999, companion.ChoiceYes); !errors.Is(err, companion.ErrNotProposal) {
		t.Fatalf("answering nothing = %v, want ErrNotProposal", err)
	}
}

func TestUserRepliedResolvesOnlyInTheCompanionSession(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	a, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "a", SentAt: fx.now})
	b, _ := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "b", SentAt: fx.now.Add(-13 * time.Hour)})
	_, _ = fx.st.ResolveOutreach(ctx, b, []string{store.OutcomePending}, store.OutcomeUnanswered, "", fx.now)
	other, _ := fx.st.CreateSessionFrom(ctx, "chat", "", store.SourceChat)
	if err := fx.svc.UserReplied(ctx, other); err != nil {
		t.Fatal(err)
	}
	if o := fx.outcome(t, a); o.Outcome != store.OutcomePending {
		t.Fatalf("a reply elsewhere resolved %d", a)
	}
	if err := fx.svc.UserReplied(ctx, fx.sid); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{a, b} {
		if o := fx.outcome(t, id); o.Outcome != store.OutcomeReplied {
			t.Fatalf("outreach %d = %s, want replied", id, o.Outcome)
		}
	}
}

func TestFeedbackAndPause(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	if _, err := fx.svc.Feedback(ctx, "louder"); err == nil {
		t.Fatal("an unknown kind must be refused")
	}
	if _, err := fx.svc.Feedback(ctx, companion.FeedbackStop); err != nil {
		t.Fatal(err)
	}
	if b := fx.budget(t); b != 2 {
		t.Fatalf("budget after stop = %v, want 2", b)
	}
	until, _ := companion.PausedUntil(ctx, fx.st)
	if !until.Equal(fx.now.Add(companion.StopPause)) {
		t.Fatalf("paused until %v, want 7 days", until)
	}
	if err := fx.svc.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if until, _ := companion.PausedUntil(ctx, fx.st); !until.IsZero() {
		t.Fatalf("still paused until %v", until)
	}
	got, err := fx.svc.Pause(ctx, 3*time.Hour)
	if err != nil || !got.Equal(fx.now.Add(3*time.Hour)) {
		t.Fatalf("pause = %v %v", got, err)
	}
	if _, err := fx.svc.Pause(ctx, 0); err == nil {
		t.Fatal("a zero pause must be refused")
	}
}

func TestUpdateInterestNeverActivates(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	fx.candidate(t, "stock:zs")
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "", "", store.InterestActive); err == nil || !strings.Contains(err.Error(), "only") {
		t.Fatalf("activating through interest_update = %v, want refused", err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestCandidate {
		t.Fatalf("state = %s", got.State)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "0 9 * * *", "", ""); err == nil {
		t.Fatal("changing a watch that is not active must be refused")
	}
}

func TestUpdateInterestChangesAndRetiresAWatch(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	if _, err := fx.svc.AnswerProposal(ctx, id, companion.ChoiceYes); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "*/15 * * * *", "", ""); err == nil {
		t.Fatal("a cron firing every 15 minutes must be refused")
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "0 16 * * 1-5", "Check ZS after the close.", ""); err != nil {
		t.Fatal(err)
	}
	in, _, _ := fx.st.InterestByKey(ctx, "stock:zs")
	job, _, _ := fx.st.Job(ctx, in.WatchJobID)
	if job.Spec != "0 16 * * 1-5" || !strings.Contains(job.Prompt, "after the close") {
		t.Fatalf("job = %+v", job)
	}
	if _, err := fx.svc.UpdateInterest(ctx, "stock:zs", "", "", store.InterestRetired); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestRetired {
		t.Fatalf("state = %s", got.State)
	}
	if job, _, _ := fx.st.Job(ctx, in.WatchJobID); job.Enabled {
		t.Fatal("retiring an interest must stop its watch")
	}
}

func TestUndecline(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, id := fx.proposal(t, "stock:zs")
	_, _ = fx.svc.AnswerProposal(ctx, id, companion.ChoiceNever)
	if err := fx.svc.Undecline(ctx, "stock:zs"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := fx.st.InterestByKey(ctx, "stock:zs"); got.State != store.InterestObserving {
		t.Fatalf("state = %s", got.State)
	}
	if err := fx.svc.Undecline(ctx, "stock:zs"); err == nil {
		t.Fatal("undeclining an observing interest must be refused")
	}
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	_, _ = fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: fx.now.Add(-time.Hour)})
	_ = fx.st.SetCompanionState(ctx, companion.StateFailures, "3")
	_ = fx.st.SetCompanionState(ctx, companion.StateLastResult, "failed: provider down")
	s, err := fx.svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Enabled || s.Channel != config.CompanionChannelTerminal || s.Budget != 4 || s.SpentToday != 1 ||
		s.Failures != 3 || s.LastResult != "failed: provider down" || s.SessionID != fx.sid {
		t.Fatalf("status = %+v", s)
	}
	if s.AlertBudget != 3 || !s.DigestSentAt.IsZero() || s.DigestDue || s.ResearchToday != 0 ||
		s.ResearchRunning != 0 || s.AlertsToday != 0 || s.AlertsHeld != 0 {
		t.Fatalf("digest, research and alert fields = %+v", s)
	}
}

func TestStatusCountsResearchAndAlerts(t *testing.T) {
	ctx := context.Background()
	fx := newService(t)
	in, _ := fx.st.TouchInterest(ctx, "stock:zs", "ZS")
	_ = fx.st.SetCompanionState(ctx, companion.StateLastDigestDay, "2026-10-11")
	_, _ = fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, Kind: store.RunResearch,
		Question: "old", Verdict: store.VerdictPending, StartedAt: fx.now.Add(-48 * time.Hour)})
	_, _ = fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, Kind: store.RunResearch,
		Question: "today", Verdict: store.VerdictPending, StartedAt: fx.now.Add(-time.Hour)})
	_, _ = fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, Kind: store.RunWatch,
		Question: "held", Verdict: store.VerdictAlert, StartedAt: fx.now.Add(-time.Hour), FinishedAt: fx.now.Add(-time.Hour)})
	s, err := fx.svc.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.DigestDue || s.ResearchToday != 1 || s.ResearchRunning != 2 || s.AlertsHeld != 1 {
		t.Fatalf("status = %+v", s)
	}
}
