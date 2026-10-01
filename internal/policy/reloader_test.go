package policy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

type reloaderFix struct {
	g    *Guard
	rl   *Reloader
	ap   *scriptedApprover
	path string
	ctx  context.Context
}

func newReloaderFix(t *testing.T) *reloaderFix {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// fs_write is not in the Ask list, so it falls to the profile default (ask).
	// A learned allow rule can apply because the hand-written ask rule
	// does not directly match fs_write.
	pc := config.PolicyConfig{Default: "ask", ApprovalTimeout: "5m", Workspace: "/ws"}
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sid, err := st.CreateSession(context.Background(), "t", "")
	if err != nil {
		t.Fatal(err)
	}
	ap := &scriptedApprover{answer: Answer{Allow: true, Scope: ScopePattern}}
	var rl *Reloader
	g := NewGuard(&recordingRunner{}, engine(t, pc), ap, st, func(d Decision, rule string) error { return rl.Learn(d, rule) })
	rl = NewReloader(path, pc, g)
	ctx := WithSession(context.Background(), Session{ID: sid, Profile: ProfileLocal, Workspace: "/ws"})
	return &reloaderFix{g: g, rl: rl, ap: ap, path: path, ctx: ctx}
}

func (f *reloaderFix) evaluate(tool string) Decision {
	return f.g.Engine().Evaluate(Session{Profile: ProfileLocal}, Call{Tool: tool, Args: []byte(`{}`)}).Decision
}

// The bug this fixes: "always allow this pattern" wrote the rule to the
// config file, but the running engine never saw it until a restart.
func TestAPatternAnswerAppliesToTheVeryNextCall(t *testing.T) {
	f := newReloaderFix(t)
	call := toolCall("fs_write", "c1", `{"path":"/ws/src/a.go"}`)
	f.g.Run(f.ctx, call)
	if f.ap.count() != 1 {
		t.Fatalf("first call asked %d times, want 1", f.ap.count())
	}
	f.g.Run(f.ctx, toolCall("fs_write", "c2", `{"path":"/ws/src/a.go"}`))
	if f.ap.count() != 1 {
		t.Fatalf("the learned rule did not apply without a restart: asked %d times", f.ap.count())
	}

	learned, err := config.ReadLearned(f.path)
	if err != nil || len(learned.Allow) != 1 {
		t.Fatalf("learned = %+v %v", learned, err)
	}
	if err := f.rl.Unlearn(DecisionAllow, learned.Allow[0]); err != nil {
		t.Fatal(err)
	}
	f.g.Run(f.ctx, toolCall("fs_write", "c3", `{"path":"/ws/src/a.go"}`))
	if f.ap.count() != 2 {
		t.Fatalf("after revoke the call must ask again: asked %d times", f.ap.count())
	}
}

func TestARebuildFailureKeepsTheOldEngine(t *testing.T) {
	f := newReloaderFix(t)
	before := f.g.Engine()
	// LearnRule accepts any text without quotes or newlines; the engine
	// refuses a predicate it does not know.
	if err := f.rl.Learn(DecisionAllow, "fs_write(no such predicate)"); err == nil {
		t.Fatal("a rule the engine cannot parse must be reported")
	}
	if f.g.Engine() != before {
		t.Error("a failed rebuild replaced the engine")
	}
}

func TestUnlearnAfterAHandEditRealignsTheEngine(t *testing.T) {
	f := newReloaderFix(t)
	if err := f.rl.Learn(DecisionAllow, "probe_tool"); err != nil {
		t.Fatal(err)
	}
	if f.evaluate("probe_tool") != DecisionAllow {
		t.Fatal("learned rule not live")
	}
	// The operator deletes the block by hand while the daemon runs.
	if err := os.WriteFile(f.path, []byte("[policy]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.rl.Unlearn(DecisionAllow, "probe_tool"); !errors.Is(err, config.ErrNotLearned) {
		t.Fatalf("err = %v, want ErrNotLearned", err)
	}
	if f.evaluate("probe_tool") != DecisionAsk {
		t.Error("the engine still holds a rule the file no longer has")
	}
}

func TestRevokingALearnedDenyRemovesItFromEveryProfile(t *testing.T) {
	f := newReloaderFix(t)
	if err := f.rl.Learn(DecisionDeny, "probe_tool"); err != nil {
		t.Fatal(err)
	}
	if err := f.rl.Unlearn(DecisionDeny, "probe_tool"); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.g.Engine().Rules() {
		if r.Rule == "probe_tool" {
			t.Errorf("revoked deny still listed under %s", r.Profile)
		}
	}
}

func TestConcurrentLearnsBothLand(t *testing.T) {
	f := newReloaderFix(t)
	var wg sync.WaitGroup
	for _, tool := range []string{"probe_a", "probe_b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f.rl.Learn(DecisionAllow, tool); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if f.evaluate("probe_a") != DecisionAllow || f.evaluate("probe_b") != DecisionAllow {
		t.Error("one of two concurrent learns is missing from the live engine")
	}
}
