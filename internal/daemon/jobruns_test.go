package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/scheduler"
)

func TestJobRunsListsEveryRunNewestFirstWithItsOutput(t *testing.T) {
	s, ts := newTestServer(t,
		provider.ScriptTurn{Text: "the first joke"},
		provider.ScriptTurn{Err: errors.New("provider timeout")},
	)
	ctx := context.Background()
	job, err := scheduler.CreateJob(ctx, s.store, "*/5 * * * *", "tell a joke", "", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	other, _ := scheduler.CreateJob(ctx, s.store, "*/5 * * * *", "another job", "", time.Now().UTC())
	first := runJob(t, s, job)
	time.Sleep(5 * time.Millisecond) // distinct created_at
	second := runJob(t, s, job)

	res, err := http.Get(ts.URL + "/api/jobs/1/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	var runs []JobRunJSON
	if err := json.NewDecoder(res.Body).Decode(&runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].Session.ID != second || runs[0].Status != RunFailed || runs[0].Output != "" {
		t.Errorf("newest run = %+v; want the failed one", runs[0])
	}
	if runs[1].Session.ID != first || runs[1].Status != RunOK || runs[1].Output != "the first joke" {
		t.Errorf("older run = %+v; want the ok one with its output", runs[1])
	}

	res2, _ := http.Get(ts.URL + "/api/jobs/" + strconv.FormatInt(other.ID, 10) + "/runs")
	var none []JobRunJSON
	_ = json.NewDecoder(res2.Body).Decode(&none)
	res2.Body.Close()
	if none == nil || len(none) != 0 {
		t.Errorf("a job that never ran should list [] not %v", none)
	}

	res3, _ := http.Get(ts.URL + "/api/jobs/99/runs")
	res3.Body.Close()
	if res3.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job gave %d, want 404", res3.StatusCode)
	}
}
