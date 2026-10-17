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

// Deleting the companion session through the API must not leave its watches
// reporting nowhere: the delete re-opens the session and re-points them.
func TestDeletingTheCompanionSessionReattachesWatches(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	d := s.CompanionDeliverer()
	old, err := d.EnsureSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jobID, _ := s.store.CreateJob(ctx, store.Job{Kind: "cron", Spec: "0 14 * * *", Prompt: "w", Enabled: true,
		NextRun: time.Now().Add(time.Hour), OriginSessionID: old, Notify: store.NotifyEach, CheckedIn: true, Profile: "companion"})
	in, _ := s.store.TouchInterest(ctx, "stock:zs", "ZS")
	_, _ = s.store.SetInterestState(ctx, in.ID, store.InterestObserving, store.InterestProposed)
	_, _ = s.store.ActivateInterest(ctx, in.ID, jobID)

	res := postJSON(t, ts.URL+"/api/sessions/delete", map[string][]string{"ids": {old}})
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d", res.StatusCode)
	}
	j, _, _ := s.store.Job(ctx, jobID)
	if j.OriginSessionID == "" || j.OriginSessionID == old {
		t.Fatalf("watch origin = %q after deleting the companion session %q; want a new session", j.OriginSessionID, old)
	}
	if sid, _, _ := s.store.CompanionState(ctx, companion.StateSessionID); sid != j.OriginSessionID {
		t.Fatalf("companion_state.session_id = %q, want %q", sid, j.OriginSessionID)
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

type noneReflector struct{ calls int }

func (r *noneReflector) Reflect(context.Context, string, string) (string, error) {
	r.calls++
	return `{"action":"none"}`, nil
}

func TestCheckRouteRunsOneTick(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t)
	s.cfg.Companion.Enabled = true
	s.cfg.Companion.Timezone = "UTC"
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	refl := &noneReflector{}
	s.AttachHeartbeat(&companion.Heartbeat{Store: s.store, Cfg: s.cfg, Recorder: companion.NewRecorder(s.store, s.cfg),
		Reflector: refl, Deliverer: s.CompanionDeliverer(), Now: time.Now})
	if _, err := s.store.TouchInterest(ctx, "stock:zs", "Zscaler (ZS)"); err != nil {
		t.Fatal(err)
	}
	// A fresh store is due its digest; mark today's sent so the plain check
	// is an ordinary tick.
	if err := s.store.SetCompanionState(ctx, companion.StateLastDigestDay, time.Now().UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	url := ts.URL + "/api/companion/check"

	res := postJSON(t, url, map[string]any{})
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || body["result"] != "nothing to decide" {
		t.Fatalf("plain check = %d %v", res.StatusCode, body)
	}

	res = postJSON(t, url, map[string]any{"promote": "stock:zs", "force": true})
	body = nil
	_ = json.NewDecoder(res.Body).Decode(&body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || body["result"] != "none" || refl.calls != 1 {
		t.Fatalf("promoted check = %d %v (calls %d)", res.StatusCode, body, refl.calls)
	}

	res = postJSON(t, url, map[string]any{"promote": "stock:nope"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown interest = %d, want 400", res.StatusCode)
	}
}

func TestCompanionCheckResearchBadRequest(t *testing.T) {
	s, ts := newTestServer(t)
	s.cfg.Companion.Enabled = true
	s.cfg.Companion.Timezone = "UTC"
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	s.AttachHeartbeat(&companion.Heartbeat{Store: s.store, Cfg: s.cfg, Recorder: companion.NewRecorder(s.store, s.cfg),
		Reflector: &noneReflector{}, Deliverer: s.CompanionDeliverer(), Now: time.Now})

	res := postJSON(t, ts.URL+"/api/companion/check", map[string]any{"research": "nope"})
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("research on an unknown interest = %d, want 400", res.StatusCode)
	}
}

func TestCheckRouteWithoutCompanionIs503(t *testing.T) {
	_, ts := newTestServer(t)
	res := postJSON(t, ts.URL+"/api/companion/check", map[string]any{})
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", res.StatusCode)
	}
}

func commandJSON(t *testing.T, url string, args []string) (int, map[string]string) {
	t.Helper()
	res := postJSON(t, url, map[string]any{"args": args})
	defer res.Body.Close()
	var body map[string]string
	_ = json.NewDecoder(res.Body).Decode(&body)
	return res.StatusCode, body
}

func TestCompanionCommandRoute(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	url := ts.URL + "/api/companion/command"
	if code, body := commandJSON(t, url, nil); code != http.StatusOK || !strings.Contains(body["text"], "companion:") {
		t.Fatalf("no args = %d %v, want status", code, body)
	}
	if code, body := commandJSON(t, url, []string{"pause", "2h"}); code != http.StatusOK || !strings.Contains(body["text"], "paused until") {
		t.Fatalf("pause = %d %v", code, body)
	}
	for _, args := range [][]string{{"check"}, {"bogus"}, {"answer", "x", "yes"}, {"answer", "1"}, {"pause", "soon"}} {
		if code, body := commandJSON(t, url, args); code != http.StatusBadRequest || body["error"] == "" {
			t.Errorf("%v = %d %v, want 400 with a message", args, code, body)
		}
	}
}

// answer goes through AnswerProposal, so its note lands in the companion
// session (and, live, in its Discord thread) exactly as a button press's.
func TestCompanionCommandAnswerPostsTheNote(t *testing.T) {
	ctx := context.Background()
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	url := ts.URL + "/api/companion/command"
	args := []string{"answer", strconv.FormatInt(id, 10), companion.ChoiceNever}
	code, body := commandJSON(t, url, args)
	if code != http.StatusOK || body["text"] == "" {
		t.Fatalf("answer = %d %v", code, body)
	}
	sid, _, _ := s.store.CompanionState(ctx, companion.StateSessionID)
	msgs, _ := s.store.Messages(ctx, sid)
	if last := msgs[len(msgs)-1]; last.Role != store.RoleNote {
		t.Fatalf("last message = %+v, want the answer's note", last)
	}
	if code, _ := commandJSON(t, url, args); code != http.StatusConflict {
		t.Fatalf("second answer = %d, want 409", code)
	}
}

func TestOpenProposalsRoute(t *testing.T) {
	s, ts := newTestServer(t)
	s.AttachCompanion(companion.NewService(s.store, s.cfg))
	id := seedProposal(t, s)
	list := func() []ProposalJSON {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/companion/proposals")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
		var out []ProposalJSON
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		if out == nil {
			t.Fatal("an empty list must be [], not null")
		}
		return out
	}
	got := list()
	if len(got) != 1 || got[0].ID != id || got[0].Text != "watch ZS?" || got[0].InterestKey != "stock:zs" || got[0].SentAt.IsZero() {
		t.Fatalf("proposals = %+v", got)
	}
	res := postJSON(t, ts.URL+"/api/companion/proposals/"+strconv.FormatInt(id, 10), map[string]string{"choice": companion.ChoiceNotNow})
	res.Body.Close()
	if got := list(); len(got) != 0 {
		t.Fatalf("after an answer = %+v, want none", got)
	}
}

func TestCompanionRoutesWithoutCompanionAre503(t *testing.T) {
	_, ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/companion/proposals")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("proposals = %d, want 503", res.StatusCode)
	}
	if code, _ := commandJSON(t, ts.URL+"/api/companion/command", []string{"status"}); code != http.StatusServiceUnavailable {
		t.Fatalf("command = %d, want 503", code)
	}
}
