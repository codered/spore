// Package schedule exposes the jobs table to the model as five tools. It
// shares one validation path with the HTTP API so a job the model creates
// and a job a human creates are indistinguishable afterwards.
package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/scheduler"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/tool"
)

// New returns the schedule builtins. schedule_list and job_output are allowed
// by default as read-only tools, and schedule_notify because it only changes
// what spore says in the chat the user is already in; schedule_create and
// schedule_cancel are ask-gated because
// a model that can silently give itself a recurring wake-up is a model that
// can work around any per-turn limit.
func New(st *store.Store) []tool.Tool {
	return []tool.Tool{
		createTool{st: st},
		listTool{st: st},
		cancelTool{st: st},
		outputTool{st: st},
		notifyTool{st: st},
	}
}

type createTool struct{ st *store.Store }

func (createTool) Name() string { return "schedule_create" }
func (createTool) Description() string {
	return "Schedule a prompt to run later. spec is either a five-field cron expression " +
		"(minute hour day-of-month month day-of-week, UTC) for a repeating job, or an " +
		"RFC3339 instant such as 2026-12-25T09:00:00Z for a one-off. Each run starts a " +
		"NEW session. After its first successful run spore reports back in this chat and " +
		"asks how the user wants to hear about later runs; record the answer with " +
		"schedule_notify. Read a run's reply later with job_output."
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
	// The chat this runs in is the job's origin: the one place its runs
	// are reported.
	origin := policy.SessionFrom(ctx).ID
	job, err := scheduler.CreateJob(ctx, c.st, in.Spec, in.Prompt, origin, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("job %d created (%s %q) — %q first runs at %s",
		job.ID, job.Kind, job.Spec, job.Prompt, job.NextRun.Format(time.RFC3339)), nil
}

type listTool struct{ st *store.Store }

func (listTool) Name() string { return "schedule_list" }
func (listTool) Description() string {
	return "List scheduled jobs. Each row shows: id, enabled/cancelled state, kind, schedule, next run time, " +
		"last run time, how later runs are reported (notify), and prompt."
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
		last := "never"
		if !j.LastRun.IsZero() {
			last = j.LastRun.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "%d\t%s\t%s\t%s\tnext %s\tlast %s\tnotify %s\t%s\n",
			j.ID, state, j.Kind, j.Spec, j.NextRun.Format(time.RFC3339), last, j.Notify, j.Prompt)
	}
	return b.String(), nil
}

type cancelTool struct{ st *store.Store }

func (cancelTool) Name() string { return "schedule_cancel" }
func (cancelTool) Description() string {
	return "Cancel a scheduled job by id. Cancellation is permanent; there is no resume."
}
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

type outputTool struct{ st *store.Store }

func (outputTool) Name() string { return "job_output" }
func (outputTool) Description() string {
	return "Show a scheduled job's most recent run: when it ran, the session it ran in, and its " +
		"final reply. Use it when the user asks what a job said or did last time; get ids from schedule_list."
}
func (outputTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {"id": {"type": "integer", "description": "the job id from schedule_list"}},
  "required": ["id"]
}`)
}
func (outputTool) ReadOnly() bool { return true }

func (o outputTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	jobs, err := o.st.ListJobs(ctx)
	if err != nil {
		return "", err
	}
	var job *store.Job
	for i := range jobs {
		if jobs[i].ID == in.ID {
			job = &jobs[i]
		}
	}
	if job == nil {
		return "", fmt.Errorf("no job %d; schedule_list shows the ids", in.ID)
	}
	if job.LastSessionID == "" || job.LastRun.IsZero() {
		return fmt.Sprintf("job %d has not run yet; its next run is %s", job.ID, job.NextRun.UTC().Format(time.RFC3339)), nil
	}
	head := fmt.Sprintf("job %d (%s %q) last ran %s in session %s",
		job.ID, job.Kind, job.Spec, job.LastRun.UTC().Format(time.RFC3339), job.LastSessionID)
	reply, err := LastReply(ctx, o.st, job.LastSessionID)
	if err != nil {
		return "", err
	}
	if reply == "" {
		return head + " and left no reply: the run failed, was stopped, or is still going.", nil
	}
	return head + ". Its reply:\n\n" + reply, nil
}

type notifyTool struct{ st *store.Store }

func (notifyTool) Name() string { return "schedule_notify" }
func (notifyTool) Description() string {
	return "Set how the chat that created a job hears about its later runs: each (a short note " +
		"after every run), failures (a note only when a run fails) or none. Use it when the user " +
		"answers the first-run check-in, or asks to change it. Runs always land in the Jobs folder."
}
func (notifyTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "id": {"type": "integer", "description": "the job id from schedule_list"},
    "mode": {"type": "string", "enum": ["each", "failures", "none"]}
  },
  "required": ["id", "mode"]
}`)
}
func (notifyTool) ReadOnly() bool { return false }

func (n notifyTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ID   int64  `json:"id"`
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("bad arguments: %w", err)
	}
	if err := n.st.SetJobNotify(ctx, in.ID, in.Mode); err != nil {
		return "", err
	}
	job, _, err := n.st.Job(ctx, in.ID)
	if err != nil {
		return "", err
	}
	if job.OriginSessionID == "" {
		return fmt.Sprintf("job %d notify set to %s, but it has no chat to report to: its runs go only to the Jobs folder", in.ID, in.Mode), nil
	}
	return fmt.Sprintf("job %d notify set to %s", in.ID, in.Mode), nil
}

// LastReply is the text of the session's last assistant message.
func LastReply(ctx context.Context, st *store.Store, sessionID string) (string, error) {
	msgs, err := st.Messages(ctx, sessionID)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != string(provider.RoleAssistant) {
			continue
		}
		var blocks []provider.Block
		if err := json.Unmarshal(msgs[i].BlocksJSON, &blocks); err != nil {
			return "", fmt.Errorf("read the run's reply: %w", err)
		}
		var parts []string
		for _, b := range blocks {
			if b.Type == provider.BlockText && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), nil
		}
	}
	return "", nil
}
