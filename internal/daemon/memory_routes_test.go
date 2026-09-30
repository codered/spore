package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/recall/sqlitefts"
)

type failingUnindex struct{}

func (failingUnindex) UnindexFact(context.Context, string) error { return errors.New("index down") }

func attachMemory(t *testing.T, s *Server, idx FactIndexer) *memory.Cache {
	t.Helper()
	dir := s.cfg.MemoryDir()
	f := memory.Fact{Name: "prefers-tabs", Type: "feedback", Description: "indentation", Body: "Use tabs, never spaces."}
	if err := memory.Write(dir, f); err != nil {
		t.Fatal(err)
	}
	if err := s.Store().IndexFact(context.Background(), f.Name, f.Description+"\n"+f.Body); err != nil {
		t.Fatal(err)
	}
	facts := memory.NewCache(dir)
	facts.Reload()
	s.AttachOperator(Operator{Facts: facts, FactIndex: idx, Recall: sqlitefts.New(s.Store().DB())})
	return facts
}

func TestMemoryListSearchAndDelete(t *testing.T) {
	s, ts := newTestServer(t)
	facts := attachMemory(t, s, s.Store())
	logs := captureLog(t)

	code, body := send(t, "GET", ts.URL+"/api/memory", nil)
	var list MemoryJSON
	_ = json.Unmarshal([]byte(body), &list)
	if code != 200 || len(list.Facts) != 1 || list.Facts[0].Body != "Use tabs, never spaces." {
		t.Fatalf("list = %d %s", code, body)
	}

	code, body = send(t, "GET", ts.URL+"/api/memory?q=tabs", nil)
	var hits MemoryJSON
	_ = json.Unmarshal([]byte(body), &hits)
	if code != 200 || hits.Query != "tabs" || len(hits.Hits) != 1 || hits.Hits[0].Name != "prefers-tabs" {
		t.Fatalf("search = %d %s", code, body)
	}
	for _, q := range []string{`"tabs`, `tabs*`, `-x`, `a OR`} {
		if code, body = send(t, "GET", ts.URL+"/api/memory?q="+url.QueryEscape(q), nil); code >= 500 {
			t.Errorf("query %q = %d %s", q, code, body)
		}
	}

	if code, body = send(t, "DELETE", ts.URL+"/api/memory/prefers-tabs", nil, "X-Spore-Client", "tui"); code != 200 {
		t.Fatalf("delete = %d %s", code, body)
	}
	if _, err := os.Stat(s.cfg.MemoryDir() + "/prefers-tabs.md"); !os.IsNotExist(err) {
		t.Error("fact file still exists")
	}
	if len(facts.Facts()) != 0 {
		t.Error("the fact cache was not reloaded")
	}
	var tombs int
	_ = s.Store().DB().QueryRow(`SELECT count(*) FROM recall_tombstones WHERE kind = 'fact' AND ref_id = 'prefers-tabs'`).Scan(&tombs)
	if tombs != 1 {
		t.Errorf("tombstones = %d, want 1", tombs)
	}
	if !strings.Contains(logs.String(), "action=delete_fact") {
		t.Errorf("no audit line:\n%s", logs.String())
	}
	if code, _ = send(t, "DELETE", ts.URL+"/api/memory/prefers-tabs", nil); code != 404 {
		t.Errorf("second delete = %d, want 404", code)
	}
	if code, _ = send(t, "DELETE", ts.URL+"/api/memory/..%2Fconfig", nil); code != 400 {
		t.Errorf("traversal name = %d, want 400", code)
	}
}

func TestDeleteFactSucceedsWhenUnindexFails(t *testing.T) {
	s, ts := newTestServer(t)
	attachMemory(t, s, failingUnindex{})
	if code, body := send(t, "DELETE", ts.URL+"/api/memory/prefers-tabs", nil); code != 200 {
		t.Fatalf("delete = %d %s; the file is gone, so the delete succeeded", code, body)
	}
}

func TestMemoryUnwired(t *testing.T) {
	s, ts := newTestServer(t)
	if code, _ := send(t, "GET", ts.URL+"/api/memory", nil); code != http.StatusServiceUnavailable {
		t.Errorf("unwired = %d, want 503", code)
	}
	facts := memory.NewCache(s.cfg.MemoryDir())
	s.AttachOperator(Operator{Facts: facts})
	code, body := send(t, "GET", ts.URL+"/api/memory?q=x", nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, "recall search is unavailable") {
		t.Errorf("search without recall = %d %s", code, body)
	}
}
