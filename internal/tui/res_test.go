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
	next, ok := (jobsRes{}).Drill(rows[0])
	if !ok || next.Name() != "job 7 runs" {
		t.Fatalf("enter opens %v, want job 7's runs", next)
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
	if len(resources()) != 8 {
		t.Fatalf("%d resources, want skills, agents, usage, jobs, refinements, mcp, policy, memory", len(resources()))
	}
}

func TestRefinementRowsProposedFirstAndActionsByStatus(t *testing.T) {
	before, after := "old", "new"
	fb := &fakeBackend{refinements: []daemon.RefinementJSON{
		{ID: 2, RoundID: "r2", SessionID: "s1", Trigger: "manual", Kind: "fact.update", Target: "style", Before: &before, After: &after, Rationale: "user said", Status: "applied", CreatedAt: fixedNow()},
		{ID: 1, RoundID: "r1", SessionID: "s2", Trigger: "idle", Kind: "notes.append", Target: "/w/.spore/agent.md", After: &after, Rationale: "lint", Status: "proposed", CreatedAt: fixedNow()},
	}}
	rows := fetch(t, refinementsRes{}, fb, "")
	if len(rows) != 2 || rows[0].ID != "1" {
		t.Fatalf("proposed rows must sort first: %+v", rows)
	}
	if rows[0].Cells[3] != "agent.md" {
		t.Errorf("notes target cell = %q, want agent.md", rows[0].Cells[3])
	}
	if d := (refinementsRes{}).Detail(rows[1]); !strings.Contains(d, "old") || !strings.Contains(d, "new") || !strings.Contains(d, "user said") {
		t.Errorf("detail = %q", d)
	}
	acts := map[string]Action{}
	for _, a := range (refinementsRes{}).Actions() {
		acts[a.Key] = a
	}
	if !acts["a"].Applies(rows[0]) || acts["a"].Applies(rows[1]) {
		t.Error("accept applies only to proposed rows")
	}
	if !acts["r"].Applies(rows[0]) || acts["r"].Applies(rows[1]) {
		t.Error("reject applies only to proposed rows")
	}
	if acts["x"].Applies(rows[0]) || !acts["x"].Applies(rows[1]) {
		t.Error("rollback applies only to applied rows")
	}
	ctx := context.Background()
	_ = acts["a"].Run(ctx, fb, rows[0])
	_ = acts["x"].Run(ctx, fb, rows[1])
	if len(fb.accepted) != 1 || fb.accepted[0] != 1 || len(fb.rolledBack) != 1 || fb.rolledBack[0] != "s1:r2" {
		t.Fatalf("accepted=%v rolledBack=%v", fb.accepted, fb.rolledBack)
	}
}

func TestMCPRowsAndToolsDrill(t *testing.T) {
	fb := &fakeBackend{mcp: []daemon.MCPServerJSON{{
		Name: "gh", Transport: "stdio", State: "up",
		Tools: []daemon.MCPToolJSON{
			{Name: "mcp__gh__search", Decision: "allow", Rule: "mcp__gh__*"},
			{Name: "mcp__gh__write", Decision: "ask", Rule: "policy.default", DependsOnArgs: true},
		},
		Skipped: []daemon.MCPSkipJSON{{Tool: "bad", Reason: "schema too large"}},
	}, {Name: "fs", Transport: "http", State: "down", LastError: "connection refused"}}}
	rows := fetch(t, mcpRes{}, fb, "")
	if got := strings.Join(rows[0].Cells, "|"); got != "gh|stdio|up|2|" {
		t.Errorf("gh row = %q", got)
	}
	if rows[1].Cells[4] != "connection refused" {
		t.Errorf("fs row = %q", rows[1].Cells)
	}
	reconnect := (mcpRes{}).Actions()[0]
	if reconnect.Key != "r" || reconnect.Confirm != nil {
		t.Error("r reconnects without asking")
	}
	_ = reconnect.Run(context.Background(), fb, rows[1])
	if len(fb.reconnected) != 1 || fb.reconnected[0] != "fs" {
		t.Errorf("reconnected = %v", fb.reconnected)
	}
	next, ok := (mcpRes{}).Drill(rows[0])
	if !ok {
		t.Fatal("enter must open the server's tools")
	}
	tools := fetch(t, next, fb, "")
	var got []string
	for _, r := range tools {
		got = append(got, strings.Join(r.Cells, "|"))
	}
	want := "search|allow|mcp__gh__*\nwrite|ask|policy.default (depends on args)\nbad|skipped|schema too large"
	if strings.Join(got, "\n") != want {
		t.Errorf("tools:\n%s\nwant:\n%s", strings.Join(got, "\n"), want)
	}
}

