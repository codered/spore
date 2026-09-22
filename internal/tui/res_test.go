package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/subagent"
)

func fetch(t *testing.T, r Resource, fb *fakeBackend, session string) []Row {
	t.Helper()
	rows, err := r.Fetch(context.Background(), fb, session)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestSkillRowsShowLoadedTokensAndLoadErrors(t *testing.T) {
	fb := &fakeBackend{skills: daemon.SkillsJSON{
		Skills: []daemon.SkillJSON{{Name: "debug", Description: "systematic debugging", BodyTokens: 1200, Loaded: true, Body: "# debug"}},
		Errors: []string{`gamma: unknown key "bad"`},
	}}
	rows := fetch(t, skillsRes{}, fb, "s1")
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := strings.Join(rows[0].Cells, "|"); got != "debug|✓|1.2k|systematic debugging" {
		t.Errorf("skill row = %q", got)
	}
	if rows[1].Cells[0] != "!" || !strings.Contains(rows[1].Cells[3], "gamma") {
		t.Errorf("error row = %q", rows[1].Cells)
	}
	if d := (skillsRes{}).Detail(rows[0]); !strings.Contains(d, "# debug") {
		t.Errorf("detail = %q, want the body", d)
	}
}

func TestAgentRowsShowStateAgeAndCostAndStopOnlyWhenRunning(t *testing.T) {
	started := fixedNow().Add(-90 * time.Second)
	fb := &fakeBackend{agents: daemon.AgentsJSON{Agents: []subagent.Status{
		{ID: "7f3e99aa11", Prompt: "audit the tests", State: "running", CostUSD: 0.0123, Started: started},
		{ID: "0c0c0c0c0c", Prompt: "done already", State: "done", Started: started, Ended: started.Add(30 * time.Second)},
	}}}
	rows := fetch(t, agentsRes{}, fb, "root")
	if got := strings.Join(rows[0].Cells, "|"); got != "7f3e99aa|running|1m|$0.0123|audit the tests" {
		t.Errorf("running row = %q", got)
	}
	if rows[1].Cells[2] != "30s" {
		t.Errorf("finished age = %q, want its run time, 30s", rows[1].Cells[2])
	}
	stop := (agentsRes{}).Actions()[0]
	if !stop.Applies(rows[0]) || stop.Applies(rows[1]) {
		t.Error("stop must apply only to a running sub-agent")
	}
	if err := stop.Run(context.Background(), fb, rows[0]); err != nil {
		t.Fatal(err)
	}
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "root>7f3e99aa11" {
		t.Fatalf("cancelled = %v, want root>7f3e99aa11", fb.cancelled)
	}
	if got := (agentsRes{}).Open(rows[0]); got != "7f3e99aa11" {
		t.Fatalf("enter opens %q, want the child session", got)
	}
}

func TestUsageRowsPutTheSessionFirstThenTheDays(t *testing.T) {
	fb := &fakeBackend{usage: daemon.UsageJSON{
		Session: []store.UsageRow{{Model: "a/one", Turns: 2, TokensIn: 100, TokensOut: 20, TokensCacheRead: 900, CostUSD: 0.5}},
		Days: []store.DailyUsageRow{
			{Day: "2026-09-22", UsageRow: store.UsageRow{Model: "a/one", Turns: 3, TokensIn: 1000, TokensOut: 10, CostUSD: 1}},
		},
	}}
	rows := fetch(t, usageRes{}, fb, "s1")
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if got := strings.Join(rows[0].Cells, "|"); got != "this session|a/one|2|100|20|90%|$0.5000" {
		t.Errorf("session row = %q", got)
	}
	if got := strings.Join(rows[1].Cells, "|"); got != "2026-09-22|a/one|3|1.0k|10|-|$1.0000" {
		t.Errorf("day row = %q (no cache reads shows -)", got)
	}
}

func TestJobRowsShowRelativeTimesAndCancelOnlyEnabledJobs(t *testing.T) {
	last := fixedNow().Add(-3 * time.Hour)
	fb := &fakeBackend{jobs: []daemon.JobJSON{
		{ID: 7, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning briefing", Enabled: true, NextRun: fixedNow().Add(2 * time.Hour), LastRun: &last, LastSessionID: "abcd"},
		{ID: 8, Kind: "cron", Spec: "0 18 * * *", Prompt: "evening", Enabled: false, NextRun: fixedNow()},
	}}
	rows := fetch(t, jobsRes{}, fb, "")
	if got := strings.Join(rows[0].Cells, "|"); got != "7|cron|0 9 * * *|in 2h|3h ago|morning briefing" {
		t.Errorf("enabled row = %q", got)
	}
	if rows[1].Cells[3] != "disabled" || rows[1].Cells[4] != "never" {
		t.Errorf("disabled row = %q", rows[1].Cells)
	}
	cancel := (jobsRes{}).Actions()[0]
	if !cancel.Applies(rows[0]) || cancel.Applies(rows[1]) {
		t.Error("cancel must apply only to an enabled job")
	}
	if err := cancel.Run(context.Background(), fb, rows[0]); err != nil {
		t.Fatal(err)
	}
	if len(fb.cancelledJobs) != 1 || fb.cancelledJobs[0] != 7 {
		t.Fatalf("cancelled jobs = %v", fb.cancelledJobs)
	}
	if got := (jobsRes{}).Open(rows[0]); got != "abcd" {
		t.Fatalf("enter opens %q, want the last session", got)
	}
}

func TestEveryResourceIsReachableByNameAndHotkey(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range resources() {
		if byName, ok := resourceByName(r.Name()); !ok || byName.Name() != r.Name() {
			t.Errorf("%s not found by name", r.Name())
		}
		if byKey, ok := resourceByHotkey(r.Hotkey()); !ok || byKey.Name() != r.Name() {
			t.Errorf("%s not found by hotkey %s", r.Name(), r.Hotkey())
		}
		if seen[r.Hotkey()] {
			t.Errorf("hotkey %s used twice", r.Hotkey())
		}
		seen[r.Hotkey()] = true
		flex := 0
		for _, c := range r.Columns() {
			if c.Flex {
				flex++
			}
		}
		if flex > 1 {
			t.Errorf("%s has %d flex columns, want at most one", r.Name(), flex)
		}
	}
	if len(resources()) != 4 {
		t.Fatalf("%d resources, want skills, agents, usage, jobs", len(resources()))
	}
}
