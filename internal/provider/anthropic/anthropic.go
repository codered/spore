// Package anthropic adapts the Anthropic Messages API to provider.Provider.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
)

const apiVersion = "2023-06-01"

type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, hc: hc}
}

func (c *Client) Name() string { return "anthropic" }

type wireBlock struct {
	Type    string          `json:"type"`
	Text    string          `json:"text,omitempty"`
	ID      string          `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Content string          `json:"content,omitempty"`
	IsError bool            `json:"is_error,omitempty"`

	// tool_result references the originating tool_use id.
	ToolUseID string `json:"tool_use_id,omitempty"`
}

func toWire(msgs []provider.Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := string(m.Role)
		blocks := make([]wireBlock, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockToolResult:
				// Anthropic carries tool results on a user-role message.
				role = "user"
				blocks = append(blocks, wireBlock{Type: "tool_result", ToolUseID: b.ID, Content: b.Content, IsError: b.IsError})
			case provider.BlockToolUse:
				blocks = append(blocks, wireBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: b.Input})
			default:
				blocks = append(blocks, wireBlock{Type: "text", Text: b.Text})
			}
		}
		out = append(out, map[string]any{"role": role, "content": blocks})
	}
	return out
}

func (c *Client) Stream(ctx context.Context, req provider.Request) (<-chan provider.Event, error) {
	body := map[string]any{
		"model":      req.Model,
		"max_tokens": req.MaxTokens,
		"stream":     true,
		"messages":   toWire(req.Messages),
	}
	if req.System != "" {
		body["system"] = req.System
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"name": t.Name, "description": t.Description, "input_schema": t.Schema,
			})
		}
		body["tools"] = tools
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("anthropic %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Event, 32)
	go c.parse(resp.Body, ch)
	return ch, nil
}

// parse turns the SSE stream into events. Tool-call JSON arrives in
// fragments, so partial input is accumulated per content-block index and
// emitted only at content_block_stop.
func (c *Client) parse(rc io.ReadCloser, ch chan<- provider.Event) {
	defer close(ch)
	defer rc.Close()

	type pending struct {
		id, name string
		input    strings.Builder
	}
	tools := map[int]*pending{}
	var usage provider.Usage

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				Usage struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
			ContentBlock wireBlock `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("decode sse: %w", err)}
			return
		}

		switch ev.Type {
		case "message_start":
			usage.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				tools[ev.Index] = &pending{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				ch <- provider.Event{Type: provider.EventTextDelta, Text: ev.Delta.Text}
			case "input_json_delta":
				if p := tools[ev.Index]; p != nil {
					p.input.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "content_block_stop":
			if p := tools[ev.Index]; p != nil {
				input := p.input.String()
				if input == "" {
					input = "{}"
				}
				ch <- provider.Event{Type: provider.EventToolCall, Block: &provider.Block{
					Type: provider.BlockToolUse, ID: p.id, Name: p.name, Input: json.RawMessage(input),
				}}
				delete(tools, ev.Index)
			}
		case "message_delta":
			if ev.Usage.OutputTokens > 0 {
				usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("anthropic stream error")}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}
	u := usage
	ch <- provider.Event{Type: provider.EventDone, Usage: &u}
}