func TestPolicyRowsRevokeOnlyLearned(t *testing.T) {
	fb := &fakeBackend{policy: daemon.PolicyJSON{Rules: []daemon.PolicyRuleJSON{
		{Profile: "local", Decision: "deny", Rule: "fs_*(path outside workspace)", Source: "baseline"},
		{Profile: "local", Decision: "allow", Rule: "fs_read", Source: "config"},
		{Profile: "local", Decision: "deny", Rule: "probe", Source: "learned"},
		{Profile: "remote", Decision: "deny", Rule: "probe", Source: "learned"},
		{Profile: "local", Decision: "ask", Rule: "(default)", Source: "config"},
	}}}
	rows := fetch(t, policyRes{}, fb, "")
	if got := strings.Join(rows[2].Cells, "|"); got != "local|deny|learned|probe" {
		t.Errorf("learned row = %q", got)
	}
	ids := map[string]bool{}
	for _, r := range rows {
		if ids[r.ID] {
			t.Errorf("duplicate row id %q", r.ID)
		}
		ids[r.ID] = true
	}
	revoke := (policyRes{}).Actions()[0]
	if revoke.Applies(rows[0]) || revoke.Applies(rows[1]) || !revoke.Applies(rows[2]) {
		t.Error("revoke must apply only to learned rows")
	}
	if got := revoke.Confirm(rows[2]); got != `revoke "probe"?` {
		t.Errorf("confirm = %q", got)
	}
	_ = revoke.Run(context.Background(), fb, rows[2])
	if len(fb.revoked) != 1 || fb.revoked[0] != "deny probe" {
		t.Errorf("revoked = %v", fb.revoked)
	}
	if !strings.Contains((policyRes{}).Detail(rows[1]), "edit config.toml") {
		t.Error("a config row's detail must point at config.toml")
	}
}

func TestRefinementDetailShowsAPolicyRuleNotADiff(t *testing.T) {
	after := "fs_write(path matches /ws/a/**)"
	row := Row{Data: daemon.RefinementJSON{ID: 7, Kind: "policy.allow", Target: after, After: &after, Status: "proposed", Rationale: "allow fs_write on /ws/a/x"}}
	got := refinementsRes{}.Detail(row)
	if !strings.Contains(got, "rule: "+after) || strings.Contains(got, "--- before") {
		t.Errorf("detail:\n%s", got)
	}
}

func TestMemoryRowsAndSearch(t *testing.T) {
	fb := &fakeBackend{memory: daemon.MemoryJSON{Facts: []daemon.FactJSON{
		{Name: "prefers-tabs", Type: "feedback", Description: "indentation", Body: "Use tabs."},
	}}}
	rows := fetch(t, memoryRes{}, fb, "")
	if got := strings.Join(rows[0].Cells, "|"); got != "prefers-tabs|feedback|indentation" {
		t.Errorf("fact row = %q", got)
	}
	if (memoryRes{}).Detail(rows[0]) != "Use tabs." {
		t.Error("detail is the fact body")
	}
	del := (memoryRes{}).Actions()[0]
	if got := del.Confirm(rows[0]); got != `delete fact "prefers-tabs"?` {
		t.Errorf("confirm = %q", got)
	}
	_ = del.Run(context.Background(), fb, rows[0])
	if len(fb.deletedFacts) != 1 || fb.deletedFacts[0] != "prefers-tabs" {
		t.Errorf("deleted = %v", fb.deletedFacts)
	}

	fb.memory = daemon.MemoryJSON{Query: "tabs", Hits: []daemon.MemoryHitJSON{{Name: "prefers-tabs", Score: 1.5, Excerpt: "Use [tabs]."}}}
	q := memoryRes{query: "tabs"}
	if q.Name() != `memory · "tabs"` {
		t.Errorf("title = %q", q.Name())
	}
	rows = fetch(t, q, fb, "")
	if got := strings.Join(rows[0].Cells, "|"); got != "prefers-tabs|1.50|Use [tabs]." {
		t.Errorf("hit row = %q", got)
	}
	if last := fb.memoryQueries[len(fb.memoryQueries)-1]; last != "tabs" {
		t.Errorf("query sent = %q", last)
	}
}
