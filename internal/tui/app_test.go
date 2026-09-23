package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/subagent"
)

// fakeBackend records every call the model makes.
type fakeBackend struct {
	mu          sync.Mutex
	sent        []string
	stopped     []string
	resolved    []string
	cancelled   []string
	sendErr     error
	resolveErr  error
	created     string
	sessions    []daemon.SessionJSON
	transcripts map[string]daemon.TranscriptJSON

	skills        daemon.SkillsJSON
	agents        daemon.AgentsJSON
	jobRuns       map[int64][]daemon.JobRunJSON
	jobs          []daemon.JobJSON
	usage         daemon.UsageJSON
	viewErr       error
	fetches       int
	cancelledJobs []int64

	deletes     []deleteCall
	deleteNotes []string
	seen        []string
}

func (f *fakeBackend) Sessions(context.Context) ([]daemon.SessionJSON, error) { return f.sessions, nil }
func (f *fakeBackend) Transcript(_ context.Context, id string) (daemon.TranscriptJSON, error) {
	if t, ok := f.transcripts[id]; ok {
		return t, nil
	}
	return daemon.TranscriptJSON{Session: daemon.SessionJSON{ID: id}}, nil
}
func (f *fakeBackend) Events(context.Context) (<-chan daemon.WireEvent, error) {
	return make(chan daemon.WireEvent), nil
}
func (f *fakeBackend) Send(_ context.Context, id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, id+":"+text)
	return f.sendErr
}
func (f *fakeBackend) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return nil
}
func (f *fakeBackend) Resolve(_ context.Context, id string, pendingID int64, ans policy.Answer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolved = append(f.resolved, fmt.Sprintf("%s:%d:%t", id, pendingID, ans.Allow))
	return f.resolveErr
}
func (f *fakeBackend) CancelAgent(_ context.Context, parent, child string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelled = append(f.cancelled, parent+">"+child)
	return nil
}
func (f *fakeBackend) NewSession(context.Context, string) (string, error) { return f.created, nil }
func (f *fakeBackend) Slash(_ context.Context, _, cmd string) (string, error) {
	return "did " + cmd, nil
}

type deleteCall struct {
	ids          []string
	all, discord bool
}

