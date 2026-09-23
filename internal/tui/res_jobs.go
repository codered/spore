package tui

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/codered/spore/internal/daemon"
)

// jobsRes lists the scheduled jobs.
type jobsRes struct{}

func (jobsRes) Name() string   { return "jobs" }
func (jobsRes) Hotkey() string { return "J" }
func (jobsRes) Scoped() bool   { return false }
func (jobsRes) Columns() []Column {
	return []Column{
		{Title: "ID", Min: 4, Right: true},
		{Title: "KIND", Min: 5},
		{Title: "SCHEDULE", Min: 12},
		{Title: "NEXT RUN", Min: 9},
		{Title: "LAST RUN", Min: 9},
		{Title: "PROMPT", Min: 10, Flex: true},
	}
}

func (jobsRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	jobs, err := v.Jobs(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(jobs))
	for _, j := range jobs {
		next := "disabled"
		if j.Enabled {
			next = "in " + age(j.NextRun.Sub(clock()))
		}
		last := "never"
		if j.LastRun != nil {
			last = age(clock().Sub(*j.LastRun)) + " ago"
		}
		rows = append(rows, Row{
			ID:    strconv.FormatInt(j.ID, 10),
			Cells: []string{strconv.FormatInt(j.ID, 10), j.Kind, j.Spec, next, last, j.Prompt},
			Data:  j,
		})
	}
	return rows, nil
}

func (jobsRes) Detail(r Row) string {
	j, ok := r.Data.(daemon.JobJSON)
	if !ok {
		return ""
	}
	s := fmt.Sprintf("job %d (%s)\nschedule: %s\nenabled: %t\nnext run: %s\n", j.ID, j.Kind, j.Spec, j.Enabled, j.NextRun.UTC().Format(time.RFC3339))
	if j.LastRun != nil {
		s += fmt.Sprintf("last run: %s\nlast session: %s\n", j.LastRun.UTC().Format(time.RFC3339), j.LastSessionID)
	}
	return s + "\nprompt:\n" + j.Prompt
}

func (jobsRes) Actions() []Action {
	return []Action{{
		Key:   "x",
		Label: "cancel",
		Applies: func(r Row) bool {
			j, ok := r.Data.(daemon.JobJSON)
			return ok && j.Enabled
		},
		Confirm: func(r Row) string { return "cancel job " + r.ID + "?" },
		Run: func(ctx context.Context, v Views, r Row) error {
			return v.CancelJob(ctx, r.Data.(daemon.JobJSON).ID)
		},
	}}
}

// Drill opens the job's runs.
func (jobsRes) Drill(r Row) (Resource, bool) {
	id, ok := jobIDOf(r)
	if !ok {
		return nil, false
	}
	return jobRunsRes{job: id}, true
}
