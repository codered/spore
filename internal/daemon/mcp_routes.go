package daemon

import (
	"errors"
	"net/http"

	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/policy"
)

type MCPToolJSON struct {
	Name     string `json:"name"`
	Decision string `json:"decision"`
	Rule     string `json:"rule"`
	// DependsOnArgs is set when an allow or ask rule with an argument
	// predicate names the tool, so a real call may be decided differently
	// from the empty-argument decision shown.
	DependsOnArgs bool `json:"depends_on_args"`
}

type MCPSkipJSON struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

type MCPServerJSON struct {
	Name      string        `json:"name"`
	Transport string        `json:"transport"`
	State     string        `json:"state"`
	LastError string        `json:"last_error"`
	Tools     []MCPToolJSON `json:"tools"`
	Skipped   []MCPSkipJSON `json:"skipped"`
}

// mcpServersJSON explains each server's tools under the local profile, the
// operator's own. A nil engine leaves the decisions empty.
func mcpServersJSON(status []mcp.ServerStatus, eng *policy.Engine) []MCPServerJSON {
	out := make([]MCPServerJSON, 0, len(status))
	for _, st := range status {
		row := MCPServerJSON{
			Name: st.Name, Transport: st.Transport, State: st.State, LastError: st.LastErr,
			Tools: make([]MCPToolJSON, 0, len(st.Tools)), Skipped: make([]MCPSkipJSON, 0, len(st.Skipped)),
		}
		for _, name := range st.Tools {
			t := MCPToolJSON{Name: name}
			if eng != nil {
				res, dep := eng.ToolDecision(policy.ProfileLocal, name)
				t.Decision, t.Rule, t.DependsOnArgs = string(res.Decision), res.Rule, dep
			}
			row.Tools = append(row.Tools, t)
		}
		for _, sk := range st.Skipped {
			row.Skipped = append(row.Skipped, MCPSkipJSON{Tool: sk.Tool, Reason: sk.Reason})
		}
		out = append(out, row)
	}
	return out
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.op.MCP == nil || !s.op.MCP.Configured() {
		writeError(w, http.StatusServiceUnavailable, "no MCP servers configured")
		return
	}
	var eng *policy.Engine
	if s.guard != nil {
		eng = s.guard.Engine()
	}
	writeJSON(w, http.StatusOK, mcpServersJSON(s.op.MCP.Status(), eng))
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	if s.op.MCP == nil {
		writeError(w, http.StatusServiceUnavailable, "no MCP servers configured")
		return
	}
	name := r.PathValue("server")
	if err := s.op.MCP.Redial(name); errors.Is(err, mcp.ErrUnknownServer) {
		writeError(w, http.StatusNotFound, "no MCP server named %q", name)
		return
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	audit(r, "reconnect", name)
	writeJSON(w, http.StatusAccepted, map[string]string{"reconnecting": name})
}
