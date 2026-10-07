package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func TestTurnRequestsTheConfiguredOutputLimit(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{Text: "hi"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxOutputTokens = 12345
	sid, _ := st.CreateSession(ctx, "t", "")
	ch, err := a.Run(ctx, sid, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	collect(t, ch)
	reqs := script.Requests()
	if len(reqs) != 1 || reqs[0].MaxTokens != 12345 {
		t.Fatalf("requests = %d, MaxTokens = %v; want one request asking for 12345", len(reqs), maxTokensOf(reqs))
	}
}

// A reasoning model can spend the whole output budget thinking and reply
// with nothing. Ending the turn as if it had answered left the user with a
// blank reply and no clue; the turn must fail and name the setting.
func TestEmptyReplyAtTheOutputLimitFailsTheTurn(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{HitMaxTokens: true, Usage: provider.Usage{OutputTokens: 4096}})
	a, st := harness(t, script, nil)
	sid, _ := st.CreateSession(ctx, "t", "")
	ch, err := a.Run(ctx, sid, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var failed error
	for _, ev := range drainAll(ch) {
		if ev.Type == EvTurnDone {
			t.Fatal("the turn ended as if the model had answered")
		}
		if ev.Type == EvError {
			failed = ev.Err
		}
	}
	if failed == nil || !strings.Contains(failed.Error(), "context.max_output_tokens") {
		t.Fatalf("turn error = %v, want one naming context.max_output_tokens", failed)
	}
}

// A reply that has text is shown even when the limit cut it short: the
// text is still the model's answer.
func TestReplyWithTextAtTheOutputLimitStillCompletes(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{Text: "partial answer", HitMaxTokens: true})
	a, st := harness(t, script, nil)
	sid, _ := st.CreateSession(ctx, "t", "")
	ch, err := a.Run(ctx, sid, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	done := false
	for _, ev := range collect(t, ch) {
		if ev.Type == EvError {
			t.Fatalf("turn failed: %v", ev.Err)
		}
		done = done || ev.Type == EvTurnDone
	}
	if !done {
		t.Fatal("the turn did not complete")
	}
}

func maxTokensOf(reqs []provider.Request) []int {
	var out []int
	for _, r := range reqs {
		out = append(out, r.MaxTokens)
	}
	return out
}
