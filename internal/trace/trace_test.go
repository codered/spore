package trace

import (
	"context"
	"testing"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/provider"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { tp.Shutdown(context.Background()) })
	return sr
}

func attrs(kvs []attribute.KeyValue) map[string]string {
	m := map[string]string{}
	for _, kv := range kvs {
		m[string(kv.Key)] = kv.Value.Emit()
	}
	return m
}

func TestLLMSpanCarriesOpenInferenceAttributes(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	ctx, turn := StartTurn(context.Background(), "sess-1", "cli")
	ctx, llm := StartLLM(ctx, "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "what module is this?", "spore", provider.Usage{InputTokens: 100, OutputTokens: 20}, 0.0021)
	turn.End()

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("recorded %d spans, want 2", len(spans))
	}
	var llmSpan sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "llm" {
			llmSpan = s
		}
	}
	if llmSpan == nil {
		t.Fatal("no span named llm")
	}
	a := attrs(llmSpan.Attributes())
	if a["openinference.span.kind"] != "LLM" {
		t.Errorf("span kind = %q", a["openinference.span.kind"])
	}
	if a["llm.model_name"] != "anthropic/claude-opus-5" || a["spore.call_site"] != "chat" {
		t.Errorf("attrs = %+v", a)
	}
	if a["llm.token_count.prompt"] != "100" || a["llm.token_count.completion"] != "20" {
		t.Errorf("token attrs = %+v", a)
	}
	if a["input.value"] != "what module is this?" || a["output.value"] != "spore" {
		t.Errorf("io attrs = %+v", a)
	}
	// The llm span must be a child of the turn span.
	if !llmSpan.Parent().IsValid() {
		t.Error("llm span has no parent")
	}
}

func TestRedactDropsPromptAndCompletionButKeepsCounts(t *testing.T) {
	sr := recorder(t)
	SetRedact(true)
	t.Cleanup(func() { SetRedact(false) })

	_, llm := StartLLM(context.Background(), "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "secret prompt", "secret completion", provider.Usage{InputTokens: 7, OutputTokens: 3}, 0.1)

	a := attrs(sr.Ended()[0].Attributes())
	if _, ok := a["input.value"]; ok {
		t.Error("input.value present under redaction")
	}
	if _, ok := a["output.value"]; ok {
		t.Error("output.value present under redaction")
	}
	if a["llm.token_count.prompt"] != "7" {
		t.Errorf("token counts must survive redaction: %+v", a)
	}
}

func TestInitDisabledIsANoOpWithUsableShutdown(t *testing.T) {
	shutdown, err := Init(context.Background(), config.TraceConfig{Enabled: false})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown func")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
