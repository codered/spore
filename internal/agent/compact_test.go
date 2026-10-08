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

func compactFixture(t *testing.T, messages int) (*Agent, string) {
	t.Helper()
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: conversation summary"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.MaxTokens = 100_000
	a.Cfg.Context.CompactAt = 0.75
	a.Cfg.Context.KeepRecent = 12
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	seedMessages(t, st, sid, messages)
	return a, sid
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

	sid, _ := st.CreateSession(ctx, "long", "")
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

	sid, _ := st.CreateSession(ctx, "short", "")
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

	sid, _ := st.CreateSession(ctx, "long", "")
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

	sid, _ := st.CreateSession(ctx, "long", "")
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

	sid, _ := st.CreateSession(ctx, "t", "")
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

func TestCompactFoldsBelowTheThreshold(t *testing.T) {
	// A session far below compact_at: MaybeCompact must do nothing, and
	// Compact must fold anyway. That difference is the whole command.
	a, id := compactFixture(t, 30) // 30 messages, tiny bodies
	if err := a.MaybeCompact(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, through, _ := a.Store.Summary(context.Background(), id); through != 0 {
		t.Fatal("MaybeCompact must not fold a session below the threshold")
	}
	folded, before, after, err := a.Compact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 30-a.Cfg.Context.KeepRecent {
		t.Fatalf("want %d folded, got %d", 30-a.Cfg.Context.KeepRecent, folded)
	}
	if after >= before {
		t.Fatalf("compaction must shrink the estimate: %d -> %d", before, after)
	}
	if _, through, _ := a.Store.Summary(context.Background(), id); through == 0 {
		t.Fatal("the summary boundary must have moved")
	}
}

func TestCompactWithNothingOutsideTheProtectedWindow(t *testing.T) {
	a, id := compactFixture(t, 3) // fewer than KeepRecent
	folded, before, after, err := a.Compact(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 0 {
		t.Fatalf("nothing should fold, got %d", folded)
	}
	if before != after {
		t.Fatal("a no-op compaction must not change the estimate")
	}
}

func appendNote(t *testing.T, st *store.Store, sid, text string) {
	t.Helper()
	blocks, _ := json.Marshal([]provider.Block{{Type: provider.BlockText, Text: text}})
	if _, err := st.AppendMessage(context.Background(), store.Message{SessionID: sid, Role: store.RoleNote, BlocksJSON: blocks}); err != nil {
		t.Fatal(err)
	}
}

// A note is written for the person reading the chat. The model never sees
// it, so the user/assistant alternation every provider checks is untouched.
func TestSnapshotLeavesNotesOut(t *testing.T) {
	a := newTestAgent(t)
	ctx := context.Background()
	sid, _ := a.Store.CreateSession(ctx, "t", "")
	seedMessages(t, a.Store, sid, 1)
	appendNote(t, a.Store, sid, "⏰ job 1 ran at 02:30 UTC — ok")
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 1 || snap.Messages[0].Role != provider.RoleUser {
		t.Fatalf("snapshot messages = %+v; want only the user message", snap.Messages)
	}
}

// Compact pairs stored rows with snapshot messages by position, so a note
// among the rows must be left out of both, or the fold cuts in the wrong
// place and summarises what the model never saw.
func TestCompactSkipsNotes(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: conversation summary"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.KeepRecent = 4
	ctx := context.Background()
	id, _ := st.CreateSession(ctx, "t", "")
	for i := 0; i < 10; i++ {
		seedMessages(t, st, id, 1)
		appendNote(t, st, id, "NOTE-TEXT job ran")
	}
	folded, _, _, err := a.Compact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 6 {
		t.Fatalf("folded %d, want 6: notes must not count toward the window", folded)
	}
	// The boundary is the sixth model message, seq 11, not the sixth row.
	if _, through, _ := st.Summary(ctx, id); through != 11 {
		t.Errorf("summary boundary = %d, want 11", through)
	}
	reqs := script.Requests()
	if len(reqs) != 1 {
		t.Fatalf("provider called %d times", len(reqs))
	}
	for _, m := range reqs[0].Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "NOTE-TEXT") {
				t.Fatal("a note reached the compaction model")
			}
		}
	}
}

type hookRecorder struct {
	compacted []int
	turns     int
}

func (h *hookRecorder) AfterCompact(_ string, through int) {
	h.compacted = append(h.compacted, through)
}
func (h *hookRecorder) AfterTurn(string) { h.turns++ }

func TestCompactAndTurnCallTheRefineHook(t *testing.T) {
	a, sid := compactFixture(t, 20)
	h := &hookRecorder{}
	a.Refine = h
	if _, _, _, err := a.Compact(context.Background(), sid); err != nil {
		t.Fatal(err)
	}
	if len(h.compacted) != 1 || h.compacted[0] != 8 { // 20 messages, KeepRecent 12
		t.Fatalf("AfterCompact calls = %v, want [8]", h.compacted)
	}

	script := provider.NewScript(provider.ScriptTurn{Text: "hello"})
	b, st := harness(t, script, nil)
	b.Refine = h
	sid2, _ := st.CreateSession(context.Background(), "t", "")
	ch, err := b.Run(context.Background(), sid2, "hi")
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	if h.turns != 1 {
		t.Fatalf("AfterTurn calls = %d, want 1", h.turns)
	}
}

