package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestOutreachLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	at := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	in, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachProposal, Channel: OutreachChannelTerminal,
		Text: "watch ZS?", Payload: `{"cron":"0 14 * * 1-5","goal":"g"}`, SentAt: at})
	if err != nil {
		t.Fatal(err)
	}
	o, ok, err := st.OutreachByID(ctx, id)
	if err != nil || !ok {
		t.Fatalf("OutreachByID: %v %v", ok, err)
	}
	if o.Outcome != OutcomePending || o.InterestID != in.ID || !o.SentAt.Equal(at) || o.NudgeOf != 0 {
		t.Fatalf("stored = %+v", o)
	}
	moved, err := st.ResolveOutreach(ctx, id, []string{OutcomePending}, OutcomeUnanswered, "", at.Add(13*time.Hour))
	if err != nil || !moved {
		t.Fatalf("pending -> unanswered: %v %v", moved, err)
	}
	moved, err = st.ResolveOutreach(ctx, id, []string{OutcomePending}, OutcomeIgnored, "", at)
	if err != nil || moved {
		t.Fatalf("a second resolve from pending moved it: %v %v", moved, err)
	}
	nid, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: OutreachNudge, Text: "still?", NudgeOf: id, SentAt: at.Add(14 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	n, ok, err := st.NudgeFor(ctx, id)
	if err != nil || !ok || n.ID != nid {
		t.Fatalf("NudgeFor = %+v %v %v", n, ok, err)
	}
	if _, ok, _ := st.NudgeFor(ctx, nid); ok {
		t.Fatal("a nudge has no nudge of its own")
	}
	un, err := st.OutreachByOutcome(ctx, OutcomeUnanswered)
	if err != nil || len(un) != 1 || un[0].ID != id {
		t.Fatalf("unanswered = %+v %v", un, err)
	}
	since, err := st.OutreachSince(ctx, at.Add(time.Hour))
	if err != nil || len(since) != 1 || since[0].ID != nid {
		t.Fatalf("since = %+v %v", since, err)
	}
	if err := st.SetOutreachChannel(ctx, nid, OutreachChannelFallback); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.OutreachByID(ctx, nid); n.Channel != OutreachChannelFallback {
		t.Fatalf("channel = %q", n.Channel)
	}
	if _, err := st.ResolveOutreach(ctx, id, []string{OutcomeUnanswered}, OutcomeIgnored, "moment passed", at); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := st.OutreachByID(ctx, id); o.DropReason != "moment passed" || o.Outcome != OutcomeIgnored {
		t.Fatalf("dropped = %+v", o)
	}
}

