"""Task 03 — four resources: skills, agents, usage, jobs.

Source plan: Task 3. One component (internal/tui). The resources read the
daemon's JSON types through the Views seam Task 01 proved; the fake backend
here stands in for the adapter only in unit tests.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import gate

import plan_lib
from plan_lib import One, go, gotest, lines, offset, todo

KIND = "scripted"
TIER = "T1"
TOUCHES = ["internal/tui"]
CROSSES = []

FAKE_FIELDS = """
    skills        daemon.SkillsJSON
    agents        daemon.AgentsJSON
    jobs          []daemon.JobJSON
    usage         daemon.UsageJSON
    viewErr       error
    fetches       int
    cancelledJobs []int64
"""

FAKE_METHODS = """
func (f *fakeBackend) fetched() {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.fetches++
}
func (f *fakeBackend) Skills(context.Context, string) (daemon.SkillsJSON, error) {
    f.fetched()
    return f.skills, f.viewErr
}
func (f *fakeBackend) Agents(context.Context, string) (daemon.AgentsJSON, error) {
    f.fetched()
    return f.agents, f.viewErr
}
func (f *fakeBackend) Jobs(context.Context) ([]daemon.JobJSON, error) {
    f.fetched()
    return f.jobs, f.viewErr
}
func (f *fakeBackend) Usage(context.Context, string) (daemon.UsageJSON, error) {
    f.fetched()
    return f.usage, f.viewErr
}
func (f *fakeBackend) CancelJob(_ context.Context, id int64) error {
    f.mu.Lock()
    defer f.mu.Unlock()
    f.cancelledJobs = append(f.cancelledJobs, id)
    return nil
}
"""

RES_TEST = """
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
"""

REGISTRY = """
package tui

import (
    "fmt"
    "time"
)

// resources is every view in hotkey order. The header's hint grid, the :
// command completion and the hotkeys all come from it.
func resources() []Resource {
    return []Resource{skillsRes{}, agentsRes{}, usageRes{}, jobsRes{}}
}

func resourceByName(name string) (Resource, bool) {
    for _, r := range resources() {
        if r.Name() == name {
            return r, true
        }
    }
    return nil, false
}

func resourceByHotkey(key string) (Resource, bool) {
    for _, r := range resources() {
        if r.Hotkey() == key {
            return r, true
        }
    }
    return nil, false
}

// age renders a duration the way k9s does: 45s, 12m, 3h, 2d.
func age(d time.Duration) string {
    if d < 0 {
        d = -d
    }
    switch {
    case d < time.Minute:
        return fmt.Sprintf("%ds", int(d.Seconds()))
    case d < time.Hour:
        return fmt.Sprintf("%dm", int(d.Minutes()))
    case d < 48*time.Hour:
        return fmt.Sprintf("%dh", int(d.Hours()))
    }
    return fmt.Sprintf("%dd", int(d.Hours()/24))
}
"""

SKILLS = """
package tui

import (
    "context"
    "fmt"

    "github.com/codered/spore/internal/daemon"
)

// skillsRes lists the skills available to the selected session.
type skillsRes struct{}

func (skillsRes) Name() string   { return "skills" }
func (skillsRes) Hotkey() string { return "S" }
func (skillsRes) Scoped() bool   { return true }
func (skillsRes) Columns() []Column {
    return []Column{
        {Title: "NAME", Min: 16},
        {Title: "LOADED", Min: 6},
        {Title: "TOKENS", Min: 6, Right: true},
        {Title: "DESCRIPTION", Min: 10, Flex: true},
    }
}

func (skillsRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
    list, err := v.Skills(ctx, sessionID)
    if err != nil {
        return nil, err
    }
    rows := make([]Row, 0, len(list.Skills)+len(list.Errors))
    for _, s := range list.Skills {
        loaded := ""
        if s.Loaded {
            loaded = "✓"
        }
        rows = append(rows, Row{ID: s.Name, Cells: []string{s.Name, loaded, humanTokens(s.BodyTokens), s.Description}, Data: s})
    }
    for i, e := range list.Errors {
        rows = append(rows, Row{ID: fmt.Sprintf("!%d", i), Cells: []string{"!", "", "", e}, Data: e})
    }
    return rows, nil
}

func (skillsRes) Detail(r Row) string {
    switch d := r.Data.(type) {
    case daemon.SkillJSON:
        return d.Name + "\\n" + d.Description + "\\n\\n" + d.Body
    case string:
        return "load error\\n\\n" + d
    }
    return ""
}

func (skillsRes) Actions() []Action { return nil }
"""

AGENTS = """
package tui

import (
    "context"
    "fmt"
    "strings"
    "time"

    "github.com/codered/spore/internal/subagent"
)