func (f *fakeBackend) DeleteSessions(_ context.Context, ids []string, all, discord bool) (daemon.DeleteSessionsJSON, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, deleteCall{ids: ids, all: all, discord: discord})
	gone := ids
	if all {
		gone = nil
		for _, s := range f.sessions {
			gone = append(gone, s.ID)
		}
	}
	return daemon.DeleteSessionsJSON{Deleted: gone, Notes: f.deleteNotes}, nil
}
func (f *fakeBackend) MarkSeen(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, id)
	return nil
}
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
func (f *fakeBackend) JobRuns(_ context.Context, id int64) ([]daemon.JobRunJSON, error) {
	f.fetched()
	return f.jobRuns[id], f.viewErr
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

func newTestModel(t *testing.T, fb *fakeBackend, selected string) *Model {
	t.Helper()
	m := New(context.Background(), fb, selected, Options{Now: fixedNow})
	// A blinking cursor schedules a timer per keystroke; hold it still.
	m.input.Cursor.SetMode(cursor.CursorStatic)
	m.line.Cursor.SetMode(cursor.CursorStatic)
	// A real refresh tick sleeps two seconds; tests drive ticks by hand.
	m.viewTick = func(int) tea.Cmd { return nil }
	// Initialize with a default session if none provided
	if selected != "" && len(fb.sessions) == 0 {
		fb.sessions = []daemon.SessionJSON{{ID: selected, Source: "chat", Workspace: "/tmp"}}
		run(m, sessionsMsg{list: fb.sessions})
	}
	run(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	return m
}

// run applies msg and executes every command it returns, feeding the
// resulting messages back in, the way the Bubble Tea runtime does.
func run(m *Model, msg tea.Msg) {
	queue := []tea.Msg{msg}
	for steps := 0; len(queue) > 0 && steps < 200; steps++ {
		next := queue[0]
		queue = queue[1:]
		_, cmd := m.Update(next)
		queue = append(queue, expand(cmd)...)
	}
}

func expand(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, expand(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+b":
		return tea.KeyMsg{Type: tea.KeyCtrlB}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

func press(m *Model, ks ...string) {
	for _, k := range ks {
		run(m, keyMsg(k))
	}
}

func typeText(m *Model, s string) {
	for _, r := range s {
		run(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

func feed(m *Model, evs ...daemon.WireEvent) {
	for _, ev := range evs {
		run(m, eventMsg{ev: ev})
	}
}

func TestAYTypedInInsertNeverAnswersAnApproval(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "s1")
	feed(m, wev("s1", daemon.WireTurnStarted),
		daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1, Tool: "shell", Args: `{}`})

	typeText(m, "yes")
	if len(fb.resolved) != 0 {
		t.Fatalf("typing in INSERT resolved %v", fb.resolved)
	}
	if m.input.Value() != "yes" {
		t.Fatalf("input = %q, want the typed text", m.input.Value())
	}
	if !strings.Contains(m.View(), "esc, then y/n/s/p") {
		t.Fatal("the overlay does not tell an INSERT-mode user how to answer")
	}

	press(m, "esc", "y", "y")
	if len(fb.resolved) != 1 || fb.resolved[0] != "s1:1:true" {
		t.Fatalf("resolved = %v, want s1:1:true", fb.resolved)
	}
}

func TestEscInInsertLeavesInsertAndOnlyEscInNormalStops(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "s1")
	feed(m, wev("s1", daemon.WireTurnStarted))

	press(m, "esc")
	if m.mode != modeNormal || len(fb.stopped) != 0 {
		t.Fatalf("after one esc: mode=%s stopped=%v, want NORMAL and no stop", m.mode, fb.stopped)
	}
	press(m, "esc")
	if len(fb.stopped) != 1 || fb.stopped[0] != "s1" {
		t.Fatalf("stopped = %v, want exactly s1", fb.stopped)
	}
}

func TestAMessageTypedDuringATurnIsSentWhenItEnds(t *testing.T) {
	for _, end := range []string{daemon.WireTurnDone, daemon.WireStopped} {
		fb := &fakeBackend{}
		m := newTestModel(t, fb, "s1")
		feed(m, wev("s1", daemon.WireTurnStarted))
		typeText(m, "next")
		press(m, "enter")
		if len(fb.sent) != 0 {
			t.Fatalf("[%s] sent %v while the turn was running", end, fb.sent)
		}
		feed(m, wev("s1", end))
		if len(fb.sent) != 1 || fb.sent[0] != "s1:next" {
			t.Fatalf("[%s] sent = %v, want s1:next once the turn ended", end, fb.sent)
		}
	}
}

func TestXStopsASubagentOnlyAfterYes(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "kid")
	feed(m, daemon.WireEvent{Session: "kid", Type: daemon.WireSession, ParentID: "root", Source: "subagent", Title: "audit"})

	press(m, "esc", "x", "n")
	if len(fb.cancelled) != 0 {
		t.Fatalf("cancelled %v after answering n", fb.cancelled)
	}
	press(m, "x", "y")
	if len(fb.cancelled) != 1 || fb.cancelled[0] != "root>kid" {
		t.Fatalf("cancelled = %v, want root>kid", fb.cancelled)
	}
}

func TestASubagentsApprovalIsAnsweredThroughItsRoot(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "kid")
	feed(m,
		daemon.WireEvent{Session: "kid", Type: daemon.WireSession, Title: "kid", Workspace: "/work", ParentID: "root", Source: "subagent"},
		daemon.WireEvent{Session: "root", Type: daemon.WireApproval, PendingID: 9, Tool: "shell", Origin: "kid"},
	)
	if !strings.Contains(m.View(), "sub-agent kid") {
		t.Fatal("the overlay does not say a sub-agent is asking")
	}
	press(m, "esc", "y", "y")
	if len(fb.resolved) != 1 || fb.resolved[0] != "root:9:true" {
		t.Fatalf("resolved = %v, want root:9:true", fb.resolved)
	}
}

func TestAFailedAnswerKeepsTheApprovalOnScreen(t *testing.T) {
	fb := &fakeBackend{resolveErr: errors.New("boom")}
	m := newTestModel(t, fb, "s1")
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 3, Tool: "shell"})
	press(m, "esc", "y", "y")
	if _, _, ok := m.cache.approvalFor("s1"); !ok {
		t.Fatal("the approval disappeared although the answer failed")
	}
	if !strings.Contains(m.View(), "boom") {
		t.Fatal("the failure is not shown")
	}
}

