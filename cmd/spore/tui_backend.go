package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/tui"
)

// tuiBackend adapts the daemon client to the interface internal/tui drives.
type tuiBackend struct {
	c        *client
	showCost bool
}

var _ tui.Backend = tuiBackend{}

func (b tuiBackend) Sessions(ctx context.Context) ([]daemon.SessionJSON, error) {
	return b.c.sessions(ctx)
}

func (b tuiBackend) Transcript(ctx context.Context, id string) (daemon.TranscriptJSON, error) {
	return b.c.transcript(ctx, id)
}

// Events opens the global feed and returns once the daemon has accepted it,
// so the caller's connectedMsg is true when it is sent. The channel closes
// when the stream ends for any reason.
func (b tuiBackend) Events(ctx context.Context) (<-chan daemon.WireEvent, error) {
	ch := make(chan daemon.WireEvent, 256)
	connected := make(chan struct{})
	errc := make(chan error, 1)
	go func() {
		defer close(ch)
		errc <- b.c.streamAll(ctx, connected, func(ev daemon.WireEvent) error {
			select {
			case ch <- ev:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-connected:
		return ch, nil
	case err := <-errc:
		if err == nil {
			err = errors.New("the event feed closed before it opened")
		}
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b tuiBackend) Send(ctx context.Context, id, text string) error { return b.c.send(ctx, id, text) }

func (b tuiBackend) Stop(ctx context.Context, id string) error { return b.c.stop(ctx, id) }

func (b tuiBackend) Resolve(ctx context.Context, id string, pendingID int64, ans policy.Answer) error {
	return b.c.resolve(ctx, id, pendingID, ans)
}

func (b tuiBackend) CancelAgent(ctx context.Context, parent, child string) error {
	return b.c.cancelAgent(ctx, parent, child)
}

// NewSession opens a chat session. An empty workspace means the current
// directory, as `spore chat` itself does; a relative one is resolved from it.
func (b tuiBackend) NewSession(ctx context.Context, workspace string) (string, error) {
	dir, err := sessionWorkspace(workspace)
	if err != nil {
		return "", err
	}
	return b.c.createSession(ctx, "chat", dir)
}

func (b tuiBackend) Slash(ctx context.Context, id, cmd string) (string, error) {
	switch cmd {
	case "clear":
		if err := b.c.clear(ctx, id); err != nil {
			return "", err
		}
		return "cleared", nil
	case "compact":
		res, err := b.c.compact(ctx, id)
		if err != nil {
			return "", err
		}
		return compactSummary(res), nil
	case "context", "usage":
		data, err := b.c.getTranscript(ctx, id)
		if err != nil {
			return "", err
		}
		if cmd == "context" {
			return formatContext(data), nil
		}
		return formatUsage(data, b.showCost), nil
	case "skills":
		list, err := b.c.listSkills(ctx, id)
		if err != nil {
			return "", err
		}
		return formatSkills(list), nil
	case "agents":
		list, err := b.c.listAgents(ctx, id)
		if err != nil {
			return "", err
		}
		return formatAgents(list), nil
	}
	return "", fmt.Errorf("unknown command: %s", cmd)
}

var _ tui.Views = tuiBackend{}

// viewErr maps a missing route to tui.ErrOlderDaemon. The daemon's mux answers
// an unknown route with a plain-text 404, which client.do reports as
// "<METHOD> <path>: 404 Not Found"; a known route's own 404 (an unknown
// session) carries a JSON error message instead, and passes through.
func viewErr(err error) error {
	if err != nil && strings.HasSuffix(err.Error(), ": 404 Not Found") {
		return tui.ErrOlderDaemon
	}
	var he *httpError
	if errors.As(err, &he) && he.Status == http.StatusServiceUnavailable {
		return tui.Unavailable{Msg: he.Msg}
	}
	return err
}

func (b tuiBackend) Skills(ctx context.Context, sessionID string) (daemon.SkillsJSON, error) {
	var out daemon.SkillsJSON
	err := b.c.do(ctx, "GET", "/api/sessions/"+sessionID+"/skills?body=1", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) Agents(ctx context.Context, sessionID string) (daemon.AgentsJSON, error) {
	list, err := b.c.listAgents(ctx, sessionID)
	return list, viewErr(err)
}

func (b tuiBackend) Jobs(ctx context.Context) ([]daemon.JobJSON, error) {
	var out []daemon.JobJSON
	err := b.c.do(ctx, "GET", "/api/jobs", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) JobRuns(ctx context.Context, jobID int64) ([]daemon.JobRunJSON, error) {
	var out []daemon.JobRunJSON
	err := b.c.do(ctx, "GET", "/api/jobs/"+strconv.FormatInt(jobID, 10)+"/runs", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) Usage(ctx context.Context, sessionID string) (daemon.UsageJSON, error) {
	var out daemon.UsageJSON
	path := "/api/usage"
	if sessionID != "" {
		path += "?session=" + sessionID
	}
	err := b.c.do(ctx, "GET", path, nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) CancelJob(ctx context.Context, id int64) error {
	return viewErr(b.c.do(ctx, "DELETE", "/api/jobs/"+strconv.FormatInt(id, 10), nil, nil))
}

func (b tuiBackend) DeleteSessions(ctx context.Context, ids []string, all, discord bool) (daemon.DeleteSessionsJSON, error) {
	var out daemon.DeleteSessionsJSON
	err := b.c.do(ctx, "POST", "/api/sessions/delete", map[string]any{"ids": ids, "all": all, "discord": discord}, &out)
	return out, err
}

func (b tuiBackend) MarkSeen(ctx context.Context, id string) error {
	return b.c.do(ctx, "POST", "/api/sessions/"+id+"/seen", nil, nil)
}

func (b tuiBackend) Refine(ctx context.Context, id, instructions string) (string, error) {
	var out daemon.RefineResultJSON
	if err := b.c.do(ctx, "POST", "/api/sessions/"+id+"/refine", map[string]string{"instructions": instructions}, &out); err != nil {
		return "", err
	}
	if len(out.Dropped) > 0 {
		return out.Note + "\n  dropped: " + strings.Join(out.Dropped, "\n  dropped: "), nil
	}
	return out.Note, nil
}

func (b tuiBackend) RefineRollback(ctx context.Context, id string) (string, error) {
	var out daemon.RollbackJSON
	if err := b.c.do(ctx, "POST", "/api/sessions/"+id+"/refine/rollback", map[string]string{}, &out); err != nil {
		return "", err
	}
	s := fmt.Sprintf("rolled back %d edit(s) from round %s", len(out.RolledBack), out.RoundID)
	if n := len(out.Stale); n > 0 {
		s += fmt.Sprintf("; %d left alone because the file changed since", n)
	}
	if n := len(out.Failed); n > 0 {
		s += fmt.Sprintf("; %d failed", n)
	}
	return s, nil
}

func (b tuiBackend) Refinements(ctx context.Context) ([]daemon.RefinementJSON, error) {
	var out []daemon.RefinementJSON
	err := b.c.do(ctx, "GET", "/api/refinements", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) AcceptRefinement(ctx context.Context, id int64) error {
	return viewErr(b.c.do(ctx, "POST", "/api/refinements/"+strconv.FormatInt(id, 10)+"/accept", nil, nil))
}

func (b tuiBackend) RejectRefinement(ctx context.Context, id int64) error {
	return viewErr(b.c.do(ctx, "POST", "/api/refinements/"+strconv.FormatInt(id, 10)+"/reject", nil, nil))
}

func (b tuiBackend) RollbackRound(ctx context.Context, sessionID, roundID string) error {
	return viewErr(b.c.do(ctx, "POST", "/api/sessions/"+sessionID+"/refine/rollback", map[string]string{"round_id": roundID}, nil))
}

func (b tuiBackend) MCP(ctx context.Context) ([]daemon.MCPServerJSON, error) {
	var out []daemon.MCPServerJSON
	err := b.c.do(ctx, "GET", "/api/mcp", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) Reconnect(ctx context.Context, server string) error {
	return viewErr(b.c.do(ctx, "POST", "/api/mcp/"+url.PathEscape(server)+"/reconnect", nil, nil))
}

func (b tuiBackend) Policy(ctx context.Context) (daemon.PolicyJSON, error) {
	var out daemon.PolicyJSON
	err := b.c.do(ctx, "GET", "/api/policy", nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) Revoke(ctx context.Context, decision, rule string) error {
	return viewErr(b.c.do(ctx, "DELETE", "/api/policy/learned", map[string]string{"decision": decision, "rule": rule}, nil))
}

func (b tuiBackend) Memory(ctx context.Context, query string) (daemon.MemoryJSON, error) {
	var out daemon.MemoryJSON
	path := "/api/memory"
	if query != "" {
		path += "?q=" + url.QueryEscape(query)
	}
	err := b.c.do(ctx, "GET", path, nil, &out)
	return out, viewErr(err)
}

func (b tuiBackend) DeleteFact(ctx context.Context, name string) error {
	return viewErr(b.c.do(ctx, "DELETE", "/api/memory/"+url.PathEscape(name), nil, nil))
}
