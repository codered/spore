package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/provider"
)

// SkillJSON is one skill in the /skills listing. BodyTokens is an estimate;
// Loaded is derived by scanning the transcript for skill_load tool calls
// rather than by keeping session state.
type SkillJSON struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	BodyTokens  int    `json:"body_tokens"`
	Loaded      bool   `json:"loaded"`
}

// SkillsJSON is the /skills response. Errors are the per-file load errors
// from the last read, so a skill with broken frontmatter is visible rather
// than silently absent from the index.
type SkillsJSON struct {
	Skills []SkillJSON `json:"skills"`
	Errors []string    `json:"errors"`
}

// handleSkills lists the skills available to a session: name, description,
// estimated body size, a loaded marker derived from the transcript, and the
// per-file errors the last load returned. The endpoint reads through the
// agent's skills cache, so a skill_install is visible immediately.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.findSession(w, r, id)
	if !ok {
		return
	}

	rows, err := s.store.Messages(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read messages: %v", err)
		return
	}
	_, through, err := s.store.Summary(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "read summary boundary: %v", err)
		return
	}

	// A skill is loaded only when skill_load has a successful tool_result. A
	// tool_use with no result means the turn was interrupted; an error result
	// means the load failed. Pair calls and results by ID so either case stays
	// unmarked.
	pending := make(map[string]string)
	loaded := make(map[string]bool)
	for _, m := range rows {
		if m.Seq <= through {
			continue // folded messages are no longer in the live model context
		}
		var blocks []provider.Block
		if err := json.Unmarshal(m.BlocksJSON, &blocks); err != nil {
			continue
		}
		for _, b := range blocks {
			switch {
			case b.Type == provider.BlockToolUse && b.Name == "skill_load":
				var args struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(b.Input, &args) == nil && args.Name != "" {
					pending[b.ID] = args.Name
				}
			case b.Type == provider.BlockToolResult && !b.IsError:
				if name, ok := pending[b.ID]; ok {
					loaded[name] = true
					delete(pending, b.ID)
				}
			}
		}
	}

	out := SkillsJSON{Skills: []SkillJSON{}, Errors: []string{}}
	dir := s.cfg.SkillsDir(sess.Workspace)
	if s.agent != nil && s.agent.Skills != nil {
		for _, sk := range s.agent.Skills.Skills(dir) {
			out.Skills = append(out.Skills, SkillJSON{
				Name:        sk.Name,
				Description: sk.Description,
				BodyTokens:  agent.EstimateTokens(sk.Body),
				Loaded:      loaded[sk.Name],
			})
		}
		for _, e := range s.agent.Skills.Errors(dir) {
			out.Errors = append(out.Errors, e.Error())
		}
	}
	writeJSON(w, http.StatusOK, out)
}