// agentsRes lists the sub-agents the selected session launched.
type agentsRes struct{}

// agentRow carries the parent a stop must be sent through.
type agentRow struct {
    subagent.Status
    parent string
}

func (agentsRes) Name() string   { return "agents" }
func (agentsRes) Hotkey() string { return "A" }
func (agentsRes) Scoped() bool   { return true }
func (agentsRes) Columns() []Column {
    return []Column{
        {Title: "ID", Min: 8},
        {Title: "STATE", Min: 11},
        {Title: "AGE", Min: 4, Right: true},
        {Title: "COST", Min: 8, Right: true},
        {Title: "PROMPT", Min: 10, Flex: true},
    }
}

func (agentsRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
    list, err := v.Agents(ctx, sessionID)
    if err != nil {
        return nil, err
    }
    rows := make([]Row, 0, len(list.Agents))
    for _, a := range list.Agents {
        ran := clock().Sub(a.Started)
        if !a.Ended.IsZero() {
            ran = a.Ended.Sub(a.Started)
        }
        id := a.ID
        if len(id) > 8 {
            id = id[:8]
        }
        rows = append(rows, Row{
            ID:    a.ID,
            Cells: []string{id, a.State, age(ran), fmt.Sprintf("$%.4f", a.CostUSD), a.Prompt},
            Data:  agentRow{Status: a, parent: sessionID},
        })
    }
    return rows, nil
}

func (agentsRes) Detail(r Row) string {
    a, ok := r.Data.(agentRow)
    if !ok {
        return ""
    }
    var b strings.Builder
    fmt.Fprintf(&b, "sub-agent %s\\nstate: %s\\ncost: $%.4f\\nstarted: %s\\n", a.ID, a.State, a.CostUSD, a.Started.UTC().Format(time.RFC3339))
    if !a.Ended.IsZero() {
        fmt.Fprintf(&b, "ended: %s\\n", a.Ended.UTC().Format(time.RFC3339))
    }
    fmt.Fprintf(&b, "\\nprompt:\\n%s\\n", a.Prompt)
    if a.Result != "" {
        fmt.Fprintf(&b, "\\nresult:\\n%s\\n", a.Result)
    }
    if a.Error != "" {
        fmt.Fprintf(&b, "\\nerror:\\n%s\\n", a.Error)
    }
    return b.String()
}

func (agentsRes) Actions() []Action {
    return []Action{{
        Key:   "x",
        Label: "stop",
        Applies: func(r Row) bool {
            a, ok := r.Data.(agentRow)
            return ok && a.State == "running"
        },
        Confirm: func(r Row) string { return "stop sub-agent " + short(r.ID) + "?" },
        Run: func(ctx context.Context, v Views, r Row) error {
            a := r.Data.(agentRow)
            return v.CancelAgent(ctx, a.parent, a.ID)
        },
    }}
}

// Open selects the child's own session.
func (agentsRes) Open(r Row) string { return r.ID }
"""

USAGE = """
package tui

import (
    "context"
    "fmt"

    "github.com/codered/spore/internal/store"
)

// usageRes shows the selected session's totals, then every session's per day.
type usageRes struct{}

func (usageRes) Name() string   { return "usage" }
func (usageRes) Hotkey() string { return "U" }
func (usageRes) Scoped() bool   { return true }
func (usageRes) Columns() []Column {
    return []Column{
        {Title: "DAY", Min: 12},
        {Title: "MODEL", Min: 12, Flex: true},
        {Title: "TURNS", Min: 5, Right: true},
        {Title: "IN", Min: 6, Right: true},
        {Title: "OUT", Min: 6, Right: true},
        {Title: "CACHE %", Min: 7, Right: true},
        {Title: "COST", Min: 9, Right: true},
    }
}

func (usageRes) Fetch(ctx context.Context, v Views, sessionID string) ([]Row, error) {
    u, err := v.Usage(ctx, sessionID)
    if err != nil {
        return nil, err
    }
    rows := make([]Row, 0, len(u.Session)+len(u.Days))
    for _, r := range u.Session {
        rows = append(rows, Row{ID: "session/" + r.Model, Cells: usageCells("this session", r), Data: r})
    }
    for _, d := range u.Days {
        rows = append(rows, Row{ID: "day/" + d.Day + "/" + d.Model, Cells: usageCells(d.Day, d.UsageRow), Data: d.UsageRow})
    }
    return rows, nil
}

func usageCells(day string, r store.UsageRow) []string {
    return []string{day, r.Model, fmt.Sprint(r.Turns), humanTokens(r.TokensIn), humanTokens(r.TokensOut),
        cachePct(r), fmt.Sprintf("$%.4f", r.CostUSD)}
}

