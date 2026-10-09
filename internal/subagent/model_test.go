package subagent

import (
	"context"
	"errors"
	"testing"

	"github.com/codered/spore/internal/config"
)

type fakeChooser struct {
	ref string
	err error
	got []string
}

func (f *fakeChooser) ChildModel(_ context.Context, parentID, requested string) (string, error) {
	f.got = append(f.got, parentID+"|"+requested)
	return f.ref, f.err
}

func cfgOK() config.SubagentConfig {
	return config.SubagentConfig{MaxDepth: 3, MaxCostUSD: 10, MaxConcurrent: 4}
}

func TestRunModelStoresTheChosenModelOnTheChild(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	ch := &fakeChooser{ref: "p/small"}
	sup.SetModels(ch)
	parent := parentSession(t, st)

	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "p/small"); err != nil {
		t.Fatal(err)
	}
	if len(ch.got) != 1 || ch.got[0] != parent+"|p/small" {
		t.Fatalf("chooser calls = %v", ch.got)
	}
	kid, ok, err := st.Session(context.Background(), r.ran[0])
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if kid.ChatModel != "p/small" {
		t.Fatalf("child ChatModel = %q", kid.ChatModel)
	}
}

func TestARefusedModelLaunchesNothing(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	sup.SetModels(&fakeChooser{err: errors.New(`model "x/y" is not available; choose one of: p/a`)})
	parent := parentSession(t, st)

	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "x/y"); err == nil {
		t.Fatal("want the chooser's error")
	}
	if len(r.ran) != 0 {
		t.Fatal("a child ran")
	}
	sessions, err := st.ListSessions(context.Background(), 10, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want only the parent", len(sessions))
	}
	runs, _ := st.SubagentRunsByParent(context.Background(), parent)
	if len(runs) != 0 {
		t.Fatalf("run rows = %d, want 0", len(runs))
	}
}

func TestWithoutAChooserAModelIsRefusedAndNoneIsFine(t *testing.T) {
	r := &stubRunner{reply: "done"}
	sup, st := testSupervisor(t, cfgOK(), r)
	parent := parentSession(t, st)
	if _, err := sup.RunModel(ctxFor(parent), parent, "task", "p/x"); err == nil {
		t.Fatal("a model was accepted with nothing to check it against")
	}
	if _, err := sup.Run(ctxFor(parent), parent, "task"); err != nil {
		t.Fatal(err)
	}
}