func TestASendRefusedByARunningTurnIsQueuedNotDuplicated(t *testing.T) {
	fb := &fakeBackend{sendErr: errors.New("POST /api/sessions/s1/messages: session s1 already has a turn running")}
	m := newTestModel(t, fb, "s1")
	typeText(m, "hi")
	press(m, "enter")
	if q := m.queued["s1"]; len(q) != 1 || q[0] != "hi" {
		t.Fatalf("queued = %v, want [hi]", q)
	}

	fb.sendErr = nil
	feed(m, wev("s1", daemon.WireTurnDone))
	if len(fb.sent) != 2 || fb.sent[1] != "s1:hi" {
		t.Fatalf("sent = %v, want the refused send then the retry", fb.sent)
	}
	users := 0
	for _, b := range m.cache.get("s1").blocks {
		if b.kind == kindUser {
			users++
		}
	}
	if users != 1 {
		t.Fatalf("%d user blocks, want 1", users)
	}
}

// A multiplexer that holds a lone Esc (tmux's escape-time) delivers Esc and
// the next key as one read, which Bubble Tea parses as alt+<key>. It must act
// as Esc followed by the key, or esc-then-n does nothing.
func TestEscAndAKeyInOneReadActAsEscThenTheKey(t *testing.T) {
	fb := &fakeBackend{created: "fresh"}
	m := newTestModel(t, fb, "s1")
	run(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n"), Alt: true})
	if m.selected != "fresh" {
		t.Fatalf("alt+n from INSERT selected %q, want the new session", m.selected)
	}

	fb2 := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "aaaa", Title: "first", Source: "chat", Workspace: "/w", UpdatedAt: t0},
		{ID: "bbbb", Title: "second", Source: "chat", Workspace: "/w", UpdatedAt: t0.Add(-1)},
	}}
	m2 := newTestModel(t, fb2, "aaaa")
	run(m2, sessionsMsg{list: fb2.sessions})
	run(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j"), Alt: true})
	if m2.mode != modeNormal || m2.selected != "aaaa" {
		t.Fatalf("alt+j from INSERT: mode=%s selected=%q, want NORMAL, still on aaaa (j scrolls the chat)", m2.mode, m2.selected)
	}
	press(m2, "tab")
	run(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j"), Alt: true})
	if m2.selected != "bbbb" {
		t.Fatalf("alt+j in the sidebar selected %q, want bbbb", m2.selected)
	}
}

func TestNewCommandCreatesAndSelectsTheSession(t *testing.T) {
	fb := &fakeBackend{created: "fresh"}
	m := newTestModel(t, fb, "s1")
	press(m, "esc", ":")
	typeText(m, "new /tmp/x")
	press(m, "enter")
	if m.selected != "fresh" || m.mode != modeInsert {
		t.Fatalf("selected=%q mode=%s, want fresh in INSERT", m.selected, m.mode)
	}
}

func TestTabCompletesACommand(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	press(m, "esc", ":")
	typeText(m, "ag")
	press(m, "tab")
	if got := m.line.Value(); got != "agents " {
		t.Fatalf("completed = %q, want %q", got, "agents ")
	}
}

func TestSlashCommandsStillWorkInInsert(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	typeText(m, "/usage")
	press(m, "enter")
	if !strings.Contains(m.View(), "did usage") {
		t.Fatal("/usage did not run")
	}
}

func TestATinyTerminalSaysSo(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	run(m, tea.WindowSizeMsg{Width: 50, Height: 10})
	if got := m.View(); got != "terminal too small" {
		t.Fatalf("View = %q", got)
	}
}

func TestReconnectClearsApprovalsAndResyncs(t *testing.T) {
	fb := &fakeBackend{
		sessions: []daemon.SessionJSON{{ID: "s1", Title: "t", Source: "chat"}, {ID: "s2", Title: "u", Source: "chat"}},
		transcripts: map[string]daemon.TranscriptJSON{"s1": {
			Session:  daemon.SessionJSON{ID: "s1"},
			Messages: []daemon.MessageJSON{{Role: "user", Blocks: []provider.Block{{Type: provider.BlockText, Text: "from disk"}}}},
		}},
	}
	m := newTestModel(t, fb, "s1")
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireApproval, PendingID: 1})
	run(m, connectedMsg{})
	if _, _, ok := m.cache.approvalFor("s1"); ok {
		t.Fatal("a reconnect kept an approval the feed has not replayed")
	}
	if _, ok := m.cache.sessions["s2"]; !ok {
		t.Fatal("the session list was not reloaded")
	}
	if !strings.Contains(m.View(), "from disk") {
		t.Fatal("the selected transcript was not reloaded")
	}
}

