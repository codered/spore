// Package web implements the web_search and web_fetch builtins. Search sits
// behind a provider interface so Tavily or DDG can replace Brave without the
// tool changing.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/tool"
)

type Hit struct {
	Title   string
	URL     string
	Snippet string
}

type SearchProvider interface {
	Search(ctx context.Context, query string, count int) ([]Hit, error)
}

// Brave is the first SearchProvider: a clean paid API, no scraping.
type Brave struct {
	APIKey  string
	BaseURL string
	HC      *http.Client
}

func NewBrave(apiKey string, hc *http.Client) *Brave {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	return &Brave{APIKey: apiKey, BaseURL: "https://api.search.brave.com/res/v1/web/search", HC: hc}
}

func (b *Brave) Search(ctx context.Context, query string, count int) ([]Hit, error) {
	if count <= 0 || count > 20 {
		count = 5
	}
	u, err := url.Parse(b.BaseURL)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("q", query)
	q.Set("count", strconv.Itoa(count))
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.APIKey)

	resp, err := b.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("brave search: %s", resp.Status)
	}
	var body struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("brave search: decode: %w", err)
	}
	hits := make([]Hit, 0, len(body.Web.Results))
	for _, r := range body.Web.Results {
		hits = append(hits, Hit{
			Title:   stripTags(r.Title),
			URL:     r.URL,
			Snippet: stripTags(r.Description),
		})
	}
	return hits, nil
}

type searchTool struct{ p SearchProvider }

// NewSearchTool wraps any SearchProvider as the web_search builtin.
func NewSearchTool(p SearchProvider) tool.Tool { return searchTool{p: p} }

func (searchTool) Name() string        { return "web_search" }
func (searchTool) Description() string { return "Search the web and return titles, URLs and snippets." }
func (searchTool) ReadOnly() bool      { return true }
func (searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"query":{"type":"string","description":"Search query."},
"count":{"type":"integer","description":"Number of results, 1-20. Defaults to 5."}},
"required":["query"]}`)
}

func (s searchTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(a.Query) == "" {
		return "", fmt.Errorf("query is required")
	}
	hits, err := s.p.Search(ctx, a.Query, a.Count)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no results", nil
	}
	var b strings.Builder
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. %s\n   %s\n   %s\n", i+1, h.Title, h.URL, h.Snippet)
	}
	return b.String(), nil
}

// New builds the web tools for a config. web_search is omitted entirely when
// no key is configured, so the model is never offered a tool that must fail.
func New(cfg config.WebConfig, maxBytes int) []tool.Tool {
	hc := &http.Client{Timeout: 30 * time.Second}
	tools := []tool.Tool{NewFetchTool(hc, cfg.UserAgent, maxBytes)}
	if cfg.SearchProvider == "brave" && cfg.BraveAPIKey != "" {
		tools = append(tools, NewSearchTool(NewBrave(cfg.BraveAPIKey, hc)))
	}
	return tools
}
