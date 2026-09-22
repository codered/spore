package daemon

import (
	"net/http"
	"time"

	"github.com/codered/spore/internal/store"
)

// usageWindow is how far back the per-day usage reaches.
const usageWindow = 30 * 24 * time.Hour

// UsageJSON is GET /api/usage: the named session's totals per model, and every
// session's totals per UTC day and model over the last usageWindow.
type UsageJSON struct {
	Session []store.UsageRow      `json:"session"`
	Days    []store.DailyUsageRow `json:"days"`
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	out := UsageJSON{Session: []store.UsageRow{}, Days: []store.DailyUsageRow{}}
	if id := r.URL.Query().Get("session"); id != "" {
		if _, ok := s.findSession(w, r, id); !ok {
			return
		}
		rows, err := s.store.SessionUsage(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "session usage: %v", err)
			return
		}
		if rows != nil {
			out.Session = rows
		}
	}
	days, err := s.store.DailyUsage(r.Context(), time.Now().Add(-usageWindow))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "daily usage: %v", err)
		return
	}
	if days != nil {
		out.Days = days
	}
	writeJSON(w, http.StatusOK, out)
}