func TestTheStatusBarShowsContextAndBlocked(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	feed(m,
		daemon.WireEvent{Session: "s1", Type: daemon.WireTurnDone, Model: "sonnet-5", TokensIn: 1000, TokensCacheRead: 37000},
		daemon.WireEvent{Session: "other", Type: daemon.WireApproval, PendingID: 4},
	)
	v := m.View()
	for _, want := range []string{"sonnet-5", "ctx 38.0k", "1 blocked", "INSERT"} {
		if !strings.Contains(v, want) {
			t.Errorf("status bar is missing %q", want)
		}
	}
}

func TestJAndKMoveTheSelection(t *testing.T) {
	fb := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "aaaa", Title: "first", Source: "chat", Workspace: "/w", UpdatedAt: t0},
		{ID: "bbbb", Title: "second", Source: "chat", Workspace: "/w", UpdatedAt: t0.Add(-1)},
	}}
	m := newTestModel(t, fb, "aaaa")
	run(m, sessionsMsg{list: fb.sessions})
	press(m, "esc", "tab", "j")
	if m.selected != "bbbb" {
		t.Fatalf("after j selected = %q, want bbbb", m.selected)
	}
	press(m, "k")
	if m.selected != "aaaa" {
		t.Fatalf("after k selected = %q, want aaaa", m.selected)
	}
}

func TestAHotkeyOpensItsViewOnlyInNormal(t *testing.T) {
	fb := &fakeBackend{skills: daemon.SkillsJSON{Skills: []daemon.SkillJSON{{Name: "debug"}}}}
	m := newTestModel(t, fb, "s1")
	typeText(m, "S")
	if m.table != nil || m.input.Value() != "S" {
		t.Fatalf("S in INSERT opened a view (table=%v, input=%q)", m.table != nil, m.input.Value())
	}
	press(m, "esc", "S")
	if m.table == nil || m.table.res.Name() != "skills" {
		t.Fatal("S in NORMAL did not open skills")
	}
	if !strings.Contains(m.View(), "skills(1)") || !strings.Contains(m.View(), "debug") {
		t.Fatalf("view:\n%s", m.View())
	}
}

func TestAltHotkeyFromInsertOpensTheView(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	run(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("J"), Alt: true})
	if m.table == nil || m.table.res.Name() != "jobs" {
		t.Fatal("alt+J from INSERT did not open jobs")
	}
}

func TestEscClosesTheInnermostLayerFirst(t *testing.T) {
	fb := &fakeBackend{skills: daemon.SkillsJSON{Skills: []daemon.SkillJSON{{Name: "debug"}, {Name: "deploy"}}}}
	m := newTestModel(t, fb, "s1")
	feed(m, wev("s1", daemon.WireTurnStarted))
	press(m, "esc", "S", "enter")
	if m.table.detail == nil {
		t.Fatal("enter did not open the detail pane")
	}
	press(m, "esc")
	if m.table == nil || m.table.detail != nil {
		t.Fatal("esc must close the detail pane and keep the view")
	}
	press(m, "/")
	typeText(m, "dep")
	press(m, "enter")
	if m.table.filter != "dep" {
		t.Fatalf("filter = %q", m.table.filter)
	}
	press(m, "esc")
	if m.table == nil || m.table.filter != "" {
		t.Fatal("esc must clear the filter and keep the view")
	}
	press(m, "esc")
	if m.table != nil {
		t.Fatal("esc must close the view")
	}
	if len(fb.stopped) != 0 {
		t.Fatalf("closing layers stopped the turn: %v", fb.stopped)
	}
	press(m, "esc")
	if len(fb.stopped) != 1 {
		t.Fatal("esc on the chat screen must still stop the running turn")
	}
}

