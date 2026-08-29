package anthropic

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

func TestStreamParsesTextToolCallAndUsage(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_use.sse")
	if err != nil {
		t.Fatal(err)
	}
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "sk-test" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got == "" {
			t.Error("anthropic-version header missing")
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "claude-opus-5",
		System:    "you are spore",
		MaxTokens: 1024,
		Messages: []provider.Message{{
			Role:   provider.RoleUser,
			Blocks: []provider.Block{{Type: provider.BlockText, Text: "what module is this?"}},
		}},
		Tools: []provider.ToolSpec{{Name: "fs.read", Description: "read a file", Schema: json.RawMessage(`{"type":"object"}`)}},
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

	if text != "Checking the file." {
		t.Errorf("text = %q", text)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Name != "fs.read" || string(calls[0].Input) != `{"path":"go.mod"}` {
		t.Errorf("tool call = %+v (input %s)", calls[0], calls[0].Input)
	}
	if usage.InputTokens != 112 || usage.OutputTokens != 37 {
		t.Errorf("usage = %+v", usage)
	}
	if gotBody["system"] != "you are spore" || gotBody["stream"] != true {
		t.Errorf("request body = %+v", gotBody)
	}
}

func TestStreamSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	_, err := c.Stream(context.Background(), provider.Request{Model: "nope", MaxTokens: 16})
	if err == nil {
		t.Fatal("Stream succeeded on a 400; want error")
	}
}

func TestStreamTruncatedWithoutMessageStopIsAnError(t *testing.T) {
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
	ch, err := c.Stream(context.Background(), provider.Request{Model: "claude-opus-5", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var gotError error
	for ev := range ch {
		if ev.Type == provider.EventError {
			gotError = ev.Err
		}
	}

	if gotError == nil {
		t.Fatal("expected EventError for truncated stream, got none")
	}
	if gotError.Error() != "stream ended without message_stop (truncated response)" {
		t.Errorf("error message = %q", gotError.Error())
	}
}

func TestStreamSurfacesUpstreamErrorEvent(t *testing.T) {
	fixture, err := os.ReadFile("testdata/error_event.sse")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{Model: "claude-opus-5", MaxTokens: 1024})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var gotError error
	for ev := range ch {
		if ev.Type == provider.EventError {
			gotError = ev.Err
		}
	}

	if gotError == nil {
		t.Fatal("expected EventError for upstream error, got none")
	}
	errMsg := gotError.Error()
	if !strings.Contains(errMsg, "overloaded_error") {
		t.Errorf("error message missing overloaded_error: %q", errMsg)
	}
	if !strings.Contains(errMsg, "server is overloaded") {
		t.Errorf("error message missing server is overloaded: %q", errMsg)
	}
}

func TestToWireSendsToolResultAsUserRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gotBody map[string]any
		json.NewDecoder(r.Body).Decode(&gotBody)
		msgs, ok := gotBody["messages"].([]any)
		if !ok {
			t.Fatal("messages not a list")
		}
		if len(msgs) < 2 {
			t.Fatalf("expected at least 2 messages, got %d", len(msgs))
		}
		secondMsg, ok := msgs[1].(map[string]any)
		if !ok {
			t.Fatal("second message not a map")
		}
		if role, ok := secondMsg["role"].(string); !ok || role != "user" {
			t.Errorf("second message role = %v, want user", secondMsg["role"])
		}
		content, ok := secondMsg["content"].([]any)
		if !ok {
			t.Fatal("second message content not a list")
		}
		if len(content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(content))
		}
		block, ok := content[0].(map[string]any)
		if !ok {
			t.Fatal("content block not a map")
		}
		if blockType, ok := block["type"].(string); !ok || blockType != "tool_result" {
			t.Errorf("block type = %v, want tool_result", block["type"])
		}
		if id, ok := block["tool_use_id"].(string); !ok || id != "tool_1" {
			t.Errorf("block tool_use_id = %v, want tool_1", block["tool_use_id"])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "claude-opus-5",
		MaxTokens: 1024,
		Messages: []provider.Message{
			{
				Role:   provider.RoleAssistant,
				Blocks: []provider.Block{{Type: provider.BlockToolUse, ID: "tool_1", Name: "test", Input: json.RawMessage(`{}`)}},
			},
			{
				Role:   provider.RoleTool,
				Blocks: []provider.Block{{Type: provider.BlockToolResult, ID: "tool_1", Content: "result"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	for ev := range ch {
		if ev.Type == provider.EventError {
			t.Fatalf("unexpected error: %v", ev.Err)
		}
	}
}
