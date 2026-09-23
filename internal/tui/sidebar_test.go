package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/codered/spore/internal/daemon"
)

// seed builds a cache from session rows; later rows are older.
func seed(rows ...daemon.SessionJSON) *cache {
	c := newCache(fixedNow)
	for i, r := range rows {
		r.UpdatedAt = t0.Add(-time.Duration(i) * time.Minute)
		c.get(r.ID).info = r
	}
	return c
}

func ids(rs []row) []string {
	var out []string
	for _, r := range rs {
		if r.id != "" {
			out = append(out, fmt.Sprintf("%s@%d", r.id, r.depth))
		}
	}
	return out
}

func TestTheDefaultFilterShowsChatsBlockedAndWorking(t *testing.T) {
	c := seed(
		daemon.SessionJSON{ID: "c1", Title: "chat", Workspace: "/a", Source: "chat"},
		daemon.SessionJSON{ID: "k1", Title: "kid", Workspace: "/a", Source: "subagent", ParentID: "c1"},
		daemon.SessionJSON{ID: "d1", Title: "idle discord", Workspace: "/a", Source: "discord"},
		daemon.SessionJSON{ID: "d2", Title: "blocked discord", Workspace: "/b", Source: "discord"},
		daemon.SessionJSON{ID: "j1", Title: "working job", Workspace: "/b", Source: "job"},
	)
	c.get("d2").approvals = []daemon.WireEvent{{PendingID: 1}}
	c.get("j1").working = true

	// j1 is a job run: it lives in the Jobs folder, which starts collapsed.
	got := strings.Join(ids(c.rows(false, "", "")), " ")
	if got != "c1@0 k1@1 d2@0" {
		t.Fatalf("default rows = %q, want chat with its child, then the blocked session", got)
	}
	if all := strings.Join(ids(c.rows(true, "", "")), " "); !strings.Contains(all, "d1@0") {
		t.Fatalf(":sessions all = %q, want the idle discord session too", all)
	}
	if sel := strings.Join(ids(c.rows(false, "", "d1")), " "); !strings.Contains(sel, "d1@0") {
		t.Fatalf("rows with d1 selected = %q, the selection must always show", sel)
	}
}

// Sessions from before sources were recorded are migrated as "unknown". They
// are the user's whole history, so the default view must not hide them.
func TestTheDefaultFilterShowsSessionsOfUnknownSource(t *testing.T) {
	c := seed(
		daemon.SessionJSON{ID: "c1", Title: "now", Workspace: "/a", Source: "chat"},
		daemon.SessionJSON{ID: "u1", Title: "from before", Workspace: "/a", Source: "unknown"},
	)
	if got := strings.Join(ids(c.rows(false, "", "c1")), " "); got != "c1@0 u1@0" {
		t.Fatalf("default rows = %q, want the migrated session listed too", got)
	}
}

func TestIdleSessionsBeyondTenCollapse(t *testing.T) {
	var list []daemon.SessionJSON
	for i := 0; i < 12; i++ {
		list = append(list, daemon.SessionJSON{ID: fmt.Sprintf("s%02d", i), Workspace: "/a", Source: "chat"})
	}
	rs := seed(list...).rows(false, "", "")
	if n := len(ids(rs)); n != idleShownPerWorkspace {
		t.Fatalf("%d session rows, want %d", n, idleShownPerWorkspace)
	}
	if last := rs[len(rs)-1]; last.more != 2 {
		t.Fatalf("last row = %+v, want +2 more", last)
	}
}

func TestTheFilterMatchesTitleOrIDAcrossSources(t *testing.T) {
	c := seed(
		daemon.SessionJSON{ID: "aa11", Title: "Lint audit", Workspace: "/a", Source: "discord"},
		daemon.SessionJSON{ID: "bb22", Title: "notes", Workspace: "/a", Source: "chat"},
	)
	if got := ids(c.rows(false, "lint", "")); len(got) != 1 || got[0] != "aa11@0" {
		t.Fatalf("filter lint = %v", got)
	}
	if got := ids(c.rows(false, "bb", "")); len(got) != 1 || got[0] != "bb22@0" {
		t.Fatalf("filter bb = %v", got)
	}
}

func TestASessionRowShowsItsStateIDTitleAndSource(t *testing.T) {
	c := seed(daemon.SessionJSON{ID: "a1b2c3", Title: "fix flaky test", Workspace: "/a", Source: "chat"})
	c.get("a1b2c3").working = true
	out := ansi.Strip(renderSidebar(c, c.rows(false, "", ""), "", "", 30, 10))
	for _, want := range []string{"/a", "● a1b2 fix flaky test", "chat"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar is missing %q:\n%s", want, out)
		}
	}
	for i, line := range strings.Split(out, "\n") {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("line %d is %d cells wide, over the 30-cell sidebar: %q", i, w, line)
		}
	}
}

