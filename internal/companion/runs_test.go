package companion_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		kind  string
		text  string
	}{
		{"alert with more lines", "ALERT: ZS fell 6%\nmore", store.VerdictAlert, "ZS fell 6%\nmore"},
		{"lower case nothing", "nothing: flat day", store.VerdictNothing, "flat day"},
		{"fenced nothing", "```\nNOTHING: x\n```", store.VerdictNothing, "x"},
		{"bold alert", "**ALERT:** big move", store.VerdictAlert, "big move"},
		{"heading alert", "## Alert: y", store.VerdictAlert, "y"},
		{"leading blank lines", "\n\n  NOTHING: z", store.VerdictNothing, "z"},
		{"no marker", "no marker here", store.VerdictAlert, "no marker here"},
		{"empty", "", store.VerdictAlert, ""},
		{"bold nothing before colon", "**NOTHING**: flat day", store.VerdictNothing, "flat day"},
		{"underscore alert before colon", "__ALERT__: x", store.VerdictAlert, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := companion.ParseVerdict(c.reply)
			if got.Kind != c.kind || got.Message != c.text {
				t.Fatalf("ParseVerdict(%q) = %+v, want {%s %q}", c.reply, got, c.kind, c.text)
			}
		})
	}
}

func TestAlertTextClips(t *testing.T) {
	long := strings.Repeat("a", 1300)
	want := strings.Repeat("a", 1200) + "\n\n· the rest is in the Jobs folder"
	if got := companion.AlertText(long); got != want {
		t.Fatalf("AlertText(1300 runes) = %d runes, want %d", len([]rune(got)), len([]rune(want)))
	}
	short := "ZS fell 6%"
	if got := companion.AlertText(short); got != short {
		t.Fatalf("AlertText(short) = %q, want unchanged", got)
	}
	exact := strings.Repeat("a", 1200)
	if got := companion.AlertText(exact); got != exact {
		t.Fatalf("AlertText(exactly 1200 runes) changed, want unchanged")
	}
	multi := strings.Repeat("é", 1300)
	wantMulti := strings.Repeat("é", 1200) + "\n\n· the rest is in the Jobs folder"
	if got := companion.AlertText(multi); got != wantMulti {
		t.Fatalf("AlertText(1300 multi-byte runes) = %d runes, want 1200 é plus footer", len([]rune(got)))
	}
}

func TestResearchPromptTrimsQuestion(t *testing.T) {
	got := companion.ResearchPrompt("ZS", "  what changed?  ")
	if !strings.Contains(got, "on ZS.") || !strings.Contains(got, "Question: what changed?\n") {
		t.Fatalf("prompt = %q", got)
	}
}

// activeWatch seeds an interest proposed and its watch job, active, as the
// user's Yes would leave it. The job's origin is the companion session.
func (fx *hbFixture) activeWatch(t *testing.T, key string) (store.Interest, store.Job) {
	t.Helper()
	ctx := context.Background()
	in := fx.candidate(t, key)
	if ok, err := fx.st.SetInterestState(ctx, in.ID, store.InterestCandidate, store.InterestProposed); err != nil || !ok {
		t.Fatalf("propose %s: %v %v", key, ok, err)
	}
	sid, err := fx.del.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := fx.st.CreateJob(ctx, store.Job{Kind: "cron", Spec: "0 14 * * 1-5",
		Prompt: companion.WatchPrompt(in.Label, "alert on a daily move over 5%."), Enabled: true,
		NextRun: fx.now.Add(time.Hour), OriginSessionID: sid, Notify: store.NotifyNone, CheckedIn: true,
		Profile: config.CompanionProfile})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := fx.st.ActivateInterest(ctx, in.ID, jobID); err != nil || !ok {
		t.Fatalf("activate %s: %v %v", key, ok, err)
	}
	got, _, _ := fx.st.InterestByKey(ctx, key)
	job, ok, err := fx.st.Job(ctx, jobID)
	if err != nil || !ok {
		t.Fatalf("job %d: %v %v", jobID, ok, err)
	}
	return got, job
}

// reply appends an assistant text message to session sid, as a run's reply.
func (fx *hbFixture) reply(t *testing.T, sid, text string) {
	t.Helper()
	blocks, err := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.AppendMessage(context.Background(), store.Message{SessionID: sid,
		Role: string(provider.RoleAssistant), BlocksJSON: blocks}); err != nil {
		t.Fatal(err)
	}
}

