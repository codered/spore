// Package openaicompat adapts any OpenAI-compatible chat-completions endpoint
// (OpenAI, DeepSeek, Groq, OpenRouter, vLLM, Ollama) to provider.Provider.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
)

type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, hc: hc}
}

func (c *Client) Name() string { return "openaicompat" }

// toWire flattens spore messages into OpenAI's shape: assistant tool calls
// become `tool_calls`, and each tool result becomes its own `tool` message.
func toWire(system string, msgs []provider.Message) []map[string]any {
	out := []map[string]any{}
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	for _, m := range msgs {
		var text strings.Builder
		var calls []map[string]any
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				text.WriteString(b.Text)
			case provider.BlockToolUse:
				calls = append(calls, map[string]any{
					"id": b.ID, "type": "function",
					"function": map[string]any{"name": b.Name, "arguments": string(b.Input)},
				})
			case provider.BlockToolResult:
				content := b.Content
				if b.IsError {
					content = "Error: " + content
				}
				out = append(out, map[string]any{
					"role": "tool", "tool_call_id": b.ID, "content": content,
				})
			}
		}
		if text.Len() == 0 && len(calls) == 0 {
			continue
		}
		msg := map[string]any{"role": string(m.Role), "content": text.String()}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		out = append(out, msg)
	}
	return out
}

func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	body := map[string]any{
		"model":          req.Model,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
		"messages":       toWire(req.System, req.Messages),
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.Schema,
				},
			})
		}
		body["tools"] = tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai-compatible request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai-compatible %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Event, 32)
	go parse(resp.Body, ch)
	return ch, nil
}

func parse(rc io.ReadCloser, ch chan<- provider.Event) {
	defer close(ch)
	defer rc.Close()

	type pending struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*pending{}
	var usage provider.Usage
	var sawDone bool

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("decode chunk: %w", err)}
			return
		}
		if chunk.Error != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("openai-compatible stream error: %s: %s", chunk.Error.Type, chunk.Error.Message)}
			return
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				ch <- provider.Event{Type: provider.EventTextDelta, Text: choice.Delta.Content}
			}
			for _, tc := range choice.Delta.ToolCalls {
				p := calls[tc.Index]
				if p == nil {
					p = &pending{}
					calls[tc.Index] = p
				}
				if tc.ID != "" {
					p.id = tc.ID
				}
				if tc.Function.Name != "" {
					p.name = tc.Function.Name
				}
				p.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := sc.Err(); err != nil {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}

	if !sawDone {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("stream ended without [DONE]: truncated response")}
		return
	}

	// Emit accumulated calls in index order so replays are deterministic.
	idx := make([]int, 0, len(calls))
	for i := range calls {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		p := calls[i]
		args := p.args.String()
		if args == "" {
			args = "{}"
		}
		ch <- provider.Event{Type: provider.EventToolCall, Block: &provider.Block{
			Type: provider.BlockToolUse, ID: p.id, Name: p.name, Input: json.RawMessage(args),
		}}
	}
	u := usage
	ch <- provider.Event{Type: provider.EventDone, Usage: &u}
}
