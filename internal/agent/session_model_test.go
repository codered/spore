package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
)

func TestChatAndSubagentTurnsUseTheSessionModel(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: "a"}, provider.ScriptTurn{Text: "b"}, provider.ScriptTurn{Text: "c"})
	a, st := harness(t, script, nil)
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "/ws")

	collect(t, mustRun(t, a, sid, router.SiteChat))
	if err := st.SetSessionModel(ctx, sid, "chat", "test/model-b"); err != nil {
		t.Fatal(err)
	}
	collect(t, mustRun(t, a, sid, router.SiteChat))
	collect(t, mustRun(t, a, sid, router.SiteSubagent))

	reqs := script.Requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d", len(reqs))
	}
	if reqs[0].Model != "model-a" || reqs[1].Model != "model-b" || reqs[2].Model != "model-b" {
		t.Fatalf("models = %q, %q, %q; want model-a, then the session's model-b for chat and subagent",
			reqs[0].Model, reqs[1].Model, reqs[2].Model)
	}
}

func TestARemovedProviderFailsTheTurnLoudly(t *testing.T) {
	a, st := harness(t, provider.NewScript(provider.ScriptTurn{Text: "never"}), nil)
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "/ws")
	if err := st.SetSessionModel(ctx, sid, "chat", "gone/model"); err != nil {
		t.Fatal(err)
	}
	// collect would fail the test on the error event, so drain the channel
	// directly and read the error it carries.
	var failed error
	for ev := range mustRun(t, a, sid, router.SiteChat) {
		if ev.Type == EvError {
			failed = ev.Err
		}
	}
	if failed == nil || !strings.Contains(failed.Error(), `session model "gone/model"`) || !strings.Contains(failed.Error(), "/model") {
		t.Fatalf("err = %v, want it to name the session model and /model", failed)
	}
}

func mustRun(t *testing.T, a *Agent, sid, site string) <-chan Event {
	t.Helper()
	ch, err := a.RunSite(context.Background(), sid, "hi", site)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}