func TestColonCommandsOpenViewsAndChatReturns(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	press(m, "esc", ":")
	typeText(m, "usage")
	press(m, "enter")
	if m.table == nil || m.table.res.Name() != "usage" {
		t.Fatal(":usage did not open the usage view")
	}
	press(m, ":")
	typeText(m, "chat")
	press(m, "enter")
	if m.table != nil {
		t.Fatal(":chat did not return to the chat screen")
	}
}

func TestAConfirmedActionRunsOnlyOnYesAndRefreshes(t *testing.T) {
	fb := &fakeBackend{jobs: []daemon.JobJSON{{ID: 7, Kind: "cron", Spec: "0 9 * * *", Prompt: "morning", Enabled: true, NextRun: fixedNow().Add(time.Hour)}}}
	m := newTestModel(t, fb, "s1")
	press(m, "esc", "J", "x")
	if m.mode != modeConfirm || !strings.Contains(m.View(), "Cancel job 7?") {
		t.Fatalf("mode=%s, want the confirmation modal:\n%s", m.mode, m.View())
	}
	press(m, "n")
	if len(fb.cancelledJobs) != 0 {
		t.Fatal("n ran the action")
	}
	before := fb.fetches
	press(m, "x", "y")
	if len(fb.cancelledJobs) != 1 || fb.cancelledJobs[0] != 7 {
		t.Fatalf("cancelled = %v, want [7]", fb.cancelledJobs)
	}
	if fb.fetches <= before {
		t.Fatal("the view did not refresh after the action")
	}
}

func TestEnterOnAnAgentSelectsTheChildSession(t *testing.T) {
	fb := &fakeBackend{agents: daemon.AgentsJSON{Agents: []subagent.Status{{ID: "kid1", State: "done", Started: fixedNow()}}}}
	m := newTestModel(t, fb, "s1")
	press(m, "esc", "A", "enter")
	if m.table != nil || m.selected != "kid1" {
		t.Fatalf("table=%v selected=%q, want the chat screen on kid1", m.table != nil, m.selected)
	}
}

func TestAViewRefetchesOnItsTickAndIgnoresStaleTicks(t *testing.T) {
	fb := &fakeBackend{}
	m := newTestModel(t, fb, "s1")
	press(m, "esc", "J")
	gen, before := m.viewGen, fb.fetches
	run(m, viewTickMsg{gen: gen})
	if fb.fetches != before+1 {
		t.Fatalf("fetches = %d, want %d after a tick", fb.fetches, before+1)
	}
	press(m, "esc")
	run(m, viewTickMsg{gen: gen})
	if fb.fetches != before+1 {
		t.Fatal("a tick for a closed view fetched")
	}
}

func TestRowsForAViewThatIsNoLongerOpenAreDropped(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	press(m, "esc", "S")
	run(m, viewRowsMsg{name: "jobs", rows: things([]string{"x"})})
	if len(m.table.rows) != 0 {
		t.Fatal("rows for another view were installed")
	}
}

func TestSlashUsageInInsertStillPrintsText(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	typeText(m, "/usage")
	press(m, "enter")
	if m.table != nil || !strings.Contains(m.View(), "did usage") {
		t.Fatal("/usage typed into the input must print the report, not open the view")
	}
}

func TestZTogglesTheJobsFolder(t *testing.T) {
	m := newTestModel(t, &fakeBackend{}, "s1")
	press(m, "esc", "z")
	if !m.cache.jobsOpen {
		t.Fatal("z did not open the jobs folder")
	}
	press(m, "z")
	if m.cache.jobsOpen {
		t.Fatal("z did not close the jobs folder again")
	}
}

func jobScene(t *testing.T) (*Model, *fakeBackend) {
	t.Helper()
	fb := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "s1", Title: "chat", Source: "chat", Workspace: "/w"},
		{ID: "r1", Title: "joke", Source: "job", JobID: 7, Unread: true, Workspace: "/s/r1"},
	}}
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	return m, fb
}

