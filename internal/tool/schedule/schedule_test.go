package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

func newTools(t *testing.T) (map[string]tool.Tool, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	m := map[string]tool.Tool{}
	for _, tl := range New(st) {
		m[tl.Name()] = tl
	}
	return m, st
}

func TestScheduleCreateStoresAJob(t *testing.T) {
	ctx := context.Background()
	tools, st := newTools(t)

	out, err := tools["schedule_create"].Call(ctx, json.RawMessage(
		`{"spec":"0 9 * * 1-5","prompt":"weekday briefing"}`))
	if err != nil {
		t.Fatalf("schedule_create: %v", err)
	}
	if !strings.Contains(out, "weekday briefing") {
		t.Errorf("result %q does not describe the job it created", out)
	}

	jobs, err := st.ListJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("stored %d jobs, want 1", len(jobs))
	}
	if jobs[0].Kind != "cron" || jobs[0].NextRun.IsZero() || !jobs[0].Enabled {
		t.Errorf("stored job = %+v", jobs[0])
	}
}

func TestScheduleCreateRejectsABadSpec(t *testing.T) {
	tools, st := newTools(t)
	if _, err := tools["schedule_create"].Call(context.Background(),
		json.RawMessage(`{"spec":"every tuesday-ish","prompt":"x"}`)); err == nil {
		t.Fatal("schedule_create accepted an unparseable spec")
	}
	jobs, _ := st.ListJobs(context.Background())
	if len(jobs) != 0 {
		t.Errorf("a rejected spec still stored %d jobs", len(jobs))
	}
}

func TestScheduleListAndCancel(t *testing.T) {
	ctx := context.Background()
	tools, st := newTools(t)
	tools["schedule_create"].Call(ctx, json.RawMessage(`{"spec":"0 9 * * *","prompt":"daily"}`))

	listed, err := tools["schedule_list"].Call(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("schedule_list: %v", err)
	}
	if !strings.Contains(listed, "daily") || !strings.Contains(listed, "0 9 * * *") {
		t.Errorf("listing %q does not show the job", listed)
	}

	jobs, _ := st.ListJobs(ctx)
	id := jobs[0].ID
	if _, err := tools["schedule_cancel"].Call(ctx,
		json.RawMessage(`{"id":`+strconv.FormatInt(id, 10)+`}`)); err != nil {
		t.Fatalf("schedule_cancel: %v", err)
	}
	jobs, _ = st.ListJobs(ctx)
	if jobs[0].Enabled {
		t.Error("schedule_cancel left the job enabled")
	}

	if _, err := tools["schedule_cancel"].Call(ctx, json.RawMessage(`{"id":999}`)); err == nil {
		t.Error("cancelling a job that does not exist reported success")
	}
}

func TestOnlyListIsReadOnly(t *testing.T) {
	tools, _ := newTools(t)
	for _, name := range []string{"schedule_list", "job_output"} {
		if !tools[name].ReadOnly() {
			t.Errorf("%s should be read-only", name)
		}
	}
	for _, name := range []string{"schedule_create", "schedule_cancel"} {
		if tools[name].ReadOnly() {
			t.Errorf("%s claims to be read-only; it mutates the jobs table", name)
		}
	}
}

func TestDefaultPolicyGating(t *testing.T) {
	// Verify that schedule_list is allowed by default (read-only tool)
	// while schedule_create and schedule_cancel are ask-gated (mutating tools).
	cfg := config.Default()
	engine, err := policy.NewEngine(cfg.Policy)
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}

	// schedule_list, job_output and schedule_notify should resolve to allow
	for _, name := range []string{"schedule_list", "job_output", "schedule_notify"} {
		result := engine.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: "."}, policy.Call{Tool: name, Args: json.RawMessage(`{"id":1}`)})
		if result.Decision != policy.DecisionAllow {
			t.Errorf("%s: got decision %v, want %v", name, result.Decision, policy.DecisionAllow)
		}
	}

	// schedule_create and schedule_cancel should resolve to ask
	for _, name := range []string{"schedule_create", "schedule_cancel"} {
		result := engine.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: "."}, policy.Call{Tool: name, Args: json.RawMessage(`{}`)})
		if result.Decision != policy.DecisionAsk {
			t.Errorf("%s: got decision %v, want %v", name, result.Decision, policy.DecisionAsk)
		}
	}
}

// runJob records a finished run of job id the way the scheduler and the
// agent do: a job session holding the prompt and the reply.
func runJob(t *testing.T, st *store.Store, id int64, reply string, at time.Time) string {
	t.Helper()
	ctx := context.Background()
	sid, err := st.CreateSessionFrom(ctx, "joke", "", store.SourceJob)
	if err != nil {
		t.Fatal(err)
	}
	user, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: "tell a joke"}})
	if _, err := st.AppendMessage(ctx, store.Message{SessionID: sid, Role: "user", BlocksJSON: user}); err != nil {
		t.Fatal(err)
	}
	if reply != "" {
		blocks, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: reply}})
		if _, err := st.AppendMessage(ctx, store.Message{SessionID: sid, Role: "assistant", BlocksJSON: blocks}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.MarkJobRun(ctx, id, at, at.Add(5*time.Minute), sid); err != nil {
		t.Fatal(err)
	}
	return sid
}

