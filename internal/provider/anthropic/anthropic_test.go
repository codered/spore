package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
		if got := r.Header.Get("anthropic-workspace-id"); got != "wrkspc_test" {
			t.Errorf("anthropic-workspace-id = %q", got)
		}
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "wrkspc_test", true, srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{
		Model:     "claude-opus-5",
		System:    []provider.Block{{Type: provider.BlockText, Text: "you are spore"}},
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
	systemBlocks, ok := gotBody["system"].([]any)
	if !ok || len(systemBlocks) != 1 {
		t.Errorf("system blocks = %+v, want array with 1 block", gotBody["system"])
	}
	if blockText, ok := systemBlocks[0].(map[string]any)["text"].(string); !ok || blockText != "you are spore" {
		t.Errorf("system block text = %+v, want 'you are spore'", systemBlocks[0])
	}
	if gotBody["stream"] != true {
		t.Errorf("stream = %+v, want true", gotBody["stream"])
	}
}

func TestStreamSurfacesHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"bad model"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "wrkspc_test", true, srv.Client())
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

	c := New(srv.URL, "sk-test", "wrkspc_test", true, srv.Client())
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

	c := New(srv.URL, "sk-test", "wrkspc_test", true, srv.Client())
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

	c := New(srv.URL, "sk-test", "wrkspc_test", true, srv.Client())
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

// With no workspace configured the header is omitted, which makes the API act
// in the key's default workspace; the id it reports back is reused after that.
func TestStreamOmitsWorkspaceHeaderThenAdoptsTheDefault(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_use.sse")
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("anthropic-workspace-id"))
		w.Header().Set("anthropic-workspace-id", "wrkspc_default")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "", true, srv.Client())
	for i := 0; i < 2; i++ {
		ch, err := c.Stream(context.Background(), provider.Request{Model: "claude-opus-5", MaxTokens: 16})
		if err != nil {
			t.Fatalf("Stream %d: %v", i, err)
		}
		for range ch {
		}
	}

	want := []string{"", "wrkspc_default"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("workspace headers = %q, want %q", seen, want)
	}
}

// An identity-linked key spanning several workspaces rejects a header-less
// request but names the default workspace; the client retries with it.
func TestStreamRetriesWithDefaultWorkspaceOn400(t *testing.T) {
	fixture, err := os.ReadFile("testdata/tool_use.sse")
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws := r.Header.Get("anthropic-workspace-id")
		seen = append(seen, ws)
		w.Header().Set("anthropic-workspace-id", "wrkspc_default")
		if ws == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"anthropic-workspace-id is required when authenticating with an identity-linked API key"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(fixture)
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "", true, srv.Client())
	ch, err := c.Stream(context.Background(), provider.Request{Model: "claude-opus-5", MaxTokens: 16})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	want := []string{"", "wrkspc_default"}
	if len(seen) != 2 || seen[0] != want[0] || seen[1] != want[1] {
		t.Errorf("workspace headers = %q, want %q", seen, want)
	}
}

// Without a workspace to fall back on the error tells the user what to set.
func TestStreamWorkspaceErrorIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"anthropic-workspace-id is required"}}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "sk-test", "", true, srv.Client())
	_, err := c.Stream(context.Background(), provider.Request{Model: "claude-opus-5", MaxTokens: 16})
	if err == nil {
		t.Fatal("Stream succeeded on a 400; want error")
	}
	if !strings.Contains(err.Error(), "workspace_id") {
		t.Errorf("error = %q, want it to name workspace_id", err)
	}
}

// bodyOf runs one Stream against a recording server and returns the decoded
// request body. The wire format is the contract with the API, so it is
// asserted on the bytes rather than on an intermediate struct.
func bodyOf(t *testing.T, cache bool, req provider.Request) map[string]any {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "k", "", cache, nil)
	ch, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}
	return got
}

func cachedRequest() provider.Request {
	return provider.Request{
		Model:     "claude-opus-5",
		MaxTokens: 100,
		System: []provider.Block{
			{Type: provider.BlockText, Text: "stable"},
			{Type: provider.BlockText, Text: "summary", CacheBreak: true},
		},
		Messages: []provider.Message{{Role: provider.RoleUser, Blocks: []provider.Block{
			{Type: provider.BlockText, Text: "hello", CacheBreak: true},
			{Type: provider.BlockText, Text: "env"},
		}}},
	}
}

func TestSystemIsBlocksWithCacheControlOnTheMarkedOne(t *testing.T) {
	body := bodyOf(t, true, cachedRequest())

	blocks, ok := body["system"].([]any)
	if !ok {
		t.Fatalf("system = %T, want an array of blocks", body["system"])
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d system blocks, want 2", len(blocks))
	}
	if _, marked := blocks[0].(map[string]any)["cache_control"]; marked {
		t.Error("the first system block carries cache_control; only the last should")
	}
	cc, marked := blocks[1].(map[string]any)["cache_control"].(map[string]any)
	if !marked {
		t.Fatal("the last system block carries no cache_control")
	}
	if cc["type"] != "ephemeral" {
		t.Errorf("cache_control type = %v, want ephemeral", cc["type"])
	}
}

