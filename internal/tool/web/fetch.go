package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/codered/spore/internal/tool"
	"golang.org/x/net/html"
)

type fetchTool struct {
	hc        *http.Client
	userAgent string
	maxBytes  int
}

func NewFetchTool(hc *http.Client, userAgent string, maxBytes int) tool.Tool {
	if hc == nil {
		hc = http.DefaultClient
	}
	if userAgent == "" {
		userAgent = "spore/0.1"
	}
	if maxBytes <= 0 {
		maxBytes = 2 << 20
	}
	return fetchTool{hc: hc, userAgent: userAgent, maxBytes: maxBytes}
}

func (fetchTool) Name() string { return "web_fetch" }
func (fetchTool) Description() string {
	return "Fetch an http or https URL and return its readable text content."
}
func (fetchTool) ReadOnly() bool { return true }
func (fetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"url":{"type":"string","description":"Absolute http or https URL."}},
"required":["url"]}`)
}

func (f fetchTool) Call(ctx context.Context, args json.RawMessage) (string, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil {
		return "", fmt.Errorf("invalid url %q: %w", a.URL, err)
	}
	// Only http(s). file:// and friends would turn a web tool into an
	// unpoliced filesystem read.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url %q: only http and https are allowed", a.URL)
	}
	if u.Host == "" {
		return "", fmt.Errorf("url %q: missing host", a.URL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", f.userAgent)
	req.Header.Set("Accept", "text/html,text/plain;q=0.9,*/*;q=0.5")

	resp, err := f.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(f.maxBytes)))
	if err != nil {
		return "", err
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") {
		return string(raw), nil
	}
	text, err := htmlToText(string(raw))
	if err != nil {
		return "", err
	}
	return text, nil
}

// blockTags force a line break; skipTags and their subtrees are dropped.
var (
	skipTags  = map[string]bool{"script": true, "style": true, "noscript": true, "svg": true, "nav": true, "footer": true}
	blockTags = map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "tr": true, "section": true,
		"article": true, "pre": true, "blockquote": true, "table": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	}
)

// htmlToText renders a document as readable plain text: headings become
// markdown-style headings, list items get bullets, and script/style content
// is dropped. It is deliberately small — a faithful HTML-to-markdown
// converter is a dependency spore does not need to read a page.
func htmlToText(body string) (string, error) {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skipTags[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
			return
		}
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "title":
				b.WriteString("# ")
			case len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6':
				b.WriteString("\n" + strings.Repeat("#", int(n.Data[1]-'0')) + " ")
			case n.Data == "li":
				b.WriteString("\n- ")
			case blockTags[n.Data]:
				b.WriteString("\n")
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockTags[n.Data] {
			b.WriteString("\n")
		}
	}
	walk(doc)
	return collapseBlankLines(b.String()), nil
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	var out []string
	blank := 0
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// stripTags removes the markup Brave uses to highlight matched terms.
func stripTags(s string) string {
	var b strings.Builder
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
