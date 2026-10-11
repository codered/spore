package companion_test

import (
	"context"
	"testing"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
)

func TestLLMReflectorUsesTheCompanionSite(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: `{"action":"none"}`})
	reg := provider.NewRegistry()
	reg.Register("script", script, provider.ProviderPrice{})
	rt, err := router.New([]config.Route{{When: "companion", Model: "script/reflect"}}, "script/chat")
	if err != nil {
		t.Fatal(err)
	}
	r := companion.LLMReflector{Registry: reg, Router: rt, MaxTokens: 4096}
	got, err := r.Reflect(context.Background(), "the system", "the input")
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"action":"none"}` {
		t.Fatalf("reply = %q", got)
	}
	reqs := script.Requests()
	if len(reqs) != 1 || reqs[0].Model != "reflect" || reqs[0].System[0].Text != "the system" ||
		reqs[0].Messages[0].Blocks[0].Text != "the input" || reqs[0].MaxTokens != 4096 {
		t.Fatalf("request = %+v", reqs)
	}
}