func TestOpeningAnUnreadJobRunMarksItSeen(t *testing.T) {
	m, fb := jobScene(t)
	press(m, "esc", "tab", "z", "k") // open the folder; its run is listed above the chats
	if m.selected != "r1" {
		t.Fatalf("selected = %q, want the job run", m.selected)
	}
	if len(fb.seen) != 1 || fb.seen[0] != "r1" {
		t.Fatalf("seen = %v, want r1 marked seen on the daemon", fb.seen)
	}
	if m.cache.get("r1").info.Unread {
		t.Fatal("the run still reads unread after it was opened")
	}
}

func TestAJobRunFinishingWhileAwayBadgesTheFolder(t *testing.T) {
	m, fb := jobScene(t)
	m.cache.get("r1").info.Unread = false
	feed(m, wev("r1", daemon.WireTurnDone))
	if !m.cache.get("r1").info.Unread {
		t.Fatal("a job run that finished while another session was open must read unread")
	}
	if !strings.Contains(m.View(), "jobs") {
		t.Fatalf("view:\n%s", m.View())
	}
	if len(fb.seen) != 0 {
		t.Fatalf("seen = %v, want nothing marked while the run was not open", fb.seen)
	}
}

func deleteScene(t *testing.T) (*Model, *fakeBackend) {
	t.Helper()
	fb := &fakeBackend{sessions: []daemon.SessionJSON{
		{ID: "s1", Title: "first", Source: "chat", Workspace: "/w"},
		{ID: "s2", Title: "second", Source: "chat", Workspace: "/w"},
	}}
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	return m, fb
}

func TestDeleteAsksThenDeletesTheSelectedSessionAndMovesOn(t *testing.T) {
	m, fb := deleteScene(t)
	press(m, "esc", "d")
	if m.mode != modeConfirm || !strings.Contains(m.View(), `Delete "first"?`) {
		t.Fatalf("mode = %s, want the delete prompt:\n%s", m.mode, m.View())
	}
	press(m, "y")
	if len(fb.deletes) != 1 || fb.deletes[0].all || fb.deletes[0].discord || len(fb.deletes[0].ids) != 1 || fb.deletes[0].ids[0] != "s1" {
		t.Fatalf("deletes = %+v, want s1 only, not on Discord", fb.deletes)
	}
	if _, still := m.cache.sessions["s1"]; still {
		t.Fatal("the deleted session is still in the sidebar")
	}
	if m.selected != "s2" {
		t.Fatalf("selected = %q, want the next session", m.selected)
	}
	if !strings.Contains(m.View(), "deleted 1 session") {
		t.Fatalf("view does not report the delete:\n%s", m.View())
	}
}

func TestCapitalDAlsoDeletesOnDiscordAndShowsWhatItDid(t *testing.T) {
	m, fb := deleteScene(t)
	fb.deleteNotes = []string{"deleted the Discord thread 42"}
	press(m, "esc", "d", "D")
	if len(fb.deletes) != 1 || !fb.deletes[0].discord {
		t.Fatalf("deletes = %+v, want the Discord copy deleted too", fb.deletes)
	}
	if !strings.Contains(m.View(), "deleted the Discord thread 42") {
		t.Fatalf("view does not show the bridge's report:\n%s", m.View())
	}
}

func TestAnythingButYOrDKeepsTheSession(t *testing.T) {
	m, fb := deleteScene(t)
	press(m, "esc", "d", "n")
	if len(fb.deletes) != 0 || m.selected != "s1" {
		t.Fatalf("deletes = %+v, selected = %q; want nothing deleted", fb.deletes, m.selected)
	}
}

func TestColonDeleteAllAsksAboutEverySession(t *testing.T) {
	m, fb := deleteScene(t)
	press(m, "esc", ":")
	typeText(m, "delete all")
	press(m, "enter")
	if m.mode != modeConfirm || !strings.Contains(m.View(), "EVERY session") {
		t.Fatalf("mode = %s, want the delete-all prompt:\n%s", m.mode, m.View())
	}
	press(m, "y")
	if len(fb.deletes) != 1 || !fb.deletes[0].all {
		t.Fatalf("deletes = %+v, want one delete-all", fb.deletes)
	}
	if len(m.selectable()) != 0 || m.selected != "" {
		t.Fatalf("selectable = %v, selected = %q; want an empty sidebar", m.selectable(), m.selected)
	}
}

