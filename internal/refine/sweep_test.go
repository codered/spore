package refine

import (
	"context"
	"testing"
	"time"

	"github.com/codered/spore/internal/store"
)

func TestSweepRefinesIdleUntrustedSessionAsProposals(t *testing.T) {
	f := newFix(t, store.SourceDiscord, createTabs)
	f.say(t, "user", text("remember I like tabs"))
	if err := f.r.Sweep(context.Background(), time.Now().Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	f.r.Wait()
	if _, ok := read(t, f.factPath("prefers-tabs")); ok {
		t.Fatal("a swept Discord session wrote a fact")
	}
	if rows, _ := f.st.Refinements(context.Background(), store.RefineProposed, 10); len(rows) != 1 {
		t.Fatalf("proposed = %+v", rows)
	}
}

func TestSweepSkipsSessionsWithNothingNew(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	f.say(t, "user", text("hi"))
	later := time.Now().Add(time.Hour)
	_ = f.r.Sweep(context.Background(), later, nil)
	f.r.Wait()
	_ = f.r.Sweep(context.Background(), later, nil)
	f.r.Wait()
	if n := len(f.script.Requests()); n != 1 {
		t.Fatalf("provider called %d times across two sweeps, want 1", n)
	}
}

func TestSweepSkipsActiveAndRunningSessions(t *testing.T) {
	f := newFix(t, store.SourceChat, `{"edits":[]}`)
	f.say(t, "user", text("hi"))
	_ = f.r.Sweep(context.Background(), time.Now().Add(-time.Hour), nil) // not idle yet
	_ = f.r.Sweep(context.Background(), time.Now().Add(time.Hour), func(string) bool { return true })
	f.r.Wait()
	if n := len(f.script.Requests()); n != 0 {
		t.Fatalf("provider called %d times", n)
	}
	off := false
	f.cfg.Refine.Enabled = &off
	_ = f.r.Sweep(context.Background(), time.Now().Add(time.Hour), nil)
	f.r.Wait()
	if n := len(f.script.Requests()); n != 0 {
		t.Fatal("a disabled config must not sweep")
	}
}
