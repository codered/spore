package refine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/codered/spore/internal/store"
)

func TestUpdateSelfWritesAndLedgers(t *testing.T) {
	f := newFix(t, store.SourceChat)
	row, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "noted", func(cur string) (string, error) {
		if cur != "" {
			t.Errorf("cur = %q, want empty for an absent file", cur)
		}
		return "hello\n", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(f.cfg.SelfPath())
	if string(b) != "hello\n" {
		t.Fatalf("self.md = %q", b)
	}
	if row.Kind != KindSelfUpdate || row.Status != store.RefineApplied || row.Before != nil || row.Target != f.cfg.SelfPath() {
		t.Fatalf("row = %+v", row)
	}
}

func TestUpdateSelfRefusesOverTheCap(t *testing.T) {
	f := newFix(t, store.SourceChat)
	f.cfg.Companion.SelfMaxBytes = 1024
	_, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "big", func(string) (string, error) {
		return strings.Repeat("x", 1025), nil
	})
	if !errors.Is(err, ErrSelfTooLarge) {
		t.Fatalf("err = %v, want ErrSelfTooLarge", err)
	}
	if _, err := os.Stat(f.cfg.SelfPath()); !os.IsNotExist(err) {
		t.Fatal("an oversized self.md was written")
	}
	if rows, _ := f.st.Refinements(context.Background(), "", 10); len(rows) != 0 {
		t.Fatalf("a refused write was ledgered: %+v", rows)
	}
}

func TestSelfUpdateRollsBack(t *testing.T) {
	f := newFix(t, store.SourceChat)
	if err := os.WriteFile(f.cfg.SelfPath(), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.UpdateSelf(context.Background(), f.sid, TriggerTool, "noted", func(cur string) (string, error) {
		return cur + "after\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := f.r.Rollback(context.Background(), f.sid, "")
	if err != nil || len(out.RolledBack) != 1 {
		t.Fatalf("Rollback = %+v, %v", out, err)
	}
	b, _ := os.ReadFile(f.cfg.SelfPath())
	if string(b) != "before\n" {
		t.Fatalf("self.md after rollback = %q", b)
	}
}