func TestASessionDeletedElsewhereLeavesTheSidebar(t *testing.T) {
	m, _ := deleteScene(t)
	feed(m, daemon.WireEvent{Session: "s1", Type: daemon.WireSessionDeleted})
	if _, still := m.cache.sessions["s1"]; still {
		t.Fatal("a session deleted from another client is still listed")
	}
	if m.selected != "s2" {
		t.Fatalf("selected = %q, want the selection moved off the deleted session", m.selected)
	}
}

// The cursor climbs past the chats onto the jobs folder, enter opens it, and
// the job's label and runs are then reachable with k and j.
func TestTheCursorReachesTheJobsFolderAndOpensIt(t *testing.T) {
	m, _ := jobScene(t)
	press(m, "esc", "tab", "k")
	if m.sideCursor != folderKey || m.selected != "s1" {
		t.Fatalf("cursor=%q selected=%q; want the folder under the cursor and s1 still shown", m.sideCursor, m.selected)
	}
	if !strings.Contains(m.View(), "▸ jobs") {
		t.Fatalf("the folder is not drawn closed:\n%s", m.View())
	}
	press(m, "enter")
	if !m.cache.jobsOpen || m.mode != modeNormal {
		t.Fatalf("enter on the folder: open=%v mode=%s; want it open, still in NORMAL", m.cache.jobsOpen, m.mode)
	}
	press(m, "j")
	if m.sideCursor != jobKey(7) {
		t.Fatalf("cursor = %q, want job 7's label", m.sideCursor)
	}
	press(m, "j")
	if m.sideCursor != "" || m.selected != "r1" {
		t.Fatalf("cursor=%q selected=%q; want the run selected", m.sideCursor, m.selected)
	}
	press(m, "k", "k", "enter")
	if m.cache.jobsOpen {
		t.Fatal("enter on the open folder did not close it")
	}
}

func runsBackend() *fakeBackend {
	fb := &fakeBackend{
		sessions: []daemon.SessionJSON{
			{ID: "s1", Title: "chat", Source: "chat", Workspace: "/w"},
			{ID: "r2", Title: "joke", Source: "job", JobID: 7, Workspace: "/s/r2"},
			{ID: "r1", Title: "joke", Source: "job", JobID: 7, Workspace: "/s/r1"},
		},
		jobs: []daemon.JobJSON{{ID: 7, Kind: "cron", Spec: "*/5 * * * *", Prompt: "tell a joke", Enabled: true, NextRun: fixedNow().Add(time.Hour)}},
		jobRuns: map[int64][]daemon.JobRunJSON{7: {
			{Session: daemon.SessionJSON{ID: "r2", CreatedAt: fixedNow().Add(-5 * time.Minute)}, Status: daemon.RunFailed},
			{Session: daemon.SessionJSON{ID: "r1", CreatedAt: fixedNow().Add(-10 * time.Minute)}, Status: daemon.RunOK,
				Output: "My grandfather died leaving me his stress.\nSecond line."},
		}},
	}
	// Loading a run's transcript must not strip what the listing said
	// about it, as the daemon's own transcript never does.
	fb.transcripts = map[string]daemon.TranscriptJSON{}
	for _, s := range fb.sessions {
		fb.transcripts[s.ID] = daemon.TranscriptJSON{Session: s}
	}
	return fb
}

// Enter on a job in the jobs view lists every run with its output; enter on
// a run shows the whole output, esc steps back one window at a time, and o
// opens the run in the chat.
func TestEnterOnAJobListsItsRunsAndShowsEachOutput(t *testing.T) {
	fb := runsBackend()
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	press(m, "esc", "J", "enter")
	if m.table == nil || m.table.res.Name() != "job 7 runs" {
		t.Fatalf("enter on a job did not open its runs")
	}
	view := m.View()
	for _, want := range []string{"failed", "ok", "My grandfather died", "(no output)"} {
		if !strings.Contains(view, want) {
			t.Errorf("runs window lacks %q:\n%s", want, view)
		}
	}
	press(m, "j", "enter") // the older, successful run
	if m.table.detail == nil || !strings.Contains(m.View(), "Second line.") {
		t.Fatalf("enter did not show the run's whole output:\n%s", m.View())
	}
	press(m, "esc") // detail -> runs
	if m.table == nil || m.table.detail != nil || m.table.res.Name() != "job 7 runs" {
		t.Fatal("esc from the output did not return to the runs")
	}
	press(m, "esc") // runs -> jobs
	if m.table == nil || m.table.res.Name() != "jobs" {
		t.Fatal("esc from the runs did not return to the jobs view")
	}
	press(m, "enter", "o") // back in, open the newest run in the chat
	if m.table != nil || m.selected != "r2" || len(m.back) != 0 {
		t.Fatalf("o: table=%v selected=%q back=%d; want the chat on r2", m.table != nil, m.selected, len(m.back))
	}
}

