package schedule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/policy"
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
	if !tools["schedule_list"].ReadOnly() {
		t.Error("schedule_list should be read-only")
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

	// schedule_list should resolve to allow
	result := engine.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: "."}, policy.Call{Tool: "schedule_list", Args: json.RawMessage(`{}`)})
	if result.Decision != policy.DecisionAllow {
		t.Errorf("schedule_list: got decision %v, want %v", result.Decision, policy.DecisionAllow)
	}

	// schedule_create and schedule_cancel should resolve to ask
	for _, name := range []string{"schedule_create", "schedule_cancel"} {
		result := engine.Evaluate(policy.Session{ID: "test", Profile: policy.ProfileLocal, Workspace: "."}, policy.Call{Tool: name, Args: json.RawMessage(`{}`)})
		if result.Decision != policy.DecisionAsk {
			t.Errorf("%s: got decision %v, want %v", name, result.Decision, policy.DecisionAsk)
		}
	}
}
