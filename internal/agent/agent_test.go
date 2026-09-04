package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// fakeTools answers every call with a fixed result and records what it saw.
type fakeTools struct {
	calls  []provider.Block
	result string
}

func (f *fakeTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (f *fakeTools) ReadOnly(string) bool { return true }
func (f *fakeTools) Run(_ context.Context, call provider.Block) provider.Block {
	f.calls = append(f.calls, call)
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: f.result}
}

func harness(t *testing.T, script *provider.Script, tools ToolRunner) (*Agent, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "spore.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	reg := provider.NewRegistry()
	reg.Register("test", script, provider.ProviderPrice{In: 1, Out: 2})

	cfg := config.Default()
	cfg.DefaultModel = "test/model-a"
	cfg.SystemPrompt = "you are spore"

	rt, err := router.New(nil, cfg.DefaultModel)
	if err != nil {
		t.Fatal(err)
	}
	return New(st, reg, rt, cfg, tools), st
}

// newTestAgent builds an agent with no scripted turns and no tools, for tests
// that only need Store and Cfg (e.g. Snapshot) and never call Run.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	a, _ := harness(t, provider.NewScript(), nil)
	return a
}

func collect(t *testing.T, ch <-chan Event) []Event {
	t.Helper()
	var out []Event
	for ev := range ch {
		if ev.Type == EvError {
			t.Fatalf("error event: %v", ev.Err)
		}
		out = append(out, ev)
	}
	return out
}

func TestRunSingleTurnPersistsAndReportsCost(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(provider.ScriptTurn{
		Text:  "hello there",
		Usage: provider.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000},
	})
	a, st := harness(t, script, nil)

	sid, err := st.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	ch, err := a.Run(ctx, sid, "hi")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	var text string
	var done *Event
	for i, ev := range events {
		if ev.Type == EvText {
			text += ev.Text
		}
		if ev.Type == EvTurnDone {
			done = &events[i]
		}
	}
	if text != "hello there" {
		t.Errorf("streamed text = %q", text)
	}
	if done == nil {
		t.Fatal("no EvTurnDone event")
	}
	if done.Model != "test/model-a" || done.Cost != 3 { // 1M in @ $1 + 1M out @ $2
		t.Errorf("turn done = model %q cost %v", done.Model, done.Cost)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Role != "user" || msgs[1].Role != "assistant" {
		t.Fatalf("persisted %d messages: %+v", len(msgs), msgs)
	}
	if msgs[1].CallSite != router.SiteChat || msgs[1].TokensOut != 1_000_000 {
		t.Errorf("assistant row = %+v", msgs[1])
	}
}