func TestEnterOnAJobLabelInTheSidebarOpensItsRuns(t *testing.T) {
	fb := runsBackend()
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	press(m, "esc", "tab", "z")
	for m.sideCursor != jobKey(7) {
		before := m.sideCursor + m.selected
		press(m, "k")
		if m.sideCursor+m.selected == before {
			t.Fatal("never reached job 7's label")
		}
	}
	press(m, "enter")
	if m.table == nil || m.table.res.Name() != "job 7 runs" {
		t.Fatal("enter on a job's label did not open its runs")
	}
	press(m, "esc")
	if m.table != nil {
		t.Fatal("esc from runs opened from the sidebar should return to the chat")
	}
}

// Closing the folder hides everything in it, the selected run included: the
// selection moves to the first chat, so the chat pane never shows a run the
// sidebar has hidden.
func TestClosingTheFolderLeavesNoRunsBehind(t *testing.T) {
	fb := runsBackend()
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	press(m, "esc", "tab", "z", "k") // open, then up onto a run
	if m.cache.get(m.selected).info.Source != "job" {
		t.Fatalf("selected = %q, want a job run", m.selected)
	}
	// Close it with enter on the folder, then again with z from a run.
	for _, how := range []string{"enter", "z"} {
		if !m.cache.jobsOpen {
			press(m, "z")
		}
		cursorTo(t, m, func() bool { return m.sideCursor == "" && m.cache.get(m.selected).info.Source == "job" })
		if how == "enter" {
			cursorTo(t, m, func() bool { return m.sideCursor == folderKey })
		}
		press(m, how)
		if m.cache.jobsOpen {
			t.Fatalf("%s did not close the folder", how)
		}
		view := m.View()
		if strings.Contains(view, "job 7 ·") || strings.Contains(view, short("r1")) || strings.Contains(view, short("r2")) {
			t.Fatalf("after %s the closed folder still shows its runs:\n%s", how, view)
		}
		if m.selected != "s1" || m.sideCursor != folderKey {
			t.Fatalf("after %s: selected=%q cursor=%q; want s1 shown and the cursor on the folder", how, m.selected, m.sideCursor)
		}
	}
}

// cursorTo moves the sidebar cursor, up then down, until at() holds.
func cursorTo(t *testing.T, m *Model, at func() bool) {
	t.Helper()
	for _, key := range []string{"k", "j"} {
		for i := 0; i < 20; i++ {
			if at() {
				return
			}
			press(m, key)
		}
	}
	if !at() {
		t.Fatal("the cursor never reached the row")
	}
}

// Opening a run from anywhere, such as o in the runs window, opens the
// folder it lives in.
func TestOpeningARunOpensTheFolder(t *testing.T) {
	fb := runsBackend()
	m := newTestModel(t, fb, "s1")
	run(m, sessionsMsg{list: fb.sessions})
	press(m, "esc", "J", "enter", "o")
	if m.selected != "r2" || !m.cache.jobsOpen {
		t.Fatalf("selected=%q open=%v; want r2 in an open folder", m.selected, m.cache.jobsOpen)
	}
}

func TestStartingOnARunOpensTheFolder(t *testing.T) {
	fb := runsBackend()
	m := newTestModel(t, fb, "r1")
	run(m, sessionsMsg{list: fb.sessions})
	if !m.cache.jobsOpen || !strings.Contains(m.View(), short("r1")) {
		t.Fatalf("started on r1: open=%v\n%s", m.cache.jobsOpen, m.View())
	}
}
