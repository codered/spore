package web

import (
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/codered/spore/internal/tool"
	"golang.org/x/net/html"
)

type fetchTool struct {
	hc        *http.Client
	userAgent string
	maxBytes  int
}

// extraPrivateNets are non-public ranges net.IP has no predicate for:
// carrier-grade NAT, which Tailscale also hands out, and "this network".
var extraPrivateNets = []*net.IPNet{mustCIDR("100.64.0.0/10"), mustCIDR("0.0.0.0/8")}

func mustCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isPrivateIP reports whether ip is loopback, private, link-local (which
// covers the 169.254.169.254 cloud-metadata address) or otherwise not on the
// public internet. Used to keep web_fetch off the LAN and the machine itself.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range extraPrivateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// guardedDialer refuses to connect to a private address. It resolves the
// host once and dials the addresses it checked, never the name: dialing the
// name would resolve it a second time, and a DNS-rebinding server can answer
// that second lookup with 127.0.0.1. Every connection passes through here,
// so each redirect hop is checked too.
type guardedDialer struct {
	lookup func(ctx context.Context, host string) ([]net.IPAddr, error)
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (g guardedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("web_fetch: %s has no addresses", host)
	}
	// Refuse if any address is private, not only the first: the dial below
	// falls through to later addresses when an earlier one fails.
	for _, ip := range ips {
		if isPrivateIP(ip.IP) {
			return nil, fmt.Errorf("web_fetch: refusing %s: it resolves to the private address %s (set [web] allow_private_hosts to allow)", host, ip.IP)
		}
	}
	var firstErr error
	for _, ip := range ips {
		c, err := g.dial(ctx, network, net.JoinHostPort(ip.IP.String(), port))
		if err == nil {
			return c, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// guardedTransport is http.DefaultTransport with the private-address check
// on every dial. It does not use a proxy: a proxy resolves and connects to
// the target itself, out of reach of the check.
func guardedTransport() *http.Transport {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DialContext = guardedDialer{lookup: net.DefaultResolver.LookupIPAddr, dial: d.DialContext}.DialContext
	return t
}

// checkRedirect re-validates the scheme on every hop. Go's transport already
// refuses a non-http(s) scheme, so a 302 to file:///etc/passwd fails today —
// but that is incidental protection from the stdlib, not this tool's own
// decision. Stating it here keeps the guarantee if that ever changes.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to %q: only http and https are allowed", req.URL.Scheme)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

// NewFetchTool builds the web_fetch tool. Unless allowPrivate is set, a
// client with no transport of its own gets guardedTransport, which refuses
// loopback, LAN, link-local and cloud-metadata addresses.
func NewFetchTool(hc *http.Client, userAgent string, maxBytes int, allowPrivate bool) tool.Tool {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	if !allowPrivate && hc.Transport == nil {
		hc.Transport = guardedTransport()
	}
	if hc.CheckRedirect == nil {
		hc.CheckRedirect = checkRedirect
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(tool.OutputLimit(ctx, f.maxBytes))))
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

// tagRE matches an actual HTML tag. A "<" that is not followed by a letter or
// "/" is literal text, and an unterminated tag has no closing ">" to match.
var tagRE = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// stripTags removes the markup Brave uses to highlight matched terms, and
// resolves entities so the model reads "AT&T" rather than "AT&amp;T". A naive
// depth counter would swallow everything between a literal "<" and the next
// ">" — a snippet like "x < y comparisons" would silently lose its tail.
func stripTags(s string) string {
	return strings.TrimSpace(stdhtml.UnescapeString(tagRE.ReplaceAllString(s, "")))
}