// runSession is a fresh session standing in for one job run.
func (fx *hbFixture) runSession(t *testing.T) string {
	t.Helper()
	sid, err := fx.st.CreateSessionFrom(context.Background(), "run", "", store.SourceJob)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// plainJob is a job no companion owns.
func (fx *hbFixture) plainJob(t *testing.T, prompt string) store.Job {
	t.Helper()
	id, err := fx.st.CreateJob(context.Background(), store.Job{Kind: "cron", Spec: "0 9 * * *",
		Prompt: prompt, Enabled: true, NextRun: fx.now.Add(time.Hour), Notify: store.NotifyNone})
	if err != nil {
		t.Fatal(err)
	}
	job, _, _ := fx.st.Job(context.Background(), id)
	return job
}

// watchRuns is every companion run on record, newest first.
func (fx *hbFixture) watchRuns(t *testing.T) []store.CompanionRun {
	t.Helper()
	runs, err := fx.st.CompanionRuns(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

func TestRunPromptAddsTheSuffixOnlyToWatches(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, watch := fx.activeWatch(t, "stock:zs")

	if got := fx.h.RunPrompt(ctx, watch, fx.runSession(t)); got != watch.Prompt+companion.VerdictSuffix {
		t.Fatalf("watch prompt = %q, want the watch prompt plus VerdictSuffix", got)
	}

	plain := fx.plainJob(t, "Send me a joke")
	if got := fx.h.RunPrompt(ctx, plain, fx.runSession(t)); got != "Send me a joke" {
		t.Fatalf("plain prompt = %q, want unchanged", got)
	}

	research := fx.plainJob(t, companion.ResearchPrompt("ZS", "what changed?"))
	rid, err := fx.st.AddCompanionRun(ctx, store.CompanionRun{InterestID: in.ID, JobID: research.ID,
		Kind: store.RunResearch, Question: "what changed?", Verdict: store.VerdictPending, StartedAt: fx.now})
	if err != nil {
		t.Fatal(err)
	}
	run := fx.runSession(t)
	if got := fx.h.RunPrompt(ctx, research, run); got != research.Prompt {
		t.Fatalf("research prompt = %q, want unchanged", got)
	}
	row, ok, err := fx.st.CompanionRunByJob(ctx, research.ID, store.RunResearch)
	if err != nil || !ok || row.ID != rid || row.SessionID != run {
		t.Fatalf("research row = %+v %v %v, want session %s", row, ok, err, run)
	}
}

func TestWatchNothingPostsNothing(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	in, job := fx.activeWatch(t, "stock:zs")
	run := fx.runSession(t)
	fx.reply(t, run, "NOTHING: flat")

	handled, failNote := fx.h.RunFinished(ctx, job, run, true)
	if !handled || failNote {
		t.Fatalf("RunFinished = %v %v, want handled and no fail note", handled, failNote)
	}
	runs := fx.watchRuns(t)
	if len(runs) != 1 || runs[0].Verdict != store.VerdictNothing || runs[0].Kind != store.RunWatch ||
		runs[0].InterestID != in.ID || runs[0].JobID != job.ID {
		t.Fatalf("runs = %+v, want one nothing watch run", runs)
	}
	if len(fx.del.sent) != 0 {
		t.Fatalf("delivered %+v, want nothing", fx.del.sent)
	}
}

func TestWatchAlertIsDelivered(t *testing.T) {
	ctx := context.Background()
	t.Run("message", func(t *testing.T) {
		fx := newHeartbeat(t)
		_, job := fx.activeWatch(t, "stock:zs")
		run := fx.runSession(t)
		fx.reply(t, run, "ALERT: ZS -6%")

		if handled, _ := fx.h.RunFinished(ctx, job, run, true); !handled {
			t.Fatal("RunFinished did not handle the watch")
		}
		if len(fx.del.sent) != 1 {
			t.Fatalf("delivered %d, want 1", len(fx.del.sent))
		}
		o := fx.del.sent[0]
		if o.Kind != store.OutreachAlert || o.Text != "ZS -6%" {
			t.Fatalf("outreach = %+v, want an alert with text ZS -6%%", o)
		}
		runs := fx.watchRuns(t)
		if len(runs) != 1 || runs[0].Verdict != store.VerdictAlert || runs[0].ReportedOutreachID != o.ID {
			t.Fatalf("runs = %+v, want one alert reported by outreach %d", runs, o.ID)
		}
	})
	t.Run("empty alert", func(t *testing.T) {
		fx := newHeartbeat(t)
		in, job := fx.activeWatch(t, "stock:zs")
		run := fx.runSession(t)
		fx.reply(t, run, "ALERT:")

		fx.h.RunFinished(ctx, job, run, true)
		if len(fx.del.sent) != 1 {
			t.Fatalf("delivered %d, want 1", len(fx.del.sent))
		}
		want := in.Label + " watch raised an alert with no details · see the Jobs folder"
		if got := fx.del.sent[0].Text; got != want {
			t.Fatalf("text = %q, want %q", got, want)
		}
	})
}

func TestWatchAlertHeld(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		setup func(t *testing.T, fx *hbFixture)
	}{
		{"over the alert budget", func(t *testing.T, fx *hbFixture) {
			for i := 0; i < fx.cfg.Companion.AlertBudget; i++ {
				if _, err := fx.st.AddOutreach(ctx, store.Outreach{Kind: store.OutreachAlert,
					Channel: store.OutreachChannelTerminal, Text: "earlier", SentAt: fx.now}); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{"quiet hours", func(t *testing.T, fx *hbFixture) {
			fx.now = time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC)
		}},
		{"paused", func(t *testing.T, fx *hbFixture) {
			if err := companion.SetPausedUntil(ctx, fx.st, fx.now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}},
		{"the companion session is busy", func(t *testing.T, fx *hbFixture) {
			fx.del.err = companion.ErrBusy
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newHeartbeat(t)
			_, job := fx.activeWatch(t, "stock:zs")
			run := fx.runSession(t)
			fx.reply(t, run, "ALERT: ZS -6%")
			c.setup(t, fx)

			if handled, _ := fx.h.RunFinished(ctx, job, run, true); !handled {
				t.Fatal("RunFinished did not handle the watch")
			}
			if len(fx.del.sent) != 0 {
				t.Fatalf("delivered %+v, want the alert held", fx.del.sent)
			}
			runs := fx.watchRuns(t)
			if len(runs) != 1 || runs[0].Verdict != store.VerdictAlert || runs[0].ReportedOutreachID != 0 {
				t.Fatalf("runs = %+v, want one unreported alert", runs)
			}
		})
	}
	t.Run("quiet hours allowed by the flag", func(t *testing.T) {
		fx := newHeartbeat(t)
		fx.cfg.Companion.AlertsInQuietHours = true
		fx.now = time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC)
		_, job := fx.activeWatch(t, "stock:zs")
		run := fx.runSession(t)
		fx.reply(t, run, "ALERT: ZS -6%")

		fx.h.RunFinished(ctx, job, run, true)
		if len(fx.del.sent) != 1 {
			t.Fatalf("delivered %d, want the alert sent in quiet hours", len(fx.del.sent))
		}
	})
}

func TestRetryHeldAlertsSendsWithoutAModelCall(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	_, job := fx.activeWatch(t, "stock:zs")
	run := fx.runSession(t)
	fx.reply(t, run, "ALERT: ZS -6%")

	fx.del.err = companion.ErrBusy
	fx.h.RunFinished(ctx, job, run, true)
	if len(fx.del.sent) != 0 {
		t.Fatal("a busy session still got the alert")
	}
	fx.del.err = nil
	if err := fx.h.RetryHeldAlerts(ctx, fx.now); err != nil {
		t.Fatal(err)
	}
	if len(fx.del.sent) != 1 || fx.del.sent[0].Text != "ZS -6%" {
		t.Fatalf("delivered %+v, want the held alert", fx.del.sent)
	}
	if runs := fx.watchRuns(t); runs[0].ReportedOutreachID != fx.del.sent[0].ID {
		t.Fatalf("run not reported by the retried outreach: %+v", runs[0])
	}
	if len(fx.refl.inputs) != 0 {
		t.Fatalf("retry made %d model calls, want none", len(fx.refl.inputs))
	}
}

func TestFailedWatchRunIsRecordedFailed(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	_, job := fx.activeWatch(t, "stock:zs")
	handled, failNote := fx.h.RunFinished(ctx, job, fx.runSession(t), false)
	if !handled || !failNote {
		t.Fatalf("RunFinished = %v %v, want handled with a fail note", handled, failNote)
	}
	runs := fx.watchRuns(t)
	if len(runs) != 1 || runs[0].Verdict != store.VerdictFailed {
		t.Fatalf("runs = %+v, want one failed run", runs)
	}
	if len(fx.del.sent) != 0 {
		t.Fatal("a failed run sent an outreach")
	}
}

func TestDisabledCompanionLeavesJobsAlone(t *testing.T) {
	ctx := context.Background()
	fx := newHeartbeat(t)
	_, watch := fx.activeWatch(t, "stock:zs")
	fx.cfg.Companion.Enabled = false
	if got := fx.h.RunPrompt(ctx, watch, fx.runSession(t)); got != watch.Prompt {
		t.Fatalf("prompt = %q, want unchanged while disabled", got)
	}
	if handled, failNote := fx.h.RunFinished(ctx, watch, fx.runSession(t), true); handled || failNote {
		t.Fatalf("RunFinished = %v %v while disabled, want false false", handled, failNote)
	}
	if runs := fx.watchRuns(t); len(runs) != 0 {
		t.Fatalf("runs = %+v while disabled, want none", runs)
	}
}
