"""Task 01 — the view data path, store -> daemon HTTP -> cmd/spore adapter.

Source plan: Task 1 (daemon) plus the Views interface (Task 2 Step 1) and the
adapter methods (Task 6 Step 2), moved forward so the first task already
crosses every seam the views read through. The adapter is proven against a
real daemon here, not at the end.
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

import taskkit
from taskkit import gate

import plan_lib
from plan_lib import One, append_go, go, gotest, lines, offset, todo

KIND = "scripted"
TIER = "T2"
TOUCHES = ["internal/store", "internal/daemon", "internal/tui", "cmd/spore"]
CROSSES = ["store -> daemon (DailyUsage)", "daemon HTTP -> adapter (/api/usage, ?body=1, /api/jobs)", "adapter -> tui (Views)"]

# ------------------------------------------------------------------ tests --

STORE_TEST = """
func TestDailyUsageGroupsByDayAndModelInsideTheWindow(t *testing.T) {
    st := openTestStore(t)
    ctx := context.Background()
    id, err := st.CreateSession(ctx, "t", "/ws")
    if err != nil {
        t.Fatal(err)
    }
    add := func(model string, in, out, read int, cost float64, at time.Time) {
        t.Helper()
        mid, err := st.AppendMessage(ctx, Message{
            SessionID: id, Role: "assistant", BlocksJSON: []byte(`[]`), Model: model,
            TokensIn: in, TokensOut: out, TokensCacheRead: read, CostUSD: cost,
        })
        if err != nil {
            t.Fatal(err)
        }
        if _, err := st.db.ExecContext(ctx, `UPDATE messages SET created_at = ? WHERE id = ?`,
            at.UTC().Format(timeFormat), mid); err != nil {
            t.Fatal(err)
        }
    }
    now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
    add("a/one", 10, 1, 90, 0.01, now)
    add("a/one", 20, 2, 0, 0.02, now.Add(-time.Hour))
    add("b/two", 5, 5, 0, 0.05, now.Add(-24*time.Hour))
    add("a/one", 99, 99, 0, 9.99, now.Add(-40*24*time.Hour)) // outside the window

    rows, err := st.DailyUsage(ctx, now.Add(-30*24*time.Hour))
    if err != nil {
        t.Fatal(err)
    }
    if len(rows) != 2 {
        t.Fatalf("rows = %+v, want two (one per day in the window)", rows)
    }
    if r := rows[0]; r.Day != "2026-09-22" || r.Model != "a/one" || r.Turns != 2 || r.TokensIn != 30 || r.TokensCacheRead != 90 {
        t.Errorf("first row = %+v, want 2026-09-22 a/one with 2 turns, 30 in, 90 cache read", r)
    }
    if r := rows[1]; r.Day != "2026-09-21" || r.Model != "b/two" || r.Turns != 1 {
        t.Errorf("second row = %+v, want 2026-09-21 b/two with 1 turn", r)
    }
}
"""

DAEMON_USAGE_TEST = """
package daemon

import (
    "encoding/json"
    "net/http"
    "testing"

    "github.com/codered/spore/internal/provider"
)

func getUsage(t *testing.T, url string) UsageJSON {
    t.Helper()
    res, err := http.Get(url)
    if err != nil {
        t.Fatal(err)
    }
    defer res.Body.Close()
    if res.StatusCode != http.StatusOK {
        t.Fatalf("GET %s: %s", url, res.Status)
    }
    var out UsageJSON
    if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
        t.Fatal(err)
    }
    return out
}

func TestUsageReportsTheSessionAndTheDays(t *testing.T) {
    _, ts := newTestServer(t, provider.ScriptTurn{
        Text: "hi", Usage: provider.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 30},
    })
    id := createTestSession(t, ts.URL)
    body := attachStream(t, ts, id)
    res := postJSON(t, ts.URL+"/api/sessions/"+id+"/messages", map[string]string{"text": "go"})
    res.Body.Close()
    readSSE(t, body, 3) // turn_started, text, turn_done

    u := getUsage(t, ts.URL+"/api/usage?session="+id)
    if len(u.Session) != 1 || u.Session[0].Turns != 1 || u.Session[0].TokensIn != 10 || u.Session[0].TokensCacheRead != 30 {
        t.Fatalf("session = %+v, want one model row with 1 turn, 10 in, 30 cache read", u.Session)
    }
    if len(u.Days) != 1 || u.Days[0].Turns != 1 || u.Days[0].Day == "" {
        t.Fatalf("days = %+v, want one row for today", u.Days)
    }
}