// seedToolTurn stores one request followed by pairs of assistant tool calls
// and their results: the shape of a long agentic turn, which has a single
// user message at its head and nothing but tool traffic after it.
func seedToolTurn(t *testing.T, st *store.Store, sid, request string, pairs int) {
	t.Helper()
	ctx := context.Background()
	put := func(role string, b provider.Block) {
		raw, _ := json.Marshal([]provider.Block{b})
		if _, err := st.AppendMessage(ctx, store.Message{SessionID: sid, Role: role, BlocksJSON: raw}); err != nil {
			t.Fatal(err)
		}
	}
	put("user", provider.Block{Type: provider.BlockText, Text: request})
	for i := 0; i < pairs; i++ {
		id := "call" + strings.Repeat("x", i+1)
		put("assistant", provider.Block{Type: provider.BlockToolUse, ID: id, Name: "go_run", Input: json.RawMessage(`{}`)})
		put("tool", provider.Block{Type: provider.BlockToolResult, ID: id, Content: "ok"})
	}
}

// Compaction mid-turn can fold the turn's only user message into the
// summary. The summary lives in the system prompt, so the request would open
// on an assistant message with no user message at all -- which Qwen's chat
// template rejects outright ("No user query found in messages") and which
// Anthropic rejects too. The folded request is restated at the head instead.
func TestSnapshotRestatesAFoldedRequest(t *testing.T) {
	a := newTestAgent(t)
	ctx := context.Background()
	sid, _ := a.Store.CreateSession(ctx, "t", "")
	seedToolTurn(t, a.Store, sid, "review the code and write security_analysis.md", 10)
	// Rows 1..21; fold through 5, so the live window opens on row 6, an
	// assistant tool call.
	if err := a.Store.SetSummary(ctx, sid, "SUMMARY", 5); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	first := snap.Messages[0]
	if first.Role != provider.RoleUser || len(first.Blocks) != 1 ||
		!strings.Contains(first.Blocks[0].Text, "review the code and write security_analysis.md") {
		t.Fatalf("first message = %+v; want the folded request restated as a user message", first)
	}
	if got, want := len(snap.Messages), 21-5+1; got != want {
		t.Errorf("snapshot has %d messages, want %d (live rows plus the restated request)", got, want)
	}
}

// A live window that already opens on a user message is left alone.
func TestSnapshotDoesNotRestateWhenALiveRequestLeads(t *testing.T) {
	a := newTestAgent(t)
	ctx := context.Background()
	sid, _ := a.Store.CreateSession(ctx, "t", "")
	seedMessages(t, a.Store, sid, 6)
	if err := a.Store.SetSummary(ctx, sid, "SUMMARY", 3); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 3 {
		t.Fatalf("snapshot has %d messages, want 3", len(snap.Messages))
	}
}

// The fold must not separate a tool result from the call it answers: a
// result whose tool_use was summarised away is an orphan every provider
// rejects. When the protected window would open on a tool row, the call
// before it stays live too.
func TestCompactKeepsAToolResultWithItsCall(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: tool work"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.KeepRecent = 3 // odd, so a plain count would cut between a pair
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	seedToolTurn(t, st, sid, "do the work", 10) // rows 1..21

	if _, _, _, err := a.Compact(ctx, sid); err != nil {
		t.Fatal(err)
	}
	_, through, err := st.Summary(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if through != 17 { // 21-3 = 18 would leave row 19, a tool result, leading
		t.Errorf("through = %d, want 17", through)
	}
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Messages[0].Role != provider.RoleUser {
		t.Fatalf("first message role = %q, want user", snap.Messages[0].Role)
	}
	if snap.Messages[1].Role != provider.RoleAssistant {
		t.Errorf("second message role = %q, want the assistant call", snap.Messages[1].Role)
	}
}

// A second compaction summarises live rows, never the restated request: the
// restatement is assembled per request and is not part of the history.
func TestCompactDoesNotFoldTheRestatement(t *testing.T) {
	script := provider.NewScript(provider.ScriptTurn{Text: "SUMMARY: more tool work"})
	a, st := harness(t, script, nil)
	a.Cfg.Context.KeepRecent = 2
	ctx := context.Background()
	sid, _ := st.CreateSession(ctx, "t", "")
	seedToolTurn(t, st, sid, "do the work", 10) // rows 1..21
	if err := st.SetSummary(ctx, sid, "SUMMARY", 5); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.Compact(ctx, sid); err != nil {
		t.Fatal(err)
	}
	_, through, err := st.Summary(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if through != 19 { // live rows 6..21, keep the last 2
		t.Errorf("through = %d, want 19", through)
	}
	reqs := script.Requests()
	transcript := reqs[0].Messages[0].Blocks[0].Text
	if strings.Contains(transcript, "do the work") {
		t.Errorf("compaction transcript repeats the restated request:\n%s", transcript)
	}
}
