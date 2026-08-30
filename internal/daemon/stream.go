package daemon

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// heartbeat keeps an idle SSE connection from being closed by an intermediary
// and gives the client a liveness signal between turns.
const heartbeat = 25 * time.Second

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.findSession(w, r, id); !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Subscribe BEFORE writing the header so no event published between the
	// two is lost by a client that has already been told the stream is open.
	events, unsubscribe := s.hub.Subscribe(id)
	defer unsubscribe()
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// A client attaching after a restart, or as a second client, is told
	// about anything already waiting on a human before it sees any deltas.
	for _, ev := range s.pendingApprovalEvents(r.Context(), id) {
		writeSSE(w, flusher, ev)
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case ev, open := <-events:
			if !open {
				return
			}
			if !writeSSE(w, flusher, ev) {
				return
			}
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			// The client went away. Unsubscribing is all that happens: the
			// turn belongs to the daemon and keeps running.
			return
		}
	}
}

func writeSSE(w http.ResponseWriter, f http.Flusher, ev WireEvent) bool {
	payload, err := ev.Encode()
	if err != nil {
		return true // skip an unencodable event rather than dropping the stream
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return false
	}
	f.Flush()
	return true
}

// pendingApprovalEvents is filled in by Task 4; it lists approvals already
// waiting on a human when a client attaches.
func (s *Server) pendingApprovalEvents(ctx context.Context, sessionID string) []WireEvent { return nil }