func TestRunDispatchesToolsAndFeedsResultsBack(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{
			ToolCalls: []provider.Block{{
				Type: provider.BlockToolUse, ID: "c1", Name: "fs.read",
				Input: json.RawMessage(`{"path":"go.mod"}`),
			}},
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		provider.ScriptTurn{Text: "the module is spore", Usage: provider.Usage{InputTokens: 20, OutputTokens: 6}},
	)
	tools := &fakeTools{result: "module github.com/codered/spore"}
	a, st := harness(t, script, tools)

	sid, _ := st.CreateSession(ctx, "t", "")
	ch, err := a.Run(ctx, sid, "what module is this?")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events := collect(t, ch)

	var sawCall, sawResult bool
	for _, ev := range events {
		switch ev.Type {
		case EvToolCall:
			sawCall = ev.Block.Name == "fs.read"
		case EvToolResult:
			sawResult = strings.Contains(ev.Block.Content, "codered/spore")
		}
	}
	if !sawCall || !sawResult {
		t.Errorf("missing tool events: call=%v result=%v", sawCall, sawResult)
	}
	if len(tools.calls) != 1 {
		t.Fatalf("tool invoked %d times", len(tools.calls))
	}

	// Four rows: user, assistant(tool_use), tool result, assistant(text).
	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 4 {
		t.Fatalf("persisted %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != "tool" {
		t.Errorf("row 3 role = %q, want tool", msgs[2].Role)
	}

	// The second upstream request must carry the tool result and the tool specs.
	reqs := script.Requests()
	if len(reqs) != 2 {
		t.Fatalf("provider called %d times, want 2", len(reqs))
	}
	if len(reqs[0].Tools) != 1 || reqs[0].Tools[0].Name != "fs.read" {
		t.Errorf("tool specs not sent: %+v", reqs[0].Tools)
	}
	var foundResult bool
	for _, m := range reqs[1].Messages {
		for _, b := range m.Blocks {
			if b.Type == provider.BlockToolResult && b.ID == "c1" {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Errorf("second request lacks the tool result: %+v", reqs[1].Messages)
	}
}

func TestRunStopsAtMaxIterations(t *testing.T) {
	ctx := context.Background()
	turns := make([]provider.ScriptTurn, maxIterations+2)
	for i := range turns {
		turns[i] = provider.ScriptTurn{
			ToolCalls: []provider.Block{{Type: provider.BlockToolUse, ID: "c", Name: "fs.read", Input: json.RawMessage(`{}`)}},
		}
	}
	a, st := harness(t, provider.NewScript(turns...), &fakeTools{result: "x"})
	sid, _ := st.CreateSession(ctx, "t", "")

	ch, _ := a.Run(ctx, sid, "loop forever")
	var sawError bool
	for ev := range ch {
		if ev.Type == EvError {
			sawError = true
		}
	}
	if !sawError {
		t.Error("a runaway tool loop must end in an error event, not silence")
	}
}

type concurrentTools struct {
	mu    sync.Mutex
	calls []provider.Block
}

func (c *concurrentTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (c *concurrentTools) ReadOnly(string) bool { return true }
func (c *concurrentTools) Run(_ context.Context, call provider.Block) provider.Block {
	c.mu.Lock()
	c.calls = append(c.calls, call)
	c.mu.Unlock()
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "result for " + call.ID}
}

func TestRunDispatchesReadOnlyBatchConcurrently(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{
			ToolCalls: []provider.Block{
				{Type: provider.BlockToolUse, ID: "c1", Name: "fs.read", Input: json.RawMessage(`{"path":"a"}`)},
				{Type: provider.BlockToolUse, ID: "c2", Name: "fs.read", Input: json.RawMessage(`{"path":"b"}`)},
			},
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		provider.ScriptTurn{Text: "file contents", Usage: provider.Usage{InputTokens: 20, OutputTokens: 6}},
	)
	tools := &concurrentTools{}
	a, st := harness(t, script, tools)
	sid, _ := st.CreateSession(ctx, "t", "")

	ch, err := a.Run(ctx, sid, "read two files")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = collect(t, ch)

	// Both tool calls should have been run
	if len(tools.calls) != 2 {
		t.Fatalf("tool invoked %d times, want 2", len(tools.calls))
	}

	// Persisted messages: user, assistant(2 tool_uses), tool_result(2), assistant(text)
	msgs, _ := st.Messages(ctx, sid)
	if len(msgs) != 4 {
		t.Fatalf("persisted %d messages, want 4: %+v", len(msgs), msgs)
	}

	// Tool result message should contain both results
	if msgs[2].Role != "tool" {
		t.Errorf("row 3 role = %q, want tool", msgs[2].Role)
	}
	var blocks []provider.Block
	if err := json.Unmarshal(msgs[2].BlocksJSON, &blocks); err != nil {
		t.Fatalf("decode tool results: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("tool result message has %d blocks, want 2", len(blocks))
	}

	// Results must be ordered by call index (c1 then c2), not completion order
	if blocks[0].ID != "c1" || blocks[1].ID != "c2" {
		t.Errorf("tool results out of order: [0].ID=%q, [1].ID=%q (want c1, c2)", blocks[0].ID, blocks[1].ID)
	}
}

func TestMidStreamProviderErrorEndsLLMSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		tp.Shutdown(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
	})

	script := provider.NewScript(provider.ScriptTurn{Err: errors.New("upstream exploded")})
	a, st := harness(t, script, nil)
	sid, _ := st.CreateSession(context.Background(), "t", "")

	ch, err := a.Run(context.Background(), sid, "hello")
	if err != nil {
		t.Fatal(err)
	}
	var sawError bool
	for ev := range ch {
		if ev.Type == EvError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("expected an error event from the turn")
	}

	var llmSpans int
	for _, s := range sr.Ended() {
		if s.Name() == "llm" {
			llmSpans++
		}
	}
	if llmSpans != 1 {
		t.Errorf("expected exactly 1 ended llm span after a mid-stream provider error, got %d", llmSpans)
	}
}

// throttleTools records the high-water mark of concurrent Run calls.
type throttleTools struct {
	mu       sync.Mutex
	inFlight int
	peak     int
}

func (tt *throttleTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (tt *throttleTools) ReadOnly(string) bool { return true }
func (tt *throttleTools) Run(_ context.Context, call provider.Block) provider.Block {
	tt.mu.Lock()
	tt.inFlight++
	if tt.inFlight > tt.peak {
		tt.peak = tt.inFlight
	}
	tt.mu.Unlock()

	// Hold the slot long enough that every goroutine the dispatcher is
	// willing to run at once has overlapped with this one.
	time.Sleep(5 * time.Millisecond)

	tt.mu.Lock()
	tt.inFlight--
	tt.mu.Unlock()
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "ok"}
}

func TestReadOnlyBatchConcurrencyIsBounded(t *testing.T) {
	ctx := context.Background()

	// Three times the cap, so a batch that ignored the semaphore would show a
	// peak far above it rather than merely brushing against it.
	const calls = maxParallelTools * 3
	batch := make([]provider.Block, calls)
	for i := range batch {
		batch[i] = provider.Block{
			Type:  provider.BlockToolUse,
			ID:    fmt.Sprintf("c%02d", i),
			Name:  "fs.read",
			Input: json.RawMessage(`{"path":"a"}`),
		}
	}
	script := provider.NewScript(
		provider.ScriptTurn{ToolCalls: batch, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
		provider.ScriptTurn{Text: "done", Usage: provider.Usage{InputTokens: 20, OutputTokens: 6}},
	)
	tools := &throttleTools{}
	a, st := harness(t, script, tools)
	sid, _ := st.CreateSession(ctx, "t", "")

	ch, err := a.Run(ctx, sid, "read many files")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = collect(t, ch)

	tools.mu.Lock()
	peak, left := tools.peak, tools.inFlight
	tools.mu.Unlock()

	if peak > maxParallelTools {
		t.Errorf("%d tools ran at once, want at most %d: the fan-out is unbounded", peak, maxParallelTools)
	}
	if peak < 2 {
		t.Errorf("peak concurrency was %d: the batch serialised instead of running in parallel", peak)
	}
	if left != 0 {
		t.Errorf("%d tool calls still in flight after the turn: the dispatcher did not wait for them", left)
	}

	// Every call must still be answered, in order, however they were throttled.
	msgs, _ := st.Messages(ctx, sid)
	var blocks []provider.Block
	if err := json.Unmarshal(msgs[2].BlocksJSON, &blocks); err != nil {
		t.Fatalf("decode tool results: %v", err)
	}
	if len(blocks) != calls {
		t.Fatalf("got %d tool results, want %d", len(blocks), calls)
	}
	for i, b := range blocks {
		if want := fmt.Sprintf("c%02d", i); b.ID != want {
			t.Errorf("result %d has ID %q, want %q: throttling reordered the batch", i, b.ID, want)
		}
	}
}

// cancellableTools cancels the context mid-tool-dispatch to simulate daemon
// shutdown during a turn's tool phase.
type cancellableTools struct {
	cancel context.CancelFunc
}

func (c *cancellableTools) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}}
}
func (c *cancellableTools) ReadOnly(string) bool { return true }
func (c *cancellableTools) Run(ctx context.Context, call provider.Block) provider.Block {
	// Cancel the entire turn's context mid-dispatch to simulate daemon shutdown.
	c.cancel()
	// Let the cancellation propagate.
	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
	}
	// Return normally — persistCtx keeps the write alive even though ctx is cancelled.
	return provider.Block{Type: provider.BlockToolResult, ID: call.ID, Content: "result"}
}

