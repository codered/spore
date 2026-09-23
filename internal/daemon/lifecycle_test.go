package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

type fakeCleaner struct {
	got   [][]string
	notes []string
}

func (f *fakeCleaner) ForgetSessions(_ context.Context, sessions []store.Session) []string {
	var ids []string
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	f.got = append(f.got, ids)
	return f.notes
}

func deleteSessions(t *testing.T, url string, body map[string]any) (int, DeleteSessionsJSON) {
	t.Helper()
	res := postJSON(t, url+"/api/sessions/delete", body)
	defer res.Body.Close()
	var out DeleteSessionsJSON
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestDeletingASessionRemovesItsTreeAndTellsEveryClient(t *testing.T) {
	s, ts := newTestServer(t)
	ctx := context.Background()
	root, _ := s.store.CreateSessionFrom(ctx, "root", "", store.SourceChat)
	kid, err := s.store.CreateChildSession(ctx, "kid", "/ws", root)
	if err != nil {
		t.Fatal(err)
	}
	feed, stop := s.hub.SubscribeAll()
	defer stop()

	code, out := deleteSessions(t, ts.URL, map[string]any{"ids": []string{root}})
	if code != http.StatusOK || len(out.Deleted) != 2 {
		t.Fatalf("status %d, deleted %v; want 200 and the root with its sub-agent", code, out.Deleted)
	}
	for _, id := range []string{root, kid} {
		if _, found, _ := s.store.Session(ctx, id); found {
			t.Errorf("session %s is still stored", id)
		}
	}
	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case ev := <-feed:
			if ev.Type == WireSessionDeleted {
				got[ev.Session] = true
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("session_deleted events = %v, want one for %s and %s", got, root, kid)
		}
	}
}

func TestDeletingASessionWhoseTreeIsRunningIsRefused(t *testing.T) {
	s, ts := newTestServer(t)
	ctx := context.Background()
	root, _ := s.store.CreateSessionFrom(ctx, "root", "", store.SourceChat)
	kid, _ := s.store.CreateChildSession(ctx, "kid", "/ws", root)
	if !s.hub.Begin(kid) {
		t.Fatal("could not claim the child's turn slot")
	}
	defer s.hub.End(kid)

	code, out := deleteSessions(t, ts.URL, map[string]any{"ids": []string{root}})
	if code != http.StatusConflict {
		t.Fatalf("status %d (%+v), want 409 while a turn runs in the tree", code, out)
	}
	if _, found, _ := s.store.Session(ctx, root); !found {
		t.Fatal("a refused delete removed the session anyway")
	}
}

func TestDeleteAllRemovesEverySession(t *testing.T) {
	s, ts := newTestServer(t)
	ctx := context.Background()
	for _, title := range []string{"a", "b", "c"} {
		if _, err := s.store.CreateSessionFrom(ctx, title, "", store.SourceChat); err != nil {
			t.Fatal(err)
		}
	}
	code, out := deleteSessions(t, ts.URL, map[string]any{"all": true})
	if code != http.StatusOK || len(out.Deleted) != 3 {
		t.Fatalf("status %d, deleted %v; want 200 and all three", code, out.Deleted)
	}
	if left, _ := s.store.ListSessions(ctx, 10, true); len(left) != 0 {
		t.Fatalf("%d sessions left", len(left))
	}
}

func TestDeleteNeedsIdsOrAll(t *testing.T) {
	_, ts := newTestServer(t)
	if code, _ := deleteSessions(t, ts.URL, map[string]any{}); code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 for a delete that names nothing", code)
	}
}

func TestDeleteAsksTheCleanerOnlyWhenDiscordIsRequested(t *testing.T) {
	s, ts := newTestServer(t)
	ctx := context.Background()
	cl := &fakeCleaner{notes: []string{"deleted Discord thread 42"}}
	s.SetCleaner(cl)
	a, _ := s.store.CreateSessionFrom(ctx, "a", "", store.SourceDiscord)
	b, _ := s.store.CreateSessionFrom(ctx, "b", "", store.SourceDiscord)

	if code, _ := deleteSessions(t, ts.URL, map[string]any{"ids": []string{a}}); code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(cl.got) != 0 {
		t.Fatalf("cleaner called %v without discord requested", cl.got)
	}
	code, out := deleteSessions(t, ts.URL, map[string]any{"ids": []string{b}, "discord": true})
	if code != http.StatusOK || len(cl.got) != 1 || len(cl.got[0]) != 1 || cl.got[0][0] != b {
		t.Fatalf("status %d, cleaner got %v; want it asked about %s", code, cl.got, b)
	}
	if len(out.Notes) != 1 || out.Notes[0] != "deleted Discord thread 42" {
		t.Fatalf("notes = %v, want the cleaner's report passed back", out.Notes)
	}
}

func TestDiscordDeleteWithoutABridgeSaysSo(t *testing.T) {
	s, ts := newTestServer(t)
	a, _ := s.store.CreateSessionFrom(context.Background(), "a", "", store.SourceDiscord)
	code, out := deleteSessions(t, ts.URL, map[string]any{"ids": []string{a}, "discord": true})
	if code != http.StatusOK || len(out.Notes) != 1 {
		t.Fatalf("status %d, notes %v; want the delete to go ahead with a note that Discord was not touched", code, out.Notes)
	}
}

func TestAJobRunIsTaggedWithItsJobAndUnreadUntilSeen(t *testing.T) {
	s, ts := newTestServer(t, provider.ScriptTurn{Text: "a joke"})
	ctx := context.Background()
	sid, err := s.StartJob(ctx, store.Job{ID: 9, Prompt: "tell a joke"})
	if err != nil {
		t.Fatal(err)
	}
	var sess SessionJSON
	for deadline := time.Now().Add(5 * time.Second); ; {
		list := listSessions(t, ts.URL)
		for _, j := range list {
			if j.ID == sid {
				sess = j
			}
		}
		if (sess.Unread && sess.State == SessionIdle) || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sess.JobID != 9 || !sess.Unread {
		t.Fatalf("job run = %+v, want job 9 and unread once it has replied", sess)
	}
	res := postJSON(t, ts.URL+"/api/sessions/"+sid+"/seen", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("seen: status %d", res.StatusCode)
	}
	for _, j := range listSessions(t, ts.URL) {
		if j.ID == sid && j.Unread {
			t.Fatal("the run still reads unread after it was seen")
		}
	}
}

func listSessions(t *testing.T, url string) []SessionJSON {
	t.Helper()
	res, err := http.Get(url + "/api/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out []SessionJSON
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
