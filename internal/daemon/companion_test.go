package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type failingChannel struct{}

func (failingChannel) Name() string { return store.OutreachChannelDiscord }
func (failingChannel) Send(context.Context, string, store.Outreach) error {
	return errors.New("discord is down")
}

func TestDeliverRecordsAndAppends(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	o, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "Want me to watch ZS?", SentAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	sid, ok, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	if !ok || sid == "" {
		t.Fatal("no companion session recorded")
	}
	sess, _, _ := s.store.Session(ctx, sid)
	if sess.Source != store.SourceCompanion {
		t.Fatalf("source = %q", sess.Source)
	}
	msgs, err := s.store.Messages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("messages = %+v", msgs)
	}
	if !strings.Contains(string(msgs[0].BlocksJSON), SchedulerTag) || !strings.Contains(string(msgs[0].BlocksJSON), "#"+strconv.FormatInt(o.ID, 10)) {
		t.Errorf("cue = %s", msgs[0].BlocksJSON)
	}
	if !strings.Contains(string(msgs[1].BlocksJSON), "Want me to watch ZS?") {
		t.Errorf("assistant = %s", msgs[1].BlocksJSON)
	}
	got, _, _ := s.store.OutreachByID(ctx, o.ID)
	if got.Channel != store.OutreachChannelTerminal || got.Outcome != store.OutcomePending {
		t.Fatalf("outreach = %+v", got)
	}
	if _, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachNudge, Text: "still?", NudgeOf: o.ID, SentAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if again, _, _ := s.store.CompanionState(ctx, companion.StateSessionID); again != sid {
		t.Fatalf("second delivery opened session %s, want %s", again, sid)
	}
}

func TestDeliverFallsBackToTheTerminal(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	s.SetCompanionChannel(failingChannel{})
	o, err := s.CompanionDeliverer().Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: time.Now()})
	if err != nil {
		t.Fatalf("a failed Discord send must not fail the delivery: %v", err)
	}
	if got, _, _ := s.store.OutreachByID(ctx, o.ID); got.Channel != store.OutreachChannelFallback {
		t.Fatalf("channel = %q, want terminal-fallback", got.Channel)
	}
}

func TestDeliverRefusesWhileATurnRuns(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	sid, err := d.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !s.hub.Begin(sid) {
		t.Fatal("could not hold the slot")
	}
	defer s.hub.End(sid)
	if _, err := d.Deliver(ctx, store.Outreach{Kind: store.OutreachProposal, Text: "x", SentAt: time.Now()}); !errors.Is(err, companion.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if rows, _ := s.store.OutreachSince(ctx, time.Time{}); len(rows) != 0 {
		t.Fatalf("a refused delivery wrote %d outreach rows", len(rows))
	}
	if msgs, _ := s.store.Messages(ctx, sid); len(msgs) != 0 {
		t.Fatalf("a refused delivery appended %d messages", len(msgs))
	}
}

func TestEnsureSessionRecreatesAndReattachesWatches(t *testing.T) {
	ctx := context.Background()
	s, _ := newTestServer(t)
	d := s.CompanionDeliverer()
	old, _ := d.EnsureSession(ctx)
	jobID, _ := s.store.CreateJob(ctx, store.Job{Kind: "cron", Spec: "0 14 * * *", Prompt: "w", Enabled: true,
		NextRun: time.Now().Add(time.Hour), OriginSessionID: old, Notify: store.NotifyEach, CheckedIn: true, Profile: "companion"})
	in, _ := s.store.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = s.store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	_, _ = s.store.ActivateInterest(ctx, in.ID, jobID)
	if _, err := s.store.DeleteSessions(ctx, []string{old}); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fresh == old {
		t.Fatal("a deleted companion session was reused")
	}
	if j, _, _ := s.store.Job(ctx, jobID); j.OriginSessionID != fresh {
		t.Fatalf("watch origin = %q, want the new session %q", j.OriginSessionID, fresh)
	}
}

// seedProposal makes a proposed interest and its pending proposal, delivered
// into the companion session.
func seedProposal(t *testing.T, s *Server) int64 {
	t.Helper()
	ctx := context.Background()
	in, _ := s.store.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)")
	_, _ = s.store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	o, err := s.CompanionDeliverer().Deliver(ctx, store.Outreach{InterestID: in.ID, Kind: store.OutreachProposal,
		Text: "watch ZS?", Payload: `{"cron":"0 14 * * 1-5","goal":"Check ZS."}`, SentAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func TestProposalRouteAnswersOnce(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	url := ts.URL + "/api/companion/proposals/" + strconv.FormatInt(id, 10)
	res := postJSON(t, url, map[string]string{"choice": "yes"})
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(body["message"], "Zscaler") {
		t.Fatalf("first press = %d %v", res.StatusCode, body)
	}
	jobs, _ := s.store.ListJobs(context.Background())
	if len(jobs) != 1 || jobs[0].Profile != "companion" {
		t.Fatalf("jobs = %+v", jobs)
	}
	res = postJSON(t, url, map[string]string{"choice": "yes"})
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second press = %d, want 409", res.StatusCode)
	}
	res = postJSON(t, url, map[string]string{"choice": "maybe"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad choice = %d, want 400", res.StatusCode)
	}
}

func TestProposalRouteWithoutCompanionIs503(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/companion/proposals/1", map[string]string{"choice": "yes"})
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}

func TestAReplyInTheCompanionSessionAnswersOutreach(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "ok"})
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	sid, _, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	res := postJSON(t, ts.URL+"/api/sessions/"+sid+"/messages", map[string]string{"text": "tell me more"})
	res.Body.Close()
	if o, _, _ := s.store.OutreachByID(ctx, id); o.Outcome != store.OutcomeReplied {
		t.Fatalf("outcome = %s, want replied", o.Outcome)
	}
}

// A watch job runs under the companion profile: the same fs_write a local
// job may make is refused.
func TestAWatchJobRunsUnderTheCompanionProfile(t *testing.T) {
	writeNote := []provider.ScriptTurn{
		{ToolCalls: []provider.Block{{Type: provider.BlockToolUse, ID: "w1", Name: "fs_write",
			Input: json.RawMessage(`{"path":"note.txt","content":"x"}`)}}},
		{Text: "done"},
	}
	const pol = `[policy]
workspace = "%WORKSPACE%"
default = "deny"
allow = ["fs_read", "fs_list", "fs_write"]
ask = ["shell_exec"]
`
	for _, tc := range []struct {
		profile string
		wrote   bool
	}{{"local", true}, {"companion", false}} {
		t.Run(tc.profile, func(t *testing.T) {
			srv, _, _ := newFullServerWithPolicy(t, pol, writeNote...)
			sid, err := srv.StartJob(t.Context(), store.Job{ID: 1, Kind: "cron", Spec: "0 9 * * *", Prompt: "write a note", Enabled: true, Profile: tc.profile})
			if err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for srv.TurnRunning(sid) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			sess, _, _ := srv.Store().Session(t.Context(), sid)
			_, statErr := os.Stat(filepath.Join(sess.Workspace, "note.txt"))
			if wrote := statErr == nil; wrote != tc.wrote {
				t.Fatalf("%s job wrote the file = %v, want %v", tc.profile, wrote, tc.wrote)
			}
		})
	}
}
