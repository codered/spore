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
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/provider/httpstream"
)

type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
	stall   time.Duration // httpstream.StallTimeout; shorter in tests
	// cacheModels names the models that get Anthropic-style cache_control
	// markers. Nil sends none: a plain OpenAI endpoint has no use for them.
	cacheModels *regexp.Regexp
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = httpstream.NewHTTPClient()
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), apiKey: apiKey, hc: hc, stall: httpstream.StallTimeout}
}

// WithCacheControl sends Anthropic-style cache_control markers to the models
// re matches, for a proxy such as LiteLLM that forwards them to Claude. A nil
// re sends none.
func (c *Client) WithCacheControl(re *regexp.Regexp) *Client {
	c.cacheModels = re
	return c
}

func (c *Client) Name() string { return "openaicompat" }

// cacheControl is the marker LiteLLM forwards to Anthropic. The 5-minute TTL
// is the default; see the anthropic adapter for why spore uses no other.
var cacheControl = map[string]any{"type": "ephemeral"}

// textBuilder gathers a message's text. Unmarked, it is one string, as the
// wire has always carried it. A breakpoint splits it into text parts with the
// marker on the part that ends at the breakpoint, so text after it -- the
// per-turn environment -- stays outside the cached prefix.
type textBuilder struct {
	parts  []map[string]any
	cur    strings.Builder
	marked bool
}

func (t *textBuilder) write(s string, mark bool) {
	t.cur.WriteString(s)
	if !mark {
		return
	}
	if t.cur.Len() > 0 {
		t.flush()
	}
	// An empty part may not carry a marker; the one before it ends at the
	// same place.
	if n := len(t.parts); n > 0 {
		t.parts[n-1]["cache_control"] = cacheControl
		t.marked = true
	}
}

func (t *textBuilder) flush() {
	t.parts = append(t.parts, map[string]any{"type": "text", "text": t.cur.String()})
	t.cur.Reset()
}

func (t *textBuilder) len() int { return len(t.parts) + t.cur.Len() }

// content is the wire value: a string unless a marker landed.
func (t *textBuilder) content() any {
	if !t.marked {
		var all strings.Builder
		for _, p := range t.parts {
			all.WriteString(p["text"].(string))
		}
		all.WriteString(t.cur.String())
		return all.String()
	}
	if t.cur.Len() > 0 {
		t.flush()
	}
	return t.parts
}

// ListModels asks the endpoint which models it serves: GET {base_url}/models
// in the OpenAI shape, data[].id. llama-server, LiteLLM and Unsloth Studio
// all answer it. The ids come back bare and sorted; the caller adds the
// provider prefix. The caller's context bounds the call: the client's own
// timeout is sized for a long completion, not a listing.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("list models: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	ids := make([]string, 0, len(body.Data))
	for _, d := range body.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// toWire flattens spore messages into OpenAI's shape: assistant tool calls
// become `tool_calls`, and each tool result becomes its own `tool` message.
// With cache set, blocks marked CacheBreak carry cache_control.
func toWire(cache bool, system []provider.Block, msgs []provider.Message) []map[string]any {
	out := []map[string]any{}
	var sys textBuilder
	for _, b := range system {
		sys.write(b.Text, cache && b.CacheBreak)
	}
	if sys.len() > 0 {
		out = append(out, map[string]any{"role": "system", "content": sys.content()})
	}
	for _, m := range msgs {
		var text textBuilder
		var calls []map[string]any
		lastResult := -1 // index in out of this message's last tool result
		for _, b := range m.Blocks {
			switch b.Type {
			case provider.BlockText:
				text.write(b.Text, cache && b.CacheBreak)
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
				res := map[string]any{"role": "tool", "tool_call_id": b.ID, "content": content}
				// Anthropic rejects a marker inside tool_result.content,
				// where LiteLLM puts a tool message's parts. On the
				// message, LiteLLM moves it onto the tool_result.
				if cache && b.CacheBreak {
					res["cache_control"] = cacheControl
				}
				out = append(out, res)
				lastResult = len(out) - 1
			}
		}
		if text.len() == 0 && len(calls) == 0 {
			continue
		}
		role := string(m.Role)
		if m.Role == provider.RoleTool {
			// Text riding on the results message is the environment Assemble
			// appends to the tail. An OpenAI tool message must carry a
			// tool_call_id, so it cannot go out as a bare tool message. Nor
			// as a user message: the model reads one after every tool round
			// as the user sending something empty, and says so. It joins the
			// last result instead, the nearest the wire has to the Anthropic
			// shape, where it sits beside the results in one message.
			//
			// A marked result cannot take it: the marker would cache it
			// with the result, and no later turn reads an entry holding an
			// old environment. It goes as a user message instead, which
			// Anthropic merges into the results' turn -- the native shape --
			// so the empty-message reading above does not arise.
			if plain, ok := text.content().(string); ok && lastResult >= 0 && out[lastResult]["cache_control"] == nil {
				out[lastResult]["content"] = out[lastResult]["content"].(string) + plain
				continue
			}
			role = string(provider.RoleUser)
		}
		msg := map[string]any{"role": role, "content": text.content()}
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
		"messages":       toWire(c.cacheModels != nil && c.cacheModels.MatchString(req.Model), req.System, req.Messages),
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

	// The stream is cancelled through its own context when it stalls.
	ctx, cancel := context.WithCancel(ctx)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(buf))
	if err != nil {
		cancel()
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.hc.Do(httpReq)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("openai-compatible request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("openai-compatible %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}

	ch := make(chan provider.Event, 32)
	go parse(httpstream.WatchStall(resp.Body, cancel, c.stall), ch)
	return ch, nil
}

// wireUsage is the usage object as OpenAI and LiteLLM send it. Cache reads
// arrive as prompt_tokens_details.cached_tokens (OpenAI) or
// cache_read_input_tokens (LiteLLM's Anthropic passthrough), writes as
// prompt_tokens_details.cache_write_tokens or cache_creation_input_tokens.
// prompt_tokens counts both.
type wireUsage struct {
	PromptTokens       int `json:"prompt_tokens"`
	CompletionTokens   int `json:"completion_tokens"`
	CacheReadTokens    int `json:"cache_read_input_tokens"`
	CacheCreateTokens  int `json:"cache_creation_input_tokens"`
	PromptTokenDetails *struct {
		CachedTokens     int `json:"cached_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage makes InputTokens the uncached remainder, as provider.Usage
// defines it, not the prompt size.
func (w wireUsage) toUsage() provider.Usage {
	read, write := w.CacheReadTokens, w.CacheCreateTokens
	if d := w.PromptTokenDetails; d != nil {
		read = max(read, d.CachedTokens)
		write = max(write, d.CacheWriteTokens)
	}
	return provider.Usage{
		InputTokens:      max(w.PromptTokens-read-write, 0),
		OutputTokens:     w.CompletionTokens,
		CacheReadTokens:  read,
		CacheWriteTokens: write,
	}
}

func parse(rc io.ReadCloser, ch chan<- provider.Event) {
	defer close(ch)
	defer func() { _ = rc.Close() }()

	type pending struct {
		id, name string
		args     strings.Builder
	}
	calls := map[int]*pending{}
	var usage provider.Usage
	var sawDone, hitMaxTokens bool

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
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *wireUsage `json:"usage"`
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
			usage = chunk.Usage.toUsage()
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason == "length" {
				hitMaxTokens = true
			}
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
	ch <- provider.Event{Type: provider.EventDone, Usage: &u, HitMaxTokens: hitMaxTokens}
}