func TestUsageWithoutASessionStillReportsTheDays(t *testing.T) {
    _, ts := newTestServer(t)
    u := getUsage(t, ts.URL+"/api/usage")
    if u.Session == nil || u.Days == nil {
        t.Fatalf("usage = %+v, want empty arrays, not null", u)
    }
}

func TestUsageForAnUnknownSessionIsNotFound(t *testing.T) {
    _, ts := newTestServer(t)
    res, err := http.Get(ts.URL + "/api/usage?session=nope")
    if err != nil {
        t.Fatal(err)
    }
    res.Body.Close()
    if res.StatusCode != http.StatusNotFound {
        t.Fatalf("status = %d, want 404", res.StatusCode)
    }
}
"""

SKILLS_TEST = """
func TestSkillsIncludeTheirBodyOnlyWhenAsked(t *testing.T) {
    s, ts := newTestServer(t)
    dir := t.TempDir()
    if err := skill.Write(dir, skill.Skill{Name: "alpha", Description: "the alpha skill", Body: "do alpha things"}); err != nil {
        t.Fatal(err)
    }
    s.agent.Skills = skill.NewCaches()
    s.cfg.Skills.Dir = dir
    sid, err := s.store.CreateSession(context.Background(), "test", "")
    if err != nil {
        t.Fatal(err)
    }

    fetch := func(q string) SkillsJSON {
        res, err := http.Get(ts.URL + "/api/sessions/" + sid + "/skills" + q)
        if err != nil {
            t.Fatal(err)
        }
        defer res.Body.Close()
        var out SkillsJSON
        if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
            t.Fatal(err)
        }
        return out
    }
    if got := fetch(""); len(got.Skills) != 1 || got.Skills[0].Body != "" {
        t.Fatalf("without ?body=1 = %+v, want no body", got.Skills)
    }
    if got := fetch("?body=1"); len(got.Skills) != 1 || got.Skills[0].Body == "" {
        t.Fatalf("with ?body=1 = %+v, want the body", got.Skills)
    }
}
"""

# New in this plan: the crossing gate. The adapter talks to a real daemon over
# real HTTP, so every Views method is proven through the seam it reads.
ADAPTER_TESTS = """
func TestTheAdapterReadsTheViewsFromARealDaemon(t *testing.T) {
    ws := t.TempDir()
    c := e2eDaemon(t, ws, provider.ScriptTurn{Text: "hi there", Usage: provider.Usage{InputTokens: 10, OutputTokens: 2}})
    ctx := context.Background()
    sid, err := c.createSession(ctx, "chat", ws)
    if err != nil {
        t.Fatal(err)
    }
    be := tuiBackend{c: c}
    if err := be.Send(ctx, sid, "hello"); err != nil {
        t.Fatal(err)
    }
    // The turn runs in the background; its usage lands when the reply is stored.
    var u daemon.UsageJSON
    for deadline := time.Now().Add(5 * time.Second); ; {
        if u, err = be.Usage(ctx, sid); err != nil {
            t.Fatal(err)
        }
        if len(u.Session) == 1 || time.Now().After(deadline) {
            break
        }
        time.Sleep(20 * time.Millisecond)
    }
    if len(u.Session) != 1 || u.Session[0].Model != "script/fake" || u.Session[0].TokensIn != 10 {
        t.Fatalf("session usage = %+v, want one script/fake row with 10 in", u.Session)
    }
    if len(u.Days) != 1 || u.Days[0].Turns != 1 {
        t.Fatalf("days = %+v, want today's one turn", u.Days)
    }
    if _, err := be.Skills(ctx, sid); err != nil {
        t.Fatalf("skills: %v", err)
    }
    if _, err := be.Agents(ctx, sid); err != nil {
        t.Fatalf("agents: %v", err)
    }
    var job daemon.JobJSON
    if err := c.do(ctx, "POST", "/api/jobs", map[string]string{"spec": "0 9 * * *", "prompt": "morning briefing"}, &job); err != nil {
        t.Fatal(err)
    }
    if err := be.CancelJob(ctx, job.ID); err != nil {
        t.Fatal(err)
    }
    jobs, err := be.Jobs(ctx)
    if err != nil {
        t.Fatal(err)
    }
    if len(jobs) != 1 || jobs[0].ID != job.ID || jobs[0].Enabled {
        t.Fatalf("jobs = %+v, want the one job, disabled", jobs)
    }
}

