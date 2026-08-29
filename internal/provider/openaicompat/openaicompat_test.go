package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/codered/spore/internal/provider"
)

func TestStreamParsesFragmentedToolCallAndUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "qwen3:8b",
		System:    "you are spore",
		MaxTokens: 512,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "what module is this?"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var text string
	var calls []provider.Block
	var usage provider.Usage
	for ev := range ch {
		switch ev.Type {
		case provider.EventTextDelta:
			text += ev.Text
		case provider.EventToolCall:
			calls = append(calls, *ev.Block)
		case provider.EventDone:
			usage = *ev.Usage
		case provider.EventError:
			t.Fatalf("error event: %v", ev.Err)
		}
	}

	if text != "Looking it up." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || string(calls[0].Input) != `{"path":"go.mod"}` {
		t.Fatalf("calls = %+v", calls)
	}
	if usage.InputTokens != 88 || usage.OutputTokens != 25 {
		t.Errorf("usage = %+v", usage)
	}
	// The system prompt must travel as the first message, not a top-level field.
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("no messages in request body")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "you are spore" {
		t.Errorf("first message = %+v", first)
	}
}

func TestStreamSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "", srv.Client()).Stream(context.Background(), provider.Request{Model: "m"}); err == nil {
		t.Fatal("Stream succeeded on a 401; want error")
	}
}

func TestStreamTruncatedWithoutDoneIsAnError(t *testing.T) {
	fixture, err := os.ReadFile("testdata/truncated.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model: "test-model",
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "test"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var lastEvent provider.Event
	for ev := range ch {
		lastEvent = ev
	}

	if lastEvent.Type != provider.EventError {
		t.Errorf("final event type = %v, want EventError", lastEvent.Type)
	}
	if !strings.Contains(lastEvent.Err.Error(), "truncated") && !strings.Contains(lastEvent.Err.Error(), "[DONE]") {
		t.Errorf("error message = %v, want mention of truncation or [DONE]", lastEvent.Err)
	}
}

func TestStreamSurfacesUpstreamErrorChunk(t *testing.T) {
	fixture, err := os.ReadFile("testdata/error_chunk.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model: "test-model",
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "test"}},
		}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var errorEvent *provider.Event
	for ev := range ch {
		if ev.Type == provider.EventError {
			errorEvent = &ev
			break
		}
	}

	if errorEvent == nil {
		t.Fatal("no error event received")
	}
	errMsg := errorEvent.Err.Error()
	if !strings.Contains(errMsg, "rate_limit_error") {
		t.Errorf("error message missing type, got: %v", errMsg)
	}
	if !strings.Contains(errMsg, "Rate limit exceeded") {
		t.Errorf("error message missing message text, got: %v", errMsg)
	}
}

func TestToWireSendsToolResultAsToolMessage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model: "test-model",
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.Block{
					{
						Type:  provider.BlockToolUse,
						ID:    "test_call_id",
						Name:  "test_function",
						Input: json.RawMessage(`{"arg":"value"}`),
					},
				},
			},
			{
				Role: provider.RoleUser,
				Blocks: []provider.Block{
					{
						Type:    provider.BlockToolResult,
						ID:      "test_call_id",
						Content: "tool result output",
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Drain the channel
	for range ch {
	}

	msgs, ok := capturedBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatal("no messages in request body")
	}

	// Find the assistant message with tool_calls
	var assistantMsg map[string]any
	var toolMsg map[string]any
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, ok := msg["role"].(string); ok {
			if role == "assistant" && msg["tool_calls"] != nil {
				assistantMsg = msg
			}
			if role == "tool" {
				toolMsg = msg
			}
		}
	}

	if assistantMsg == nil {
		t.Fatal("no assistant message with tool_calls found")
	}
	if toolMsg == nil {
		t.Fatal("no tool message found")
	}

	// Verify assistant message has tool_calls with correct id
	toolCalls, ok := assistantMsg["tool_calls"].([]any)
	if !ok || len(toolCalls) == 0 {
		t.Fatal("assistant message has no tool_calls")
	}
	firstCall, ok := toolCalls[0].(map[string]any)
	if !ok || firstCall["id"] != "test_call_id" {
		t.Errorf("assistant tool_call id = %v, want test_call_id", firstCall["id"])
	}

	// Verify tool message has correct tool_call_id and content
	if toolMsg["tool_call_id"] != "test_call_id" {
		t.Errorf("tool message tool_call_id = %v, want test_call_id", toolMsg["tool_call_id"])
	}
	if toolMsg["content"] != "tool result output" {
		t.Errorf("tool message content = %v, want 'tool result output'", toolMsg["content"])
	}
}

func TestToWireMarksFailedToolResults(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	var capturedBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model: "test-model",
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.Block{
					{
						Type:  provider.BlockToolUse,
						ID:    "error_call_id",
						Name:  "test_function",
						Input: json.RawMessage(`{"arg":"value"}`),
					},
				},
			},
			{
				Role: provider.RoleUser,
				Blocks: []provider.Block{
					{
						Type:    provider.BlockToolResult,
						ID:      "error_call_id",
						Content: "tool execution failed",
						IsError: true,
					},
				},
			},
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.Block{
					{
						Type:  provider.BlockToolUse,
						ID:    "success_call_id",
						Name:  "test_function",
						Input: json.RawMessage(`{"arg":"value"}`),
					},
				},
			},
			{
				Role: provider.RoleUser,
				Blocks: []provider.Block{
					{
						Type:    provider.BlockToolResult,
						ID:      "success_call_id",
						Content: "success output",
						IsError: false,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Drain the channel
	for range ch {
	}

	msgs, ok := capturedBody["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatal("no messages in request body")
	}

	// Find tool messages
	var errorToolMsg map[string]any
	var successToolMsg map[string]any
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok || role != "tool" {
			continue
		}
		toolCallID, _ := msg["tool_call_id"].(string)
		if toolCallID == "error_call_id" {
			errorToolMsg = msg
		}
		if toolCallID == "success_call_id" {
			successToolMsg = msg
		}
	}

	if errorToolMsg == nil {
		t.Fatal("no error tool message found")
	}
	if successToolMsg == nil {
		t.Fatal("no success tool message found")
	}

	// Verify error tool result is prefixed with "Error: "
	errorContent, _ := errorToolMsg["content"].(string)
	if !strings.HasPrefix(errorContent, "Error: ") {
		t.Errorf("error tool message content = %q, want prefix 'Error: '", errorContent)
	}
	if errorContent != "Error: tool execution failed" {
		t.Errorf("error tool message content = %q, want 'Error: tool execution failed'", errorContent)
	}

	// Verify successful tool result is NOT prefixed
	successContent, _ := successToolMsg["content"].(string)
	if strings.HasPrefix(successContent, "Error: ") {
		t.Errorf("success tool message content should not have Error prefix, got %q", successContent)
	}
	if successContent != "success output" {
		t.Errorf("success tool message content = %q, want 'success output'", successContent)
	}
}
