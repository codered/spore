package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codered/spore/internal/models"
)

func TestPlainModelListsThenChoosesByNumber(t *testing.T) {
	v := models.View{
		Ops: []models.Op{
			{Op: "chat", Scope: "session", Selected: "a/one", Default: "a/one"},
			{Op: "compaction", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "title", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "classify", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "refinement", Scope: "global", Selected: "a/one", Default: "a/one"},
			{Op: "subagent", Scope: "session", Selected: "a/one", Default: "a/one"},
		},
		Groups: []models.Group{{Provider: "a", Refs: []string{"a/one", "a/two"}}},
	}
	var calls []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(body)))
		if r.Method == http.MethodPut {
			v.Ops[2].Selected = "a/two"
		}
		_ = json.NewEncoder(w).Encode(v)
	}))
	defer ts.Close()
	c := &client{base: ts.URL, short: ts.Client(), streamClient: ts.Client()}

	var out bytes.Buffer
	handled, err := runPlainSlash(context.Background(), c, "s1", "/model", false, &out)
	if err != nil || !handled {
		t.Fatalf("/model: handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out.String(), "-> a/one") {
		t.Fatalf("listing = %q", out.String())
	}

	out.Reset()
	handled, err = runPlainSlash(context.Background(), c, "s1", "/model title 2", false, &out)
	if err != nil || !handled {
		t.Fatalf("/model title 2: handled=%v err=%v", handled, err)
	}
	last := calls[len(calls)-1]
	if !strings.HasPrefix(last, "PUT /api/routing") || !strings.Contains(last, `"ref":"a/two"`) {
		t.Fatalf("last call = %q", last)
	}
	if !strings.Contains(out.String(), "title -> a/two") || !strings.Contains(out.String(), "every session") {
		t.Fatalf("confirm = %q", out.String())
	}
}

func TestPlainModelRejectsAFreeTextRef(t *testing.T) {
	c := &client{}
	handled, err := runPlainSlash(context.Background(), c, "s1", "/model chat a/two", false, &bytes.Buffer{})
	if !handled || err == nil {
		t.Fatalf("handled=%v err=%v, want a usage error", handled, err)
	}
}
