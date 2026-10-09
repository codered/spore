package daemon

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// ModelsJSON is GET /api/models and the answer to both PUTs, so a client
// redraws from whichever it got last.
type ModelsJSON = models.View

// AttachModels installs /model's service. Without it the routes answer 503.
func (s *Server) AttachModels(m *models.Service) { s.models = m }

type modelChoice struct {
	Op  string `json:"op"`
	Ref string `json:"ref"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	id := r.URL.Query().Get("session")
	if id != "" {
		if _, ok := s.findSession(w, r, id); !ok {
			return
		}
	}
	v, err := s.models.View(r.Context(), id, r.URL.Query().Get("fresh") == "1")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "models: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleSetSessionModel(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	var in modelChoice
	// A model choice is one op and one ref; anything larger is not a request this route serves.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	if !router.ValidSite(in.Op) {
		writeError(w, http.StatusBadRequest, "unknown operation %q", in.Op)
		return
	}
	if in.Op != router.SiteChat && in.Op != router.SiteSubagent {
		writeError(w, http.StatusBadRequest, "%s is set for every session: PUT /api/routing", in.Op)
		return
	}
	s.applyModel(w, r, id, in)
}

func (s *Server) handleSetRouting(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		writeError(w, http.StatusServiceUnavailable, "model selection is not available")
		return
	}
	// The session is optional: it only makes the answer include that
	// session's chat and subagent rows.
	id := r.URL.Query().Get("session")
	if id != "" {
		if _, ok := s.findSession(w, r, id); !ok {
			return
		}
	}
	var in modelChoice
	// A model choice is one op and one ref; anything larger is not a request this route serves.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad body: %v", err)
		return
	}
	if !router.ValidSite(in.Op) {
		writeError(w, http.StatusBadRequest, "unknown operation %q", in.Op)
		return
	}
	if !router.IsGlobalSite(in.Op) {
		writeError(w, http.StatusBadRequest, "%s is set per session: PUT /api/sessions/{id}/model", in.Op)
		return
	}
	s.applyModel(w, r, id, in)
}

func (s *Server) applyModel(w http.ResponseWriter, r *http.Request, id string, in modelChoice) {
	v, err := s.models.Set(r.Context(), id, in.Op, in.Ref)
	if errors.Is(err, models.ErrInvalid) {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "%v", err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