func TestMessageBreakpointRendersAndTheEnvironmentDoesNot(t *testing.T) {
	body := bodyOf(t, true, cachedRequest())

	msgs := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if _, marked := content[0].(map[string]any)["cache_control"]; !marked {
		t.Error("the marked message block carries no cache_control")
	}
	if _, marked := content[1].(map[string]any)["cache_control"]; marked {
		t.Error("the environment block carries cache_control; it changes every turn")
	}
}

// The escape hatch exists for a proxy that rejects the field.
func TestCacheDisabledEmitsNoCacheControl(t *testing.T) {
	body := bodyOf(t, false, cachedRequest())

	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("cache_control")) {
		t.Fatalf("cache_control was sent with caching off: %s", raw)
	}
	if _, ok := body["system"].([]any); !ok {
		t.Errorf("system = %T, want an array of blocks even with caching off", body["system"])
	}
}

// TestToWireOmitsZeroValuedOptionalFields verifies that the wire format
// preserves omitempty semantics: zero-valued optional fields are absent.
// This is critical because including them changes message size and token counts.
func TestToWireOmitsZeroValuedOptionalFields(t *testing.T) {
	body := bodyOf(t, true, provider.Request{
		Model:     "claude-opus-5",
		MaxTokens: 100,
		Messages: []provider.Message{
			{
				Role: provider.RoleAssistant,
				Blocks: []provider.Block{
					{Type: provider.BlockToolUse, ID: "tool_1", Name: "test_tool", Input: nil},
					{Type: provider.BlockToolUse, ID: "", Name: "", Input: nil},
				},
			},
			{
				Role: provider.RoleTool,
				Blocks: []provider.Block{
					{Type: provider.BlockToolResult, ID: "tool_1", Content: "success"},
					{Type: provider.BlockToolResult, ID: "tool_2", Content: "error", IsError: true},
					{Type: provider.BlockToolResult, ID: "", Content: ""},
				},
			},
		},
	})

	msgs := body["messages"].([]any)

	// Check that tool_use with nil input omits the input field entirely.
	toolUseMsg := msgs[0].(map[string]any)
	toolUseContent := toolUseMsg["content"].([]any)
	toolUseBlock := toolUseContent[0].(map[string]any)
	if _, hasInput := toolUseBlock["input"]; hasInput {
		t.Errorf("tool_use with nil input should omit the input field, but it's present in: %+v", toolUseBlock)
	}

	// Check that tool_use with empty ID and Name omits both fields.
	emptyToolUseBlock := toolUseContent[1].(map[string]any)
	if _, hasID := emptyToolUseBlock["id"]; hasID {
		t.Errorf("tool_use with empty ID should omit the id field, but it's present in: %+v", emptyToolUseBlock)
	}
	if _, hasName := emptyToolUseBlock["name"]; hasName {
		t.Errorf("tool_use with empty Name should omit the name field, but it's present in: %+v", emptyToolUseBlock)
	}

	// Check that successful tool_result omits is_error field.
	toolResultMsg := msgs[1].(map[string]any)
	toolResultContent := toolResultMsg["content"].([]any)
	successResult := toolResultContent[0].(map[string]any)
	if _, hasIsError := successResult["is_error"]; hasIsError {
		t.Errorf("successful tool_result should omit is_error field, but it's present in: %+v", successResult)
	}

	// Check that error tool_result includes is_error=true.
	errorResult := toolResultContent[1].(map[string]any)
	if isError, ok := errorResult["is_error"].(bool); !ok || !isError {
		t.Errorf("error tool_result should have is_error=true, got: %+v", errorResult)
	}

	// Check that tool_result with empty ID omits the tool_use_id field.
	emptyToolResultBlock := toolResultContent[2].(map[string]any)
	if _, hasToolUseID := emptyToolResultBlock["tool_use_id"]; hasToolUseID {
		t.Errorf("tool_result with empty ID should omit the tool_use_id field, but it's present in: %+v", emptyToolResultBlock)
	}
}

// TestInputPrecisionPreserved verifies that Input (json.RawMessage) containing
// large integers and trailing-zero decimals are preserved exactly as-is on the wire.
// This is critical because Input carries tool arguments that spore echoes back
// to the API in multi-turn history. We assert on raw bytes, not decoded maps,
// because decoding to float64 is the very step that destroys the evidence.
func TestInputPrecisionPreserved(t *testing.T) {
	// Input with large integer (exceeds float64 safe integer limit) and trailing-zero decimal
	inputJSON := `{"count":9007199254740993,"amount":1.50}`
	req := provider.Request{
		Model:     "claude-opus-5",
		MaxTokens: 100,
		Messages: []provider.Message{{
			Role: provider.RoleAssistant,
			Blocks: []provider.Block{{
				Type:  provider.BlockToolUse,
				ID:    "tool_1",
				Name:  "test",
				Input: json.RawMessage(inputJSON),
			}},
		}},
	}

	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		rawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "k", "", true, nil)
	ch, err := c.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for range ch {
	}

	// Verify the exact input literal appears in the wire bytes before any decoding
	if !bytes.Contains(rawBody, []byte(`"count":9007199254740993`)) {
		t.Errorf("large integer lost precision: wire body=%s, want literal %q present", string(rawBody), `"count":9007199254740993`)
	}
	if !bytes.Contains(rawBody, []byte(`"amount":1.50`)) {
		t.Errorf("trailing-zero decimal formatting lost: wire body=%s, want literal %q present", string(rawBody), `"amount":1.50`)
	}
}
