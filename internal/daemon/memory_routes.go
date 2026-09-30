package daemon

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/recall"
)

type FactJSON struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Body        string `json:"body"`
}

type MemoryHitJSON struct {
	Name    string  `json:"name"`
	Score   float64 `json:"score"`
	Excerpt string  `json:"excerpt"`
}

type MemoryJSON struct {
	Query string          `json:"query"`
	Facts []FactJSON      `json:"facts"`
	Hits  []MemoryHitJSON `json:"hits"`
}

// handleMemory lists the fact files, or with ?q= runs the recall search the
// model's recall tool runs, narrowed to facts. It is not scoped to a
// session: this is the operator looking at their own memory.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	if s.op.Facts == nil {
		writeError(w, http.StatusServiceUnavailable, "memory is not available on this daemon")
		return
	}
	q := r.URL.Query().Get("q")
	out := MemoryJSON{Query: q, Facts: []FactJSON{}, Hits: []MemoryHitJSON{}}
	if q == "" {
		for _, f := range s.op.Facts.Facts() {
			out.Facts = append(out.Facts, FactJSON{Name: f.Name, Type: f.Type, Description: f.Description, Body: f.Body})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if s.op.Recall == nil {
		writeError(w, http.StatusServiceUnavailable, "recall search is unavailable")
		return
	}
	hits, err := s.op.Recall.Search(r.Context(), recall.Query{Text: q, Kinds: []string{recall.KindFact}})
	if err != nil {
		// The backend tokenizes the query before MATCH, so an error here is
		// the backend's, not the caller's.
		slog.Error("memory search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "search: %v", err)
		return
	}
	for _, h := range hits {
		out.Hits = append(out.Hits, MemoryHitJSON{Name: h.ID, Score: h.Score, Excerpt: h.Excerpt})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleDeleteFact removes a fact file, reloads the cache the next turn
// assembles from, and drops the fact from the recall index. An index error
// is logged, not returned: the file is gone, which is what the prompt is
// built from, and the tombstone feed retries the vector.
func (s *Server) handleDeleteFact(w http.ResponseWriter, r *http.Request) {
	if s.op.Facts == nil {
		writeError(w, http.StatusServiceUnavailable, "memory is not available on this daemon")
		return
	}
	name := r.PathValue("name")
	if err := memory.Delete(s.op.Facts.Dir(), name); errors.Is(err, memory.ErrNoFact) {
		writeError(w, http.StatusNotFound, "%v", err)
		return
	} else if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	s.op.Facts.Reload()
	if s.op.FactIndex != nil {
		if err := s.op.FactIndex.UnindexFact(r.Context(), name); err != nil {
			slog.Warn("fact deleted but not unindexed; the tombstone feed will retry", "fact", name, "error", err)
		}
	}
	audit(r, "delete_fact", name)
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}
