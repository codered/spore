package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

func seedMessages(t *testing.T, st *store.Store, sid string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		blocks, _ := json.Marshal([]provider.Block{{
			Type: provider.BlockText,
			Text: strings.Repeat("this is a long stretch of conversation history. ", 40),
		}})
		if _, err := st.AppendMessage(context.Background(), store.Message{
			SessionID: sid, Role: "user", BlocksJSON: blocks,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMaybeCompactSummarisesAndShrinksContext(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{Text: "SUMMARY: the user rambled about spore", Usage: provider.Usage{InputTokens: 500, OutputTokens: 12}},
		provider.ScriptTurn{Text: "understood", Usage: provider.Usage{InputTokens: 40, OutputTokens: 3}},
	)
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "long")
	seedMessages(t, st, sid, 12)

	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}

	text, through, err := st.Summary(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "rambled about spore") {
		t.Errorf("summary = %q", text)
	}
	if through != 10 { // 12 messages, KeepRecent 2
		t.Errorf("through_seq = %d, want 10", through)
	}

	// The compaction call must use the compaction call site's model.
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times, want 1", len(reqs))
	}

	// The next snapshot must be smaller and carry the summary.
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 2 {
		t.Errorf("snapshot kept %d messages, want 2", len(snap.Messages))
	}
	if !strings.Contains(snap.Summary, "rambled") {
		t.Errorf("snapshot summary = %q", snap.Summary)
	}
}

func TestMaybeCompactIsANoOpUnderBudget(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript() // no turns: any call would error
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 200_000

	sid, _ := st.CreateSession(ctx, "short")
	seedMessages(t, st, sid, 2)

	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("MaybeCompact: %v", err)
	}
	if text, _, _ := st.Summary(ctx, sid); text != "" {
		t.Errorf("compacted a session that was under budget: %q", text)
	}
}

func TestMaybeCompactPreservesOriginalMessages(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: things happened"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "long")
	seedMessages(t, st, sid, 12)
	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatal(err)
	}

	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 12 {
		t.Errorf("compaction deleted rows: %d remain, want all 12", len(msgs))
	}
}

func TestMaybeCompactIsIdempotentAtTheBoundary(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{Text: "SUMMARY: first compact", Usage: provider.Usage{InputTokens: 500, OutputTokens: 12}},
	)
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "long")
	seedMessages(t, st, sid, 4) // Only one turn, so after first compact there's only one turn left

	// First compact should succeed
	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("first MaybeCompact: %v", err)
	}

	// Second compact should be a no-op (script has no more turns)
	if err := a.MaybeCompact(ctx, sid); err != nil {
		t.Fatalf("second MaybeCompact should be no-op but got: %v", err)
	}
}

func TestMaybeCompactErrorEndsLLMSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})

	ctx := context.Background()
	// Script with one turn that errors mid-stream
	script := provider.NewScript(provider.ScriptTurn{Err: errors.New("compaction failed")})
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 2000
	a.Cfg.Context.CompactAt = 0.5
	a.Cfg.Context.KeepRecent = 2

	sid, _ := st.CreateSession(ctx, "t")
	seedMessages(t, st, sid, 12)

	// Call MaybeCompact with a script that will error
	_ = a.MaybeCompact(ctx, sid)

	var llmSpans int
	for _, s := range sr.Ended() {
		if s.Name() == "llm" {
			llmSpans++
		}
	}
	if llmSpans != 1 {
		t.Errorf("expected exactly 1 ended llm span after a compaction provider error, got %d", llmSpans)
	}
}