func TestAViewAgainstADaemonWithoutTheRouteSaysSo(t *testing.T) {
    ts := httptest.NewServer(http.NotFoundHandler())
    t.Cleanup(ts.Close)
    be := tuiBackend{c: newClient(strings.TrimPrefix(ts.URL, "http://"))}
    if _, err := be.Usage(context.Background(), ""); !errors.Is(err, tui.ErrOlderDaemon) {
        t.Fatalf("err = %v, want tui.ErrOlderDaemon", err)
    }
}
"""

# ------------------------------------------------------------ production --

STORE_IMPL = """
// DailyUsageRow is one model's consumption on one UTC day.
type DailyUsageRow struct {
    Day string `json:"day"` // YYYY-MM-DD, UTC
    UsageRow
}

// DailyUsage totals usage per UTC day and model for messages created at or
// after since, newest day first. created_at is a fixed-width UTC timestamp, so
// its first ten characters are the day and string comparison orders it.
func (s *Store) DailyUsage(ctx context.Context, since time.Time) ([]DailyUsageRow, error) {
    rows, err := s.db.QueryContext(ctx,
        `SELECT substr(created_at, 1, 10), model, count(*), sum(tokens_in), sum(tokens_out),
           sum(tokens_cache_write), sum(tokens_cache_read), sum(cost_usd)
         FROM messages
         WHERE (tokens_in > 0 OR tokens_out > 0 OR cost_usd > 0) AND created_at >= ?
         GROUP BY 1, 2 ORDER BY 1 DESC, 2`,
        since.UTC().Format(timeFormat))
    if err != nil {
        return nil, err
    }
    defer func() { _ = rows.Close() }()
    var out []DailyUsageRow
    for rows.Next() {
        var r DailyUsageRow
        if err := rows.Scan(&r.Day, &r.Model, &r.Turns, &r.TokensIn, &r.TokensOut,
            &r.TokensCacheWrite, &r.TokensCacheRead, &r.CostUSD); err != nil {
            return nil, err
        }
        out = append(out, r)
    }
    return out, rows.Err()
}
"""

DAEMON_USAGE = """
package daemon

import (
    "net/http"
    "time"

    "github.com/codered/spore/internal/store"
)

// usageWindow is how far back the per-day usage reaches.
const usageWindow = 30 * 24 * time.Hour

