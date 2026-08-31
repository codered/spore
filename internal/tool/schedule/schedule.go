// Package schedule exposes the jobs table to the model as three tools. It
// shares one validation path with the HTTP API so a job the model creates
// and a job a human creates are indistinguishable afterwards.
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

// New returns the schedule builtins. schedule_list is allowed by default as
// a read-only tool; schedule_create and schedule_cancel are ask-gated because
// a model that can silently give itself a recurring wake-up is a model that
// can work around any per-turn limit.
func New(st *store.Store) []tool.Tool {
	return []tool.Tool{
		createTool{st: st},
		listTool{st: st},
		cancelTool{st: st},
	}
}

type createTool struct{ st *store.Store }

func (createTool) Name() string { return "schedule_create" }
func (createTool) Description() string {
	return "Schedule a prompt to run later. spec is either a five-field cron expression " +
		"(minute hour day-of-month month day-of-week, UTC) for a repeating job, or an " +
		"RFC3339 instant such as 2026-12-25T09:00:00Z for a one-off. Each run starts a " +
		"NEW session; it cannot post into this one."
}
func (createTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "spec": {"type": "string", "description": "cron expression or RFC3339 instant, UTC"},
    "prompt": {"type": "string", "description": "the prompt to run at each firing"}
  },
  "required": ["spec", "prompt"]
}`)
}
func (createTool) ReadOnly() bool { return false }

func (c createTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Spec   string `json:"spec"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	// scheduler.CreateJob is the one place that decides what a valid
	// schedule is; the HTTP API calls exactly the same function.
	job, err := scheduler.CreateJob(ctx, c.st, in.Spec, in.Prompt, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("job %d created (%s %q) — %q first runs at %s",
		job.ID, job.Kind, job.Spec, job.Prompt, job.NextRun.Format(time.RFC3339)), nil
}

type listTool struct{ st *store.Store }

func (listTool) Name() string { return "schedule_list" }
func (listTool) Description() string {
	return "List scheduled jobs. Each row shows: id, enabled/cancelled state, kind, schedule, next run time, and prompt."
}
func (listTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (listTool) ReadOnly() bool { return true }

func (l listTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	jobs, err := l.st.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "no scheduled jobs", nil
	}
	var b strings.Builder
	for _, j := range jobs {
		state := "enabled"
		if !j.Enabled {
			state = "cancelled"
		}
		fmt.Fprintf(&b, "%d\t%s\t%s\t%s\tnext %s\t%s\n",
			j.ID, state, j.Kind, j.Spec, j.NextRun.Format(time.RFC3339), j.Prompt)
	}
	return b.String(), nil
}

type cancelTool struct{ st *store.Store }

func (cancelTool) Name() string        { return "schedule_cancel" }
func (cancelTool) Description() string { return "Cancel a scheduled job by id. Cancellation is permanent; there is no resume." }
func (cancelTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "integer", "description": "the job id from schedule_list"}},
  "required": ["id"]
}`)
}
func (cancelTool) ReadOnly() bool { return false }

func (c cancelTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if err := c.st.SetJobEnabled(ctx, in.ID, false); err != nil {
		return "", err
	}
	return fmt.Sprintf("job %d cancelled", in.ID), nil
}