func createJob(t *testing.T, tools map[string]tool.Tool) int64 {
	t.Helper()
	out, err := tools["schedule_create"].Call(context.Background(), json.RawMessage(`{"spec":"*/5 * * * *","prompt":"tell a joke"}`))
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if _, err := fmt.Sscanf(out, "job %d", &id); err != nil {
		t.Fatalf("could not read the job id from %q", out)
	}
	return id
}

func TestJobOutputReturnsTheLastRunsReply(t *testing.T) {
	tools, st := newTools(t)
	id := createJob(t, tools)
	at := time.Date(2026, 9, 23, 2, 25, 0, 0, time.UTC)
	runJob(t, st, id, "an older joke", at.Add(-5*time.Minute))
	sid := runJob(t, st, id, "My grandfather died leaving me his stress.", at)

	out, err := tools["job_output"].Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%d}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2026-09-23T02:25:00Z", sid, "My grandfather died leaving me his stress."} {
		if !strings.Contains(out, want) {
			t.Errorf("job_output is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "an older joke") {
		t.Errorf("job_output returned an older run:\n%s", out)
	}
}

func TestJobOutputSaysWhenThereIsNothingToShow(t *testing.T) {
	tools, st := newTools(t)
	id := createJob(t, tools)
	out, err := tools["job_output"].Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%d}`, id)))
	if err != nil || !strings.Contains(out, "has not run yet") {
		t.Fatalf("never-run job: out=%q err=%v", out, err)
	}
	runJob(t, st, id, "", time.Now().UTC())
	out, err = tools["job_output"].Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"id":%d}`, id)))
	if err != nil || !strings.Contains(out, "no reply") {
		t.Fatalf("run without a reply: out=%q err=%v", out, err)
	}
	if _, err := tools["job_output"].Call(context.Background(), json.RawMessage(`{"id":999}`)); err == nil {
		t.Fatal("an unknown job id must be an error")
	}
}

func TestScheduleListShowsWhenEachJobLastRan(t *testing.T) {
	tools, st := newTools(t)
	ran := createJob(t, tools)
	createJob(t, tools)
	runJob(t, st, ran, "hi", time.Date(2026, 9, 23, 2, 25, 0, 0, time.UTC))
	out, err := tools["schedule_list"].Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "last 2026-09-23T02:25:00Z") || !strings.Contains(out, "last never") {
		t.Fatalf("schedule_list = %q, want each job's last run", out)
	}
}

// A job created inside a sub-agent reports to the chat the person is
// reading, which is the root of the sub-agent's tree.
func TestScheduleCreateRecordsTheRootSessionAsOrigin(t *testing.T) {
	tools, st := newTools(t)
	ctx := context.Background()
	root, _ := st.CreateSessionFrom(ctx, "chat", "", store.SourceChat)
	child, err := st.CreateChildSession(ctx, "child", "", root)
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := st.CreateChildSession(ctx, "grandchild", "", child)
	if err != nil {
		t.Fatal(err)
	}
	for _, from := range []string{root, grandchild} {
		callCtx := policy.WithSession(ctx, policy.Session{ID: from, Profile: policy.ProfileLocal})
		if _, err := tools["schedule_create"].Call(callCtx, json.RawMessage(`{"spec":"* * * * *","prompt":"p"}`)); err != nil {
			t.Fatal(err)
		}
	}
	// No session on the context: no origin.
	if _, err := tools["schedule_create"].Call(ctx, json.RawMessage(`{"spec":"* * * * *","prompt":"p"}`)); err != nil {
		t.Fatal(err)
	}
	jobs, _ := st.ListJobs(ctx)
	if len(jobs) != 3 {
		t.Fatalf("got %d jobs", len(jobs))
	}
	for i, want := range []string{root, root, ""} {
		if jobs[i].OriginSessionID != want {
			t.Errorf("job %d origin = %q, want %q", jobs[i].ID, jobs[i].OriginSessionID, want)
		}
	}
}

func TestScheduleNotifySetsTheMode(t *testing.T) {
	tools, st := newTools(t)
	ctx := context.Background()
	root, _ := st.CreateSessionFrom(ctx, "chat", "", store.SourceChat)
	callCtx := policy.WithSession(ctx, policy.Session{ID: root, Profile: policy.ProfileLocal})
	id := createJob(t, tools)
	if _, err := tools["schedule_create"].Call(callCtx, json.RawMessage(`{"spec":"* * * * *","prompt":"p"}`)); err != nil {
		t.Fatal(err)
	}
	withOrigin := id + 1

	out, err := tools["schedule_notify"].Call(ctx, json.RawMessage(fmt.Sprintf(`{"id":%d,"mode":"failures"}`, withOrigin)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "no chat") {
		t.Errorf("a job with an origin was reported as having none: %q", out)
	}
	j, _, _ := st.Job(ctx, withOrigin)
	if j.Notify != store.NotifyFailures {
		t.Errorf("notify = %q, want failures", j.Notify)
	}

	out, err = tools["schedule_notify"].Call(ctx, json.RawMessage(fmt.Sprintf(`{"id":%d,"mode":"each"}`, id)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no chat") {
		t.Errorf("a job with no origin should say its runs go only to the Jobs folder: %q", out)
	}
	for _, bad := range []string{`{"id":%d,"mode":"ask"}`, `{"id":%d,"mode":"sometimes"}`, `{"id":999%d,"mode":"none"}`} {
		if _, err := tools["schedule_notify"].Call(ctx, json.RawMessage(fmt.Sprintf(bad, id))); err == nil {
			t.Errorf("schedule_notify accepted %s", fmt.Sprintf(bad, id))
		}
	}
}