// UsageJSON is GET /api/usage: the named session's totals per model, and every
// session's totals per UTC day and model over the last usageWindow.
type UsageJSON struct {
    Session []store.UsageRow      `json:"session"`
    Days    []store.DailyUsageRow `json:"days"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
    out := UsageJSON{Session: []store.UsageRow{}, Days: []store.DailyUsageRow{}}
    if id := r.URL.Query().Get("session"); id != "" {
        if _, ok := s.findSession(w, r, id); !ok {
            return
        }
        rows, err := s.store.SessionUsage(r.Context(), id)
        if err != nil {
            writeError(w, http.StatusInternalServerError, "session usage: %v", err)
            return
        }
        if rows != nil {
            out.Session = rows
        }
    }
    days, err := s.store.DailyUsage(r.Context(), time.Now().Add(-usageWindow))
    if err != nil {
        writeError(w, http.StatusInternalServerError, "daily usage: %v", err)
        return
    }
    if days != nil {
        out.Days = days
    }
    writeJSON(w, http.StatusOK, out)
}
"""

TUI_VIEWS = """
package tui

import (
    "context"
    "errors"

    "github.com/codered/spore/internal/daemon"
)

// ErrOlderDaemon is what a Views method returns when the daemon has no route
// for it: the daemon predates the TUI. The view says so instead of looking
// empty.
var ErrOlderDaemon = errors.New("this daemon is older than the TUI — restart it")

// Views is the daemon as the resource views see it. It sits beside Backend so
// the chat screen's interface stays small; cmd/spore's adapter implements
// both, and New picks it up from the Backend it is given.
type Views interface {
    Skills(ctx context.Context, sessionID string) (daemon.SkillsJSON, error)
    Agents(ctx context.Context, sessionID string) (daemon.AgentsJSON, error)
    Jobs(ctx context.Context) ([]daemon.JobJSON, error)
    Usage(ctx context.Context, sessionID string) (daemon.UsageJSON, error)
    CancelAgent(ctx context.Context, parent, child string) error
    CancelJob(ctx context.Context, id int64) error
}
"""

ADAPTER_IMPL = """
var _ tui.Views = tuiBackend{}

// viewErr maps a missing route to tui.ErrOlderDaemon. The daemon's mux answers
// an unknown route with a plain-text 404, which client.do reports as
// "<METHOD> <path>: 404 Not Found"; a known route's own 404 (an unknown
// session) carries a JSON error message instead, and passes through.
func viewErr(err error) error {
    if err != nil && strings.HasSuffix(err.Error(), ": 404 Not Found") {
        return tui.ErrOlderDaemon
    }
    return err
}

func (b tuiBackend) Skills(ctx context.Context, sessionID string) (daemon.SkillsJSON, error) {
    var out daemon.SkillsJSON
    err := b.c.do(ctx, "GET", "/api/sessions/"+sessionID+"/skills?body=1", nil, &out)
    return out, viewErr(err)
}

func (b tuiBackend) Agents(ctx context.Context, sessionID string) (daemon.AgentsJSON, error) {
    list, err := b.c.listAgents(ctx, sessionID)
    return list, viewErr(err)
}

func (b tuiBackend) Jobs(ctx context.Context) ([]daemon.JobJSON, error) {
    var out []daemon.JobJSON
    err := b.c.do(ctx, "GET", "/api/jobs", nil, &out)
    return out, viewErr(err)
}

func (b tuiBackend) Usage(ctx context.Context, sessionID string) (daemon.UsageJSON, error) {
    var out daemon.UsageJSON
    path := "/api/usage"
    if sessionID != "" {
        path += "?session=" + sessionID
    }
    err := b.c.do(ctx, "GET", path, nil, &out)
    return out, viewErr(err)
}

func (b tuiBackend) CancelJob(ctx context.Context, id int64) error {
    return viewErr(b.c.do(ctx, "DELETE", "/api/jobs/"+strconv.FormatInt(id, 10), nil, nil))
}
"""


def apply_tests():
    # internal/store/usage_test.go: "time" import, then the test.
    if todo("internal/store/usage_test.go", '\t"time"'):
        with One("internal/store/usage_test.go") as ed:
            ed.insert_after(ed.locate('\t"testing"'), ['\t"time"'])
    append_go("internal/store/usage_test.go", STORE_TEST, "func TestDailyUsageGroupsByDayAndModelInsideTheWindow")

    taskkit.create_file("internal/daemon/usage_test.go", go(DAEMON_USAGE_TEST))
    append_go("internal/daemon/skills_test.go", SKILLS_TEST, "func TestSkillsIncludeTheirBodyOnlyWhenAsked")

    # cmd/spore/tui_e2e_test.go: "errors" and "net/http" imports, then the adapter tests.
    if todo("cmd/spore/tui_e2e_test.go", '\t"errors"'):
        with One("cmd/spore/tui_e2e_test.go") as ed:
            ed.insert_after(ed.locate('\t"context"'), ['\t"errors"'])
    if todo("cmd/spore/tui_e2e_test.go", '\t"net/http"\n'):
        with One("cmd/spore/tui_e2e_test.go") as ed:
            ed.insert_before(ed.locate('\t"net/http/httptest"'), ['\t"net/http"'])
    append_go("cmd/spore/tui_e2e_test.go", ADAPTER_TESTS, "func TestTheAdapterReadsTheViewsFromARealDaemon")


def apply_impl():
    # internal/store/usage.go: import "time", then DailyUsage.
    if todo("internal/store/usage.go", '\t"time"'):
        with One("internal/store/usage.go") as ed:
            ed.swap(ed.locate('import "context"'), ["import (", '\t"context"', '\t"time"', ")"])
    append_go("internal/store/usage.go", STORE_IMPL, "func (s *Store) DailyUsage(")

    taskkit.create_file("internal/daemon/usage.go", go(DAEMON_USAGE))

    if todo("internal/daemon/server.go", '"GET /api/usage"'):
        with One("internal/daemon/server.go") as ed:
            ed.insert_after(ed.locate('\tmux.HandleFunc("DELETE /api/jobs/{id}", s.handleCancelJob)'),
                            ['\tmux.HandleFunc("GET /api/usage", s.handleUsage)'])

    if todo("internal/daemon/skills.go", 'Body string `json:"body,omitempty"`'):
        with One("internal/daemon/skills.go") as ed:
            ed.insert_after(ed.locate('\tLoaded      bool   `json:"loaded"`'), lines("""
    // Body is the skill's full text, present only when the request asks for
    // it with ?body=1: the listing stays small by default.
    Body string `json:"body,omitempty"`
"""))
    if todo("internal/daemon/skills.go", 'withBody := r.URL.Query().Get("body") == "1"'):
        with One("internal/daemon/skills.go") as ed:
            ed.insert_before(ed.locate("\t\tfor _, sk := range s.agent.Skills.Skills(dir) {"),
                             ['\t\twithBody := r.URL.Query().Get("body") == "1"'])
    if todo("internal/daemon/skills.go", "j.Body = sk.Body"):
        with One("internal/daemon/skills.go") as ed:
            loop = ed.locate("\t\tfor _, sk := range s.agent.Skills.Skills(dir) {")
            first = offset(ed, loop, 1, "\t\t\tout.Skills = append(out.Skills, SkillJSON{")
            last = offset(ed, loop, 6, "\t\t\t})")
            ed.swap_range(first, last, lines("""
            j := SkillJSON{
                Name:        sk.Name,
                Description: sk.Description,
                BodyTokens:  agent.EstimateTokens(sk.Body),
                Loaded:      loaded[sk.Name],
            }
            if withBody {
                j.Body = sk.Body
            }
            out.Skills = append(out.Skills, j)
"""))

    taskkit.create_file("internal/tui/views.go", go(TUI_VIEWS))

    if todo("cmd/spore/tui_backend.go", '\t"strconv"'):
        with One("cmd/spore/tui_backend.go") as ed:
            ed.insert_after(ed.locate('\t"fmt"'), ['\t"strconv"', '\t"strings"'])
    append_go("cmd/spore/tui_backend.go", ADAPTER_IMPL, "var _ tui.Views = tuiBackend{}")


def apply():
    apply_tests()
    apply_impl()


RED = [
    ("the store test needs DailyUsage",
     plan_lib.gotest("./internal/store/", run="TestDailyUsage", race=False), ["st.DailyUsage undefined"]),
    ("the daemon tests need UsageJSON and SkillJSON.Body",
     plan_lib.gotest("./internal/daemon/", run="Usage|SkillsIncludeTheirBody", race=False),
     ["undefined: UsageJSON", "Body undefined"]),
    ("the adapter tests need the Views methods",
     plan_lib.gotest("./cmd/spore/", run="TheAdapterReads|WithoutTheRoute", race=False),
     ["be.Usage undefined"]),
]


def verify():
    gate.structural("route registered at its call site",
                    lambda: taskkit.file_contains("internal/daemon/server.go", 'mux.HandleFunc("GET /api/usage", s.handleUsage)'))
    gate.structural("skills handler reads ?body=1",
                    lambda: taskkit.file_contains("internal/daemon/skills.go", 'r.URL.Query().Get("body") == "1"'))
    gate.structural("adapter asserts it implements tui.Views",
                    lambda: taskkit.file_contains("cmd/spore/tui_backend.go", "var _ tui.Views = tuiBackend{}"))
    gate.structural("gofmt clean", lambda: plan_lib.gofmt_clean(
        "internal/store/usage.go", "internal/store/usage_test.go", "internal/daemon/usage.go",
        "internal/daemon/usage_test.go", "internal/daemon/skills.go", "internal/daemon/skills_test.go",
        "internal/daemon/server.go", "internal/tui/views.go", "cmd/spore/tui_backend.go", "cmd/spore/tui_e2e_test.go"))
    gate.component("store and daemon tests, race", gotest("./internal/store/", "./internal/daemon/"))
    gate.component("tui still builds and passes", gotest("./internal/tui/"))
    gate.crossing("adapter reads usage, skills, agents and jobs from a real daemon; an old daemon says so",
                  cmd=gotest("./cmd/spore/", run="TheAdapterReads|WithoutTheRoute", count=3))
    gate.component("cmd/spore tests, race", gotest("./cmd/spore/"))


if __name__ == "__main__":
    if "--red" in sys.argv:
        apply_tests()
        raise SystemExit(plan_lib.red(RED))
    raise SystemExit(gate.run(apply, verify))
