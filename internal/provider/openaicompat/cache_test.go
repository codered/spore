package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"testing"

	"github.com/codered/spore/internal/provider"
)

var marker = map[string]any{"type": "ephemeral"}

func part(text string, marked bool) map[string]any {
	p := map[string]any{"type": "text", "text": text}
	if marked {
		p["cache_control"] = marker
	}
	return p
}

// With caching off, a request carrying breakpoints goes out exactly as it did
// before cache_control existed: every content is a plain string.
func TestToWireIgnoresBreakpointsWhenCachingIsOff(t *testing.T) {
	out := toWire(false,
		[]provider.Block{{Type: provider.BlockText, Text: "sys", CacheBreak: true}},
		[]provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{
			{Type: provider.BlockText, Text: "hi", CacheBreak: true},
			{Type: provider.BlockText, Text: "env"},
		}}})
	want := []map[string]any{
		{"role": "system", "content": "sys"},
		{"role": "user", "content": "hienv"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// The system block's breakpoint becomes a marked text part. Through LiteLLM
// to Anthropic, tools precede system, so this one marker caches them too.
func TestToWireMarksTheSystemPrompt(t *testing.T) {
	out := toWire(true, []provider.Block{
		{Type: provider.BlockText, Text: "alpha"},
		{Type: provider.BlockText, Text: "beta", CacheBreak: true},
	}, nil)
	want := []map[string]any{{"role": "system", "content": []map[string]any{part("alphabeta", true)}}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// The per-turn environment must sit after the marker, in its own part. Marked
// together with the user's text, every turn would write a cache entry holding
// that turn's environment, which no later turn can read.
func TestToWireKeepsTheEnvironmentOutsideTheMarkedUserPart(t *testing.T) {
	out := toWire(true, nil, []provider.Message{
		{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "earlier"}}},
		{Role: provider.RoleAssistant, Blocks: []provider.Block{{Type: provider.BlockText, Text: "reply"}}},
		{Role: provider.RoleUser, Blocks: []provider.Block{
			{Type: provider.BlockText, Text: "now", CacheBreak: true},
			{Type: provider.BlockText, Text: "## Environment"},
		}},
	})
	want := []map[string]any{
		{"role": "user", "content": "earlier"},
		{"role": "assistant", "content": "reply"},
		{"role": "user", "content": []map[string]any{part("now", true), part("## Environment", false)}},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// Mid-turn the tail is the tool-results message. Anthropic rejects a marker
// inside tool_result.content, which is where LiteLLM puts a tool message's
// parts, so the marker goes on the tool message itself; LiteLLM moves it onto
// the tool_result. The environment cannot share that message without being
// cached with it, so it follows as a user message, which Anthropic merges
// into the same turn as the results: the shape the native adapter sends.
// Both were probed against the LAN proxy.
func TestToWireMarksTheLastToolResultAndSendsTheEnvironmentAfterIt(t *testing.T) {
	out := toWire(true, nil, []provider.Message{
		{Role: provider.RoleAssistant, Blocks: []provider.Block{
			{Type: provider.BlockToolUse, ID: "call_1", Name: "go_run", Input: json.RawMessage(`{}`)},
			{Type: provider.BlockToolUse, ID: "call_2", Name: "fs_read", Input: json.RawMessage(`{}`)},
		}},
		{Role: provider.RoleTool, Blocks: []provider.Block{
			{Type: provider.BlockToolResult, ID: "call_1", Content: "91"},
			{Type: provider.BlockToolResult, ID: "call_2", Content: "boom", IsError: true, CacheBreak: true},
			{Type: provider.BlockText, Text: "## Environment"},
		}},
	})
	want := []map[string]any{
		out[0], // the assistant message, covered elsewhere
		{"role": "tool", "tool_call_id": "call_1", "content": "91"},
		{"role": "tool", "tool_call_id": "call_2", "content": "Error: boom", "cache_control": marker},
		{"role": "user", "content": "## Environment"},
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// Caching on for the model, but this round's results carry no breakpoint (an
// earlier round in the history): the environment still joins the last result.
func TestToWireFoldsTheEnvironmentIntoAnUnmarkedResult(t *testing.T) {
	out := toWire(true, nil, []provider.Message{
		{Role: provider.RoleTool, Blocks: []provider.Block{
			{Type: provider.BlockToolResult, ID: "call_1", Content: "91"},
			{Type: provider.BlockText, Text: "## Environment"},
		}},
	})
	want := []map[string]any{{"role": "tool", "tool_call_id": "call_1", "content": "91## Environment"}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// Caching on, but no breakpoint in a message: it keeps the string shape.
func TestToWireLeavesUnmarkedMessagesAsStrings(t *testing.T) {
	out := toWire(true, []provider.Block{{Type: provider.BlockText, Text: "sys"}},
		[]provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{{Type: provider.BlockText, Text: "hi"}}}})
	want := []map[string]any{{"role": "system", "content": "sys"}, {"role": "user", "content": "hi"}}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out = %v\nwant %v", out, want)
	}
}

// Markers reach only the models the regex names, so one LiteLLM provider can
// serve Claude with them and everything else without.
func TestStreamSendsMarkersOnlyToMatchingModels(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client()).WithCacheControl(regexp.MustCompile("claude"))

	for _, tc := range []struct {
		model  string
		marked bool
	}{{"anthropic-claude-opus-5", true}, {"openai-gpt-5.6-luna", false}} {
		ch, err := c.Stream(context.Background(), provider.Request{
			Model:  tc.model,
			System: []provider.Block{{Type: provider.BlockText, Text: "sys", CacheBreak: true}},
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		for range ch {
		}
		sys := got["messages"].([]any)[0].(map[string]any)
		_, isParts := sys["content"].([]any)
		if isParts != tc.marked {
			t.Errorf("%s: system content = %v, want marked %v", tc.model, sys["content"], tc.marked)
		}
	}
}

// LiteLLM reports prompt_tokens as the whole prompt, with reads and writes
// inside it (probed against the LAN proxy). Usage.InputTokens is the uncached
// remainder, so both come off.
func TestStreamSplitsCachedTokensOutOfInput(t *testing.T) {
	for _, tc := range []struct {
		name, usage string
		want        provider.Usage
	}{
		{"gpt via litellm", `{"prompt_tokens":12021,"completion_tokens":13,"prompt_tokens_details":{"cached_tokens":12003,"cache_write_tokens":15,"cache_creation_tokens":15},"cache_read_input_tokens":12003}`,
			provider.Usage{InputTokens: 3, OutputTokens: 13, CacheReadTokens: 12003, CacheWriteTokens: 15}},
		{"claude via litellm", `{"prompt_tokens":18066,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":18003},"cache_read_input_tokens":18003}`,
			provider.Usage{InputTokens: 63, OutputTokens: 5, CacheReadTokens: 18003}},
		{"anthropic-style top level only", `{"prompt_tokens":100,"completion_tokens":1,"cache_read_input_tokens":60,"cache_creation_input_tokens":30}`,
			provider.Usage{InputTokens: 10, OutputTokens: 1, CacheReadTokens: 60, CacheWriteTokens: 30}},
		{"no cache fields", `{"prompt_tokens":42,"completion_tokens":7}`,
			provider.Usage{InputTokens: 42, OutputTokens: 7}},
	} {
		body := "data: {\"choices\":[],\"usage\":" + tc.usage + "}\n\ndata: [DONE]\n\n"
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(body))
		}))
		ch, err := New(srv.URL, "", srv.Client()).Stream(context.Background(), provider.Request{Model: "m"})
		if err != nil {
			srv.Close()
			t.Fatalf("%s: Stream: %v", tc.name, err)
		}
		var got provider.Usage
		for ev := range ch {
			if ev.Type == provider.EventDone {
				got = *ev.Usage
			}
		}
		srv.Close()
		if got != tc.want {
			t.Errorf("%s: usage = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
