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
	"sync"
	"time"

	"github.com/codered/spore/internal/provider"
)

const (
	apiVersion      = "2023-06-01"
	workspaceHeader = "anthropic-workspace-id"
)

type Client struct {
	baseURL     string
	apiKey      string
	workspaceID string
	hc          *http.Client

	// mu guards learnedWS, the default workspace the API reported for this
	// key. It is only consulted when workspaceID is empty.
	mu        sync.RWMutex
	learnedWS string
}

// New builds a client. workspaceID may be empty, in which case requests carry
// no anthropic-workspace-id header and the API acts in the key's default
// workspace. Identity-linked keys that span several workspaces reject that;
// the client then falls back to the default workspace the API names in the
// response and retries once.
func New(baseURL, apiKey, workspaceID string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, workspaceID: workspaceID, hc: hc}
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

	resp, err := c.post(ctx, buf)
	if err != nil {
		return nil, err
	}

	ch := make(chan provider.Event, 32)
	go c.parse(resp.Body, ch)
	return ch, nil
}

// workspace reports the workspace to name on a request: the configured id,
// otherwise the default one the API disclosed on an earlier response.
func (c *Client) workspace() string {
	if c.workspaceID != "" {
		return c.workspaceID
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.learnedWS
}

// post sends one request, retrying once against the default workspace when
// the API demands an anthropic-workspace-id it also names in the response.
func (c *Client) post(ctx context.Context, buf []byte) (*http.Response, error) {
	resp, err := c.send(ctx, buf, c.workspace())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		if c.workspaceID == "" {
			c.remember(resp.Header.Get(workspaceHeader))
		}
		return resp, nil
	}

	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	body := strings.TrimSpace(string(msg))

	// The key spans several workspaces, so the header is mandatory. The
	// response names the default one; adopt it and try again.
	if fallback := resp.Header.Get(workspaceHeader); fallback != "" && c.workspace() == "" && strings.Contains(body, workspaceHeader) {
		c.remember(fallback)
		retry, err := c.send(ctx, buf, fallback)
		if err != nil {
			return nil, err
		}
		if retry.StatusCode == http.StatusOK {
			return retry, nil
		}
		defer retry.Body.Close()
		msg, _ = io.ReadAll(io.LimitReader(retry.Body, 4096))
		return nil, fmt.Errorf("anthropic %s: %s", retry.Status, strings.TrimSpace(string(msg)))
	}
	if c.workspace() == "" && strings.Contains(body, workspaceHeader) {
		return nil, fmt.Errorf("anthropic %s: %s (set workspace_id on the provider or $ANTHROPIC_WORKSPACE_ID)", resp.Status, body)
	}
	return nil, fmt.Errorf("anthropic %s: %s", resp.Status, body)
}

func (c *Client) remember(id string) {
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learnedWS = id
}

func (c *Client) send(ctx context.Context, buf []byte, workspaceID string) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", apiVersion)
	if workspaceID != "" {
		httpReq.Header.Set(workspaceHeader, workspaceID)
	}
	resp, err := c.hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic request: %w", err)
	}
	return resp, nil
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
	var sawStop bool

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
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
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
		case "message_stop":
			sawStop = true
		case "error":
			ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("anthropic stream error: %s: %s", ev.Error.Type, ev.Error.Message)}
			return
		}
	}
	if err := sc.Err(); err != nil {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("read stream: %w", err)}
		return
	}
	if !sawStop {
		ch <- provider.Event{Type: provider.EventError, Err: fmt.Errorf("stream ended without message_stop (truncated response)")}
		return
	}
	u := usage
	ch <- provider.Event{Type: provider.EventDone, Usage: &u}
}