func TestCancelledTurnDoesNotOrphanToolUse(t *testing.T) {
	ctx := context.Background()
	script := provider.NewScript(
		provider.ScriptTurn{
			ToolCalls: []provider.Block{{
				Type: provider.BlockToolUse, ID: "c1", Name: "fs.read",
				Input: json.RawMessage(`{"path":"go.mod"}`),
			}},
			Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		},
		provider.ScriptTurn{Text: "unreached", Usage: provider.Usage{InputTokens: 20, OutputTokens: 6}},
	)
	tools := &cancellableTools{}
	a, st := harness(t, script, tools)
	sid, _ := st.CreateSession(ctx, "t", "")

	// Create a cancellable context and pass it to the tool runner.
	runCtx, runCancel := context.WithCancel(ctx)
	tools.cancel = runCancel

	ch, err := a.Run(runCtx, sid, "read a file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Consume events until the channel closes or we hit a timeout.
	// The turn should fail due to cancellation.
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("turn did not finish within timeout")
	case <-ch:
		// Turn completed or errored, which is expected.
	}

	// Read the persisted history. The tool_use message may have been written
	// (due to persistCtx), but if the tool_result message was not written,
	// Snapshot must drop the trailing tool_use.
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	// The last message should not be an assistant message containing tool_use blocks.
	if len(snap.Messages) > 0 {
		lastMsg := snap.Messages[len(snap.Messages)-1]
		if lastMsg.Role == provider.RoleAssistant {
			for _, block := range lastMsg.Blocks {
				if block.Type == provider.BlockToolUse {
					t.Fatal("Snapshot returned a trailing tool_use block without matching tool_result")
				}
			}
		}
	}
}

func TestSnapshotIncludesFactsFromTheCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := memory.Write(dir, memory.Fact{Name: "a-fact", Description: "d", Type: "user", Body: "remembered"}); err != nil {
		t.Fatal(err)
	}
	cache := memory.NewCache(dir)
	cache.Reload()

	// Build an agent the way the surrounding tests in this file do, then
	// attach the cache.
	a := newTestAgent(t)
	a.Facts = cache

	sid, err := a.Store.CreateSession(ctx, "t", "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Facts) != 1 || snap.Facts[0].Body != "remembered" {
		t.Fatalf("facts not loaded into the snapshot: %+v", snap.Facts)
	}
}

func TestSnapshotWithNoFactCacheIsEmpty(t *testing.T) {
	ctx := context.Background()
	a := newTestAgent(t)
	sid, _ := a.Store.CreateSession(ctx, "t", "")
	snap, err := a.Snapshot(ctx, sid)
	if err != nil {
		t.Fatalf("a nil fact cache must not be an error: %v", err)
	}
	if len(snap.Facts) != 0 {
		t.Fatal("facts appeared with no cache attached")
	}
}

func TestSnapshotDescribesTheSessionsWorkspace(t *testing.T) {
	a, st := harness(t, provider.NewScript(), nil)
	a.Env = func(root string) string { return "root=" + root }
	ctx := policy.WithSession(context.Background(),
		policy.Session{ID: "s1", Profile: policy.ProfileLocal, Workspace: "/ws/a"})
	id, err := st.CreateSession(ctx, "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Environment != "root=/ws/a" {
		t.Fatalf("environment = %q, want root=/ws/a", snap.Environment)
	}
}

// No session on the context means no environment section, rather than a
// description of a directory this turn is not working in.
func TestSnapshotHasNoEnvironmentWithoutAWorkspace(t *testing.T) {
	a, st := harness(t, provider.NewScript(), nil)
	a.Env = func(root string) string { return "root=" + root }
	id, err := st.CreateSession(context.Background(), "", "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Environment != "root=" {
		t.Fatalf("environment = %q, want the describer called with an empty root", snap.Environment)
	}
}
