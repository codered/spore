package main

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

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// client talks to the daemon over the same HTTP API the web UI uses. Keeping
// the CLI on that one path is the point: a bug in the API is a bug both
// clients hit, so neither drifts into being the only tested one.
type client struct {
	base string
	// short is for request/response calls; streamClient deliberately uses a client
	// with no timeout, because an SSE connection is meant to stay open.
	short        *http.Client
	streamClient *http.Client
}

func newClient(addr string) *client {
	return &client{
		base:         "http://" + addr,
		short:        &http.Client{Timeout: 30 * time.Second},
		streamClient: &http.Client{},
	}
}

func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.short.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s", method, path, e.Error)
		}
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if out != nil {
		return json.Unmarshal(payload, out)
	}
	return nil
}

func (c *client) health(ctx context.Context) error {
	return c.do(ctx, "GET", "/healthz", nil, nil)
}

func (c *client) createSession(ctx context.Context, title string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/api/sessions", map[string]string{"title": title}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *client) send(ctx context.Context, sessionID, text string) error {
	return c.do(ctx, "POST", "/api/sessions/"+sessionID+"/messages",
		map[string]string{"text": text}, nil)
}

func (c *client) resolve(ctx context.Context, sessionID string, pendingID int64, ans policy.Answer) error {
	return c.do(ctx, "POST",
		fmt.Sprintf("/api/sessions/%s/approvals/%d", sessionID, pendingID),
		map[string]any{"allow": ans.Allow, "scope": string(ans.Scope)}, nil)
}

// streamFrom reads the session's server-sent events until ctx is cancelled,
// the connection drops, or fn returns an error. It closes `connected` once
// the stream is actually open, which is what lets a caller post a message
// knowing that no event published in the meantime can be missed — attaching
// in a goroutine and posting immediately would race.
func (c *client) streamFrom(ctx context.Context, sessionID string, connected chan<- struct{}, fn func(daemon.WireEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+"/api/sessions/"+sessionID+"/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	res, err := c.streamClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("attach to session %s: %s", sessionID, res.Status)
	}
	if connected != nil {
		close(connected)
	}

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue // blank separators and ": ping" heartbeats
		}
		var ev daemon.WireEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			continue // a malformed frame is not worth dropping the session for
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}
