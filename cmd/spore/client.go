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
	//nolint:gosec // G704: c.base is the local daemon URL, path is from the API
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr) //nolint:gosec // G704: c.base is the local daemon URL, path is from the API
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	//nolint:gosec // G704: c.base is the local daemon URL
	res, err := c.short.Do(req) //nolint:gosec // G704: c.base is the local daemon URL
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
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

func (c *client) createSession(ctx context.Context, title, workspace string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/api/sessions",
		map[string]string{"title": title, "workspace": workspace}, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// setWorkspace re-roots an existing session. It is the deliberate exception
// to "the root is fixed at creation": --workspace on a resume rewrites the
// row, because the human asking for it is the one who chose the original.
func (c *client) setWorkspace(ctx context.Context, sessionID, workspace string) error {
	return c.do(ctx, "PATCH", "/api/sessions/"+sessionID,
		map[string]string{"workspace": workspace}, nil)
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

// clear moves the live-context boundary through the current last message.
// The daemon selects the sequence atomically so future messages stay visible.
func (c *client) clear(ctx context.Context, sessionID string) error {
	return c.do(ctx, "POST", "/api/sessions/"+sessionID+"/clear", nil, nil)
}

// compact triggers a manual compaction of the session via POST /compact.
func (c *client) compact(ctx context.Context, sessionID string) (daemon.CompactJSON, error) {
	var out daemon.CompactJSON
	err := c.do(ctx, "POST", "/api/sessions/"+sessionID+"/compact", nil, &out)
	return out, err
}

// getTranscript fetches the full transcript for /context and /usage.
func (c *client) getTranscript(ctx context.Context, sessionID string) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "GET", "/api/sessions/"+sessionID, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// skillJSON is one skill in the /skills listing. It mirrors the daemon's
// SkillJSON: name, description, estimated body size, and a loaded marker
// derived from the transcript.
type skillJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	BodyTokens  int    `json:"body_tokens"`
	Loaded      bool   `json:"loaded"`
}

// skillListJSON is the /skills response.
type skillListJSON struct {
	Skills []skillJSON `json:"skills"`
	Errors []string    `json:"errors"`
}

// listSkills fetches the skills available to a session.
func (c *client) listSkills(ctx context.Context, sessionID string) (skillListJSON, error) {
	var out skillListJSON
	if err := c.do(ctx, "GET", "/api/sessions/"+sessionID+"/skills", nil, &out); err != nil {
		return skillListJSON{}, err
	}
	return out, nil
}

// agentListJSON is the /agents response, shared with the daemon so the two
// cannot drift apart.
type agentListJSON = daemon.AgentsJSON

// listAgents fetches the sub-agents a session has launched.
func (c *client) listAgents(ctx context.Context, sessionID string) (agentListJSON, error) {
	var out agentListJSON
	if err := c.do(ctx, "GET", "/api/sessions/"+sessionID+"/agents", nil, &out); err != nil {
		return agentListJSON{}, err
	}
	return out, nil
}

// streamPath reads server-sent events from path until ctx is cancelled, the
// connection drops, or fn returns an error. It closes `connected` once the
// stream is actually open, which is what lets a caller post a message knowing
// that no event published in the meantime can be missed.
func (c *client) streamPath(ctx context.Context, path, label string, connected chan<- struct{}, fn func(daemon.WireEvent) error) error {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+path, nil) //nolint:gosec // G704: c.base is the local daemon URL, path is from the API
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	//nolint:gosec // G704: c.base is the local daemon URL, path is from the API
	res, err := c.streamClient.Do(req) //nolint:gosec // G704: c.base is the local daemon URL
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("attach to %s: %s", label, res.Status)
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

// streamFrom reads one session's events.
func (c *client) streamFrom(ctx context.Context, sessionID string, connected chan<- struct{}, fn func(daemon.WireEvent) error) error {
	return c.streamPath(ctx, "/api/sessions/"+sessionID+"/events", "session "+sessionID, connected, fn)
}

// streamAll reads every session's events, each tagged with its session.
func (c *client) streamAll(ctx context.Context, connected chan<- struct{}, fn func(daemon.WireEvent) error) error {
	return c.streamPath(ctx, "/api/events", "the event feed", connected, fn)
}

func (c *client) stop(ctx context.Context, sessionID string) error {
	return c.do(ctx, "POST", "/api/sessions/"+sessionID+"/stop", nil, nil)
}

func (c *client) cancelAgent(ctx context.Context, parent, child string) error {
	return c.do(ctx, "DELETE", "/api/sessions/"+parent+"/agents/"+child, nil, nil)
}

// sessions lists every session, sub-agents included.
func (c *client) sessions(ctx context.Context) ([]daemon.SessionJSON, error) {
	var out []daemon.SessionJSON
	err := c.do(ctx, "GET", "/api/sessions?children=1", nil, &out)
	return out, err
}

func (c *client) transcript(ctx context.Context, sessionID string) (daemon.TranscriptJSON, error) {
	var out daemon.TranscriptJSON
	err := c.do(ctx, "GET", "/api/sessions/"+sessionID, nil, &out)
	return out, err
}