// cachePct is the share of input served from the prompt cache. Since caching
// shipped TokensIn is only the uncached remainder, so the whole input is the
// sum of all three.
func cachePct(r store.UsageRow) string {
    total := r.TokensIn + r.TokensCacheRead + r.TokensCacheWrite
    if r.TokensCacheRead == 0 || total == 0 {
        return "-"
    }
    return fmt.Sprintf("%d%%", r.TokensCacheRead*100/total)
}

func (usageRes) Detail(r Row) string {
    u, ok := r.Data.(store.UsageRow)
    if !ok {
        return ""
    }
    return fmt.Sprintf("%s\\n\\nturns: %d\\ntokens in (uncached): %d\\ntokens out: %d\\ncache read: %d\\ncache write: %d\\ncache hit: %s\\ncost: $%.4f",
        u.Model, u.Turns, u.TokensIn, u.TokensOut, u.TokensCacheRead, u.TokensCacheWrite, cachePct(u), u.CostUSD)
}

func (usageRes) Actions() []Action { return nil }
"""

JOBS = """
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
    s := fmt.Sprintf("job %d (%s)\\nschedule: %s\\nenabled: %t\\nnext run: %s\\n", j.ID, j.Kind, j.Spec, j.Enabled, j.NextRun.UTC().Format(time.RFC3339))
    if j.LastRun != nil {
        s += fmt.Sprintf("last run: %s\\nlast session: %s\\n", j.LastRun.UTC().Format(time.RFC3339), j.LastSessionID)
    }
    return s + "\\nprompt:\\n" + j.Prompt
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

// Open selects the session the job last ran in.
func (jobsRes) Open(r Row) string {
    if j, ok := r.Data.(daemon.JobJSON); ok {
        return j.LastSessionID
    }
    return ""
}
"""

NEW_FILES = {
    "internal/tui/res_registry.go": REGISTRY,
    "internal/tui/res_skills.go": SKILLS,
    "internal/tui/res_agents.go": AGENTS,
    "internal/tui/res_usage.go": USAGE,
    "internal/tui/res_jobs.go": JOBS,
}


def apply_tests():
    # Step 1: pin the clock for every test.
    if todo("internal/tui/block_test.go", "\tclock = fixedNow"):
        with One("internal/tui/block_test.go") as ed:
            ed.insert_before(ed.locate("\tos.Exit(m.Run())"), ["\tclock = fixedNow"])
    # Step 2: the fake backend implements Views. The new fields go in their
    # own blank-line-separated group so gofmt's alignment of the old ones holds.
    if todo("internal/tui/app_test.go", "\tcancelledJobs []int64"):
        with One("internal/tui/app_test.go") as ed:
            ed.insert_after(ed.locate("\ttranscripts map[string]daemon.TranscriptJSON"), [""] + lines(FAKE_FIELDS))
    if todo("internal/tui/app_test.go", "func (f *fakeBackend) fetched() {"):
        with One("internal/tui/app_test.go") as ed:
            ret = ed.locate('\treturn "did " + cmd, nil')
            ed.insert_after(offset(ed, ret, 1, "}"), lines(FAKE_METHODS))
    # Step 3: the resource tests.
    taskkit.create_file("internal/tui/res_test.go", go(RES_TEST))


def apply_impl():
    for path, src in NEW_FILES.items():
        taskkit.create_file(path, go(src))


def apply():
    apply_tests()
    apply_impl()


RED = [
    ("the resource tests need the four resources",
     gotest("./internal/tui/", run="SkillRows|AgentRows|UsageRows|JobRows|EveryResource", race=False),
     ["undefined: skillsRes"]),
]


def verify():
    gate.structural("TestMain pins the clock", lambda: taskkit.file_contains("internal/tui/block_test.go", "\tclock = fixedNow\n\tos.Exit(m.Run())"))
    gate.structural("the fake backend implements Views",
                    lambda: taskkit.file_contains("internal/tui/app_test.go", "func (f *fakeBackend) CancelJob(_ context.Context, id int64) error {"))
    for path in NEW_FILES:
        gate.structural(path + " exists", lambda p=path: os.path.exists(p))
    gate.structural("gofmt clean", lambda: plan_lib.gofmt_clean(
        "internal/tui/block_test.go", "internal/tui/app_test.go", "internal/tui/res_test.go", *NEW_FILES))
    gate.component("the five resource tests pass",
                   gotest("./internal/tui/", run="SkillRows|AgentRows|UsageRows|JobRows|EveryResource", count=1))
    gate.component("tui tests, race", gotest("./internal/tui/"))


if __name__ == "__main__":
    if "--red" in sys.argv:
        apply_tests()
        raise SystemExit(plan_lib.red(RED))
    raise SystemExit(gate.run(apply, verify))
