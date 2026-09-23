package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/daemon"
)

// jobRunsRes lists one job's runs, newest first, each with the first line of
// its output. Enter shows a run's whole output; o opens the run in the chat.
// It has no hotkey: it is reached from the jobs view or the sidebar.
type jobRunsRes struct{ job int64 }

func (r jobRunsRes) Name() string { return fmt.Sprintf("job %d runs", r.job) }
func (jobRunsRes) Hotkey() string { return "" }
func (jobRunsRes) Scoped() bool   { return false }
func (jobRunsRes) Columns() []Column {
	return []Column{
		{Title: "RUN", Min: 8},
		{Title: "STARTED", Min: 9},
		{Title: "STATUS", Min: 7},
		{Title: "OUTPUT", Min: 10, Flex: true},
	}
}

// Fetch ignores the session: the job is the resource's own.
func (r jobRunsRes) Fetch(ctx context.Context, v Views, _ string) ([]Row, error) {
	runs, err := v.JobRuns(ctx, r.job)
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(runs))
	for _, run := range runs {
		status := run.Status
		if run.Session.Unread {
			status += " •"
		}
		rows = append(rows, Row{
			ID:    run.Session.ID,
			Cells: []string{short(run.Session.ID), age(clock().Sub(run.Session.CreatedAt)) + " ago", status, outputPreview(run)},
			Data:  run,
		})
	}
	return rows, nil
}

func outputPreview(run daemon.JobRunJSON) string {
	switch {
	case run.Output != "":
		return oneLine(run.Output)
	case run.Status == daemon.RunRunning:
		return "…"
	}
	return "(no output)"
}

func (r jobRunsRes) Detail(row Row) string {
	run, ok := row.Data.(daemon.JobRunJSON)
	if !ok {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "job %d · run %s · %s · %s\n\n", r.job, run.Session.ID, run.Status,
		run.Session.CreatedAt.Local().Format(time.DateTime))
	switch {
	case run.Output != "":
		b.WriteString(run.Output)
	case run.Status == daemon.RunRunning:
		b.WriteString("still running")
	default:
		b.WriteString("no output: the run failed or was stopped. Press o to open it in the chat and see why.")
	}
	return b.String()
}

func (jobRunsRes) Actions() []Action { return nil }

// OpenSession is the run's session, for o.
func (jobRunsRes) OpenSession(row Row) string { return row.ID }

// jobIDOf reads a jobs-view row's job id.
func jobIDOf(r Row) (int64, bool) {
	id, err := strconv.ParseInt(r.ID, 10, 64)
	return id, err == nil
}

// Tab lights the jobs tab: a job's runs are reached from there.
func (jobRunsRes) Tab() string { return "jobs" }
