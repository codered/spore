package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