func TestCompanionState(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	if _, ok, err := st.CompanionState(ctx, "daily_budget"); err != nil || ok {
		t.Fatalf("missing key: %v %v", ok, err)
	}
	for _, v := range []string{"4", "3.5"} {
		if err := st.SetCompanionState(ctx, "daily_budget", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := st.CompanionState(ctx, "daily_budget"); err != nil || !ok || v != "3.5" {
		t.Fatalf("got %q %v %v", v, ok, err)
	}
}

func TestJobProfileNotifyAndCheckIn(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	next := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC)
	id, err := st.CreateJob(ctx, Job{Kind: "cron", Spec: "0 14 * * 1-5", Prompt: "watch", Enabled: true, NextRun: next,
		Notify: NotifyEach, CheckedIn: true, Profile: "companion"})
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := st.Job(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.Profile != "companion" || j.Notify != NotifyEach || !j.CheckedIn {
		t.Fatalf("watch job = %+v", j)
	}
	plain, err := st.CreateJob(ctx, Job{Kind: "cron", Spec: "0 9 * * *", Prompt: "p", Enabled: true, NextRun: next, Notify: NotifyAsk})
	if err != nil {
		t.Fatal(err)
	}
	if j, _, _ := st.Job(ctx, plain); j.Profile != "local" || j.Notify != NotifyAsk || j.CheckedIn {
		t.Fatalf("plain job = %+v", j)
	}
	later := next.Add(24 * time.Hour)
	if err := st.UpdateJobSchedule(ctx, id, "0 15 * * 1-5", "watch harder", later); err != nil {
		t.Fatal(err)
	}
	if j, _, _ := st.Job(ctx, id); j.Spec != "0 15 * * 1-5" || j.Prompt != "watch harder" || !j.NextRun.Equal(later) {
		t.Fatalf("updated = %+v", j)
	}
}

func TestInterestHelpers(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	sid, _ := st.CreateSessionFrom(ctx, "t", "", SourceChat)
	in, _ := st.TouchInterest(ctx, "stock:zs", "ZS")
	for _, d := range []string{"2026-09-01", "2026-10-10", "2026-10-11", "2026-10-11"} {
		at, _ := time.Parse("2006-01-02", d)
		if err := st.AddInterestSignal(ctx, in.ID, sid, "asked", d, at); err != nil {
			t.Fatal(err)
		}
	}
	days, err := st.InterestDays(ctx, in.ID)
	if err != nil || len(days) != 3 || days[0] != "2026-09-01" || days[2] != "2026-10-11" {
		t.Fatalf("days = %v %v", days, err)
	}
	if n, err := st.InterestDaysSince(ctx, in.ID, "2026-10-01"); err != nil || n != 2 {
		t.Fatalf("days since = %d %v", n, err)
	}
	until := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := st.SetInterestCooldown(ctx, in.ID, until); err != nil {
		t.Fatal(err)
	}
	got, ok, err := st.InterestByID(ctx, in.ID)
	if err != nil || !ok || !got.CooldownUntil.Equal(until) || got.DaysSeen != 3 {
		t.Fatalf("by id = %+v %v %v", got, ok, err)
	}
	if moved, _ := st.ActivateInterest(ctx, in.ID, 7); moved {
		t.Fatal("an observing interest was activated; only a proposed one may be")
	}
	_, _ = st.SetInterestState(ctx, in.ID, InterestObserving, InterestProposed)
	if moved, err := st.ActivateInterest(ctx, in.ID, 7); err != nil || !moved {
		t.Fatalf("activate: %v %v", moved, err)
	}
	if got, _, _ := st.InterestByID(ctx, in.ID); got.State != InterestActive || got.WatchJobID != 7 {
		t.Fatalf("active = %+v", got)
	}
	other, _ := st.TouchInterest(ctx, "topic:go", "Go")
	_, _ = st.SetInterestState(ctx, other.ID, InterestObserving, InterestProposed)
	if moved, err := st.DeclineInterest(ctx, other.ID); err != nil || !moved {
		t.Fatalf("decline: %v %v", moved, err)
	}
	if got, _, _ := st.InterestByID(ctx, other.ID); got.State != InterestDeclined {
		t.Fatalf("declined = %+v", got)
	}
}

// The companion session is where the user answers spore; refinement must
// not review it as a chat (spec PR 1 parked item).
func TestIdleSessionsSkipsTheCompanionSession(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	comp, _ := st.CreateSessionFrom(ctx, "spore", "", SourceCompanion)
	chat, _ := st.CreateSessionFrom(ctx, "chat", "", SourceChat)
	for _, id := range []string{comp, chat} {
		if _, err := st.AppendMessage(ctx, Message{SessionID: id, Role: "user", BlocksJSON: []byte(`[{"type":"text","text":"hi"}]`)}); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := st.IdleSessions(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != chat {
		t.Fatalf("idle = %v, want only the chat %s", ids, chat)
	}
}

// OpenProposals lists what a button press can still answer, newest first:
// the test Service.AnswerProposal applies.
func TestOpenProposals(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	at := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	interest := func(key, state string) Interest {
		in, err := st.TouchInterest(ctx, key, key)
		if err != nil {
			t.Fatal(err)
		}
		if state != InterestObserving {
			if _, err := st.SetInterestState(ctx, in.ID, InterestObserving, state); err != nil {
				t.Fatal(err)
			}
		}
		return in
	}
	add := func(in Interest, kind string, sent time.Time) int64 {
		id, err := st.AddOutreach(ctx, Outreach{InterestID: in.ID, Kind: kind, Channel: OutreachChannelTerminal, Text: in.Key, SentAt: sent})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	zs := interest("stock:zs", InterestProposed)
	goTopic := interest("topic:go", InterestProposed)
	declined := interest("topic:old", InterestDeclined)

	older := add(zs, OutreachProposal, at)
	newer := add(goTopic, OutreachProposal, at.Add(time.Hour))
	add(zs, OutreachNudge, at.Add(2*time.Hour))                // not a proposal
	add(declined, OutreachProposal, at.Add(3*time.Hour))       // its interest is no longer proposed
	answered := add(zs, OutreachProposal, at.Add(4*time.Hour)) // answered below
	if _, err := st.ResolveOutreach(ctx, answered, []string{OutcomePending}, OutcomeReplied, "", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// unanswered is still open: the user may press the button late.
	if _, err := st.ResolveOutreach(ctx, older, []string{OutcomePending}, OutcomeUnanswered, "", at.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}

	got, err := st.OpenProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, o := range got {
		ids = append(ids, o.ID)
	}
	if !slices.Equal(ids, []int64{newer, older}) {
		t.Fatalf("open proposals = %v, want [%d %d]", ids, newer, older)
	}
}

func TestLatestProposal(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	at := time.Date(2026, 10, 12, 15, 0, 0, 0, time.UTC)
	zs, err := st.TouchInterest(ctx, "stock:zs", "ZS")
	if err != nil {
		t.Fatal(err)
	}
	nvda, err := st.TouchInterest(ctx, "stock:nvda", "NVDA")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.LatestProposal(ctx, zs.ID); err != nil || ok {
		t.Fatalf("no proposal yet: ok=%v err=%v", ok, err)
	}
	first, err := st.AddOutreach(ctx, Outreach{InterestID: zs.ID, Kind: OutreachProposal, Text: "one", SentAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddOutreach(ctx, Outreach{InterestID: zs.ID, Kind: OutreachFinding, Text: "not a proposal", SentAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddOutreach(ctx, Outreach{InterestID: nvda.ID, Kind: OutreachProposal, Text: "other", SentAt: at.Add(2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if o, ok, err := st.LatestProposal(ctx, zs.ID); err != nil || !ok || o.ID != first {
		t.Fatalf("latest before re-proposal = %+v %v %v, want id %d", o, ok, err, first)
	}
	second, err := st.AddOutreach(ctx, Outreach{InterestID: zs.ID, Kind: OutreachProposal, Text: "two", SentAt: at.Add(3 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if o, ok, err := st.LatestProposal(ctx, zs.ID); err != nil || !ok || o.ID != second {
		t.Fatalf("latest = %+v %v %v, want id %d", o, ok, err, second)
	}
}
