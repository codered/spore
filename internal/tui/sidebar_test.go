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

	got := strings.Join(ids(c.rows(false, "", "")), " ")
	if got != "c1@0 k1@1 d2@0 j1@0" {
		t.Fatalf("default rows = %q, want chat with its child, then the blocked and working sessions", got)
	}
	if all := strings.Join(ids(c.rows(true, "", "")), " "); !strings.Contains(all, "d1@0") {
		t.Fatalf(":sessions all = %q, want the idle discord session too", all)
	}
	if sel := strings.Join(ids(c.rows(false, "", "d1")), " "); !strings.Contains(sel, "d1@0") {
		t.Fatalf("rows with d1 selected = %q, the selection must always show", sel)
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
	out := ansi.Strip(renderSidebar(c, c.rows(false, "", ""), "", 30, 10))
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
	output := ansi.Strip(renderSidebar(c, c.rows(false, "", deepestID), deepestID, 30, 20))
	for i, line := range strings.Split(output, "\n") {
		if w := ansi.StringWidth(line); w > 30 {
			t.Errorf("line %d is %d cells wide, exceeds 30-cell budget: %q", i, w, line)
		}
	}
}
