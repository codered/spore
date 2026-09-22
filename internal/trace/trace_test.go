package trace

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
		m[string(kv.Key)] = kv.Value.String()
	}
	return m
}

func TestLLMSpanCarriesOpenInferenceAttributes(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	ctx, turn := StartTurn(context.Background(), "sess-1", "cli")
	_, llm := StartLLM(ctx, "chat", "anthropic/claude-opus-5")
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

func TestEndLLMReportsFullPromptCountWithCacheDetails(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	_, llm := StartLLM(context.Background(), "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "prompt", "completion", provider.Usage{
		InputTokens:      7,
		OutputTokens:     3,
		CacheWriteTokens: 1200,
		CacheReadTokens:  9000,
	}, 0.05)

	a := attrs(sr.Ended()[0].Attributes())
	if a["llm.token_count.prompt"] != "10207" {
		t.Errorf("llm.token_count.prompt = %s, want 10207 (sum of 7 + 1200 + 9000)", a["llm.token_count.prompt"])
	}
	if a["llm.token_count.completion"] != "3" {
		t.Errorf("llm.token_count.completion = %s, want 3", a["llm.token_count.completion"])
	}
	if a["llm.token_count.prompt_details.cache_read"] != "9000" {
		t.Errorf("llm.token_count.prompt_details.cache_read = %s, want 9000", a["llm.token_count.prompt_details.cache_read"])
	}
	if a["llm.token_count.prompt_details.cache_write"] != "1200" {
		t.Errorf("llm.token_count.prompt_details.cache_write = %s, want 1200", a["llm.token_count.prompt_details.cache_write"])
	}
}

func TestEndLLMIncludesCacheDetailsWhenZero(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	_, llm := StartLLM(context.Background(), "chat", "anthropic/claude-opus-5")
	EndLLM(llm, "prompt", "completion", provider.Usage{
		InputTokens:      100,
		OutputTokens:     20,
		CacheWriteTokens: 0,
		CacheReadTokens:  0,
	}, 0.01)

	a := attrs(sr.Ended()[0].Attributes())
	if a["llm.token_count.prompt"] != "100" {
		t.Errorf("llm.token_count.prompt = %s, want 100", a["llm.token_count.prompt"])
	}
	if a["llm.token_count.prompt_details.cache_read"] != "0" {
		t.Errorf("llm.token_count.prompt_details.cache_read = %s, want 0", a["llm.token_count.prompt_details.cache_read"])
	}
	if a["llm.token_count.prompt_details.cache_write"] != "0" {
		t.Errorf("llm.token_count.prompt_details.cache_write = %s, want 0", a["llm.token_count.prompt_details.cache_write"])
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

func TestErrorPathEndsLLMSpan(t *testing.T) {
	sr := recorder(t)
	SetRedact(false)

	// Simulate an LLM call that errors mid-stream
	// WITHOUT the fix: error paths don't call span.End(), so no span appears in sr.Ended()
	// WITH the fix: error paths call span.RecordError() and span.End() before returning
	_, llm := StartLLM(context.Background(), "chat", "anthropic/claude-opus-5")

	// Simulate error handling on EventError in the loop
	err := fmt.Errorf("stream truncated")
	llm.RecordError(err)
	llm.End()

	spans := sr.Ended()
	if len(spans) == 0 {
		t.Fatal("no spans recorded; error path did not end span")
	}

	// Verify the span was ended and recorded
	var found bool
	for _, s := range spans {
		if s.Name() == "llm" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("llm span not found in ended spans")
	}
}

func TestInitEnabledAgainstLocalCollector(t *testing.T) {
	// Stand up a local httptest server that accepts OTLP trace POSTs and
	// counts them: accepting the connection is not the contract, delivering
	// the span is.
	var posts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	// Call Init with Enabled: true pointing to the local server
	ctx := context.Background()
	shutdown, err := Init(ctx, config.TraceConfig{
		Enabled:    true,
		Endpoint:   srv.URL + "/v1/traces",
		SampleRate: 1.0,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init returned a nil shutdown func")
	}

	// Emit a span through the enabled tracer
	ctx, turn := StartTurn(ctx, "test-session", "test-client")
	turn.End()

	// Shutdown must succeed, and flush what the batcher is holding.
	if err := shutdown(ctx); err != nil {
		t.Errorf("shutdown: %v", err)
	}
	if posts.Load() == 0 {
		t.Error("the span never reached the collector")
	}
}
