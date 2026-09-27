package refine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

func TestModelRequestRunsAfterTheTurnWithLatestInstructions(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	f.say(t, "user", text("hi"))
	if err := f.r.Request(context.Background(), f.sid, "first"); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Request(context.Background(), f.sid, "second"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.script.Requests()); n != 0 {
		t.Fatal("Request must not start a round by itself")
	}
	f.r.AfterTurn(f.sid)
	f.r.Wait()
	reqs := f.script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("rounds = %d, want 1", len(reqs))
	}
	if user := reqs[0].Messages[0].Blocks[0].Text; !strings.Contains(user, "second") || strings.Contains(user, "first") {
		t.Fatalf("the second request must replace the first:\n%s", user)
	}
	f.r.AfterTurn(f.sid) // flag was consumed
	f.r.Wait()
	if len(f.script.Requests()) != 1 {
		t.Fatal("AfterTurn without a request must not start a round")
	}
}

func TestRequestRefusesSubagentsAndDisabledConfig(t *testing.T) {
	f := newFix(t, store.SourceChat)
	child, _ := f.st.CreateChildSession(context.Background(), "c", "", f.sid)
	if err := f.r.Request(context.Background(), child, ""); !errors.Is(err, ErrSubagent) {
		t.Fatalf("err = %v, want ErrSubagent", err)
	}
	off := false
	f.cfg.Refine.Enabled = &off
	if err := f.r.Request(context.Background(), f.sid, ""); err == nil {
		t.Fatal("a disabled config must refuse the model's request")
	}
}

func TestAfterCompactReviewsTheFoldedRange(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	f.say(t, "user", text("FOLDED"))
	f.say(t, "user", text("KEPT"))
	f.r.AfterCompact(f.sid, 1)
	f.r.Wait()
	user := f.script.Requests()[0].Messages[0].Blocks[0].Text
	if !strings.Contains(user, "FOLDED") || strings.Contains(user, "KEPT") {
		t.Fatalf("compaction round must cover only the folded range:\n%s", user)
	}
}