func TestSidebarRowsStayWithinWidthForWideTitlesAndDeepNesting(t *testing.T) {
	// Build a chain: root chat session plus 6 nested sub-agents
	root := daemon.SessionJSON{ID: "root", Title: "修复测试 🚀🚀🚀 flaky test with a long title", Workspace: "/a", Source: "chat"}
	sessions := []daemon.SessionJSON{root}
	parent := "root"
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("child%d", i)
		sessions = append(sessions, daemon.SessionJSON{
			ID:        id,
			Title:     "修复测试 🚀🚀🚀 flaky test with a long title",
			Workspace: "/a",
			Source:    "subagent",
			ParentID:  parent,
		})
		parent = id
	}
	c := seed(sessions...)
	deepestID := "child6"
	output := ansi.Strip(renderSidebar(c, c.rows(false, "", deepestID), deepestID, "", 30, 20))
	for i, line := range strings.Split(output, "\n") {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("line %d is %d cells wide, exceeds 30-cell budget: %q", i, w, line)
		}
	}
}

func jobRuns() *cache {
	return seed(
		daemon.SessionJSON{ID: "c1", Title: "chat", Workspace: "/a", Source: "chat"},
		daemon.SessionJSON{ID: "r1", Title: "Send me a joke", Workspace: "/s/r1", Source: "job", JobID: 7},
		daemon.SessionJSON{ID: "r2", Title: "nightly backup", Workspace: "/s/r2", Source: "job", JobID: 8},
		daemon.SessionJSON{ID: "r3", Title: "Send me a joke", Workspace: "/s/r3", Source: "job", JobID: 7},
	)
}

func folderRow(rs []row) (row, bool) {
	for _, r := range rs {
		if r.folder {
			return r, true
		}
	}
	return row{}, false
}

func TestJobRunsLiveInACollapsedJobsFolderAtTheTop(t *testing.T) {
	c := jobRuns()
	rs := c.rows(false, "", "")
	if !rs[0].folder || rs[0].runs != 3 {
		t.Fatalf("first row = %+v, want the jobs folder holding 3 runs", rs[0])
	}
	if got := strings.Join(ids(rs), " "); got != "c1@0" {
		t.Fatalf("collapsed rows = %q, want no job runs listed", got)
	}
	out := ansi.Strip(renderSidebar(c, rs, "", "", 30, 10))
	if first := strings.Split(out, "\n")[0]; !strings.HasPrefix(first, "▸ jobs") {
		t.Fatalf("first sidebar line = %q, want the collapsed folder", first)
	}
}

func TestAnOpenJobsFolderListsRunsUnderTheirJob(t *testing.T) {
	c := jobRuns()
	c.jobsOpen = true
	rs := c.rows(false, "", "")
	if got := strings.Join(ids(rs), " "); got != "r1@1 r3@1 r2@1 c1@0" {
		t.Fatalf("open rows = %q, want job 7's runs newest first, then job 8's, then the chats", got)
	}
	out := ansi.Strip(renderSidebar(c, rs, "", "", 30, 20))
	for _, want := range []string{"▾ jobs", "job 7 · Send me a joke", "job 8 · nightly backup"} {
		if !strings.Contains(out, want) {
			t.Errorf("sidebar is missing %q:\n%s", want, out)
		}
	}
}

func TestTheJobsFolderIsThereWhenEmptyAndBadgesUnreadRuns(t *testing.T) {
	empty := seed(daemon.SessionJSON{ID: "c1", Title: "chat", Workspace: "/a", Source: "chat"})
	if f, ok := folderRow(empty.rows(false, "", "")); !ok || f.runs != 0 || f.unread != 0 {
		t.Fatalf("folder with no runs = %+v %v, want an empty folder", f, ok)
	}
	c := jobRuns()
	c.get("r1").info.Unread = true
	c.get("r2").info.Unread = true
	f, _ := folderRow(c.rows(false, "", ""))
	if f.unread != 2 || f.busy {
		t.Fatalf("folder = %+v, want 2 unread and nothing running", f)
	}
	c.get("r3").working = true
	if f, _ := folderRow(c.rows(false, "", "")); !f.busy {
		t.Fatalf("folder = %+v, want it marked busy while a run is going", f)
	}
	out := ansi.Strip(renderSidebar(c, c.rows(false, "", ""), "", "", 12, 10))
	first := strings.Split(out, "\n")[0]
	if !strings.Contains(first, "jobs") || !strings.Contains(first, " 2 ") {
		t.Fatalf("first line = %q, want the folder with a badge of 2", first)
	}
	if w := ansi.StringWidth(first); w > 12 {
		t.Fatalf("the badged folder line is %d cells wide in a 12-cell sidebar: %q", w, first)
	}
}

// A closed folder shows nothing inside it, not even the selected run: the
// model keeps a run from being selected while the folder is closed, and a
// row left behind here is exactly the stale text a closed folder must not
// leave.
func TestAClosedFolderShowsNoRunsEvenTheSelectedOne(t *testing.T) {
	c := jobRuns()
	if got := strings.Join(ids(c.rows(false, "", "r2")), " "); got != "c1@0" {
		t.Fatalf("rows with r2 selected in a closed folder = %q, want only the chat", got)
	}
	c.jobsOpen = true
	if got := strings.Join(ids(c.rows(false, "", "r2")), " "); !strings.Contains(got, "r2@1") {
		t.Fatalf("rows with the folder open = %q, want r2 listed", got)
	}
}

func TestAFilterSearchesJobRunsToo(t *testing.T) {
	c := jobRuns()
	if got := strings.Join(ids(c.rows(false, "backup", "")), " "); got != "r2@1" {
		t.Fatalf("filter backup = %q, want the matching run", got)
	}
}
