package daemon

import "sync"

// subscriberBuffer is how far a client may fall behind before it starts
// missing events. A browser tab that is not reading must never block the
// turn, so a full buffer drops rather than waits.
const subscriberBuffer = 256

type sessionHub struct {
	subs    map[chan WireEvent]struct{}
	running bool
}

// Hub fans one turn's events out to every client attached to a session, and
// tracks which sessions have a turn in flight. It is the whole of what makes
// a session "a row, not a process": the turn publishes here and never holds
// a reference to any client.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionHub
}

func NewHub() *Hub { return &Hub{sessions: map[string]*sessionHub{}} }

func (h *Hub) get(sessionID string) *sessionHub {
	sh, ok := h.sessions[sessionID]
	if !ok {
		sh = &sessionHub{subs: map[chan WireEvent]struct{}{}}
		h.sessions[sessionID] = sh
	}
	return sh
}

// Subscribe attaches a client to a session. The returned function detaches it
// and closes the channel; it is safe to call more than once.
func (h *Hub) Subscribe(sessionID string) (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, subscriberBuffer)
	h.mu.Lock()
	h.get(sessionID).subs[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			sh, ok := h.sessions[sessionID]
			if !ok {
				return
			}
			if _, still := sh.subs[ch]; !still {
				return
			}
			delete(sh.subs, ch)
			// Closing under the lock is what makes Publish safe: Publish
			// holds the same lock, so it can never be mid-send on a channel
			// that is being closed.
			close(ch)
			h.gc(sessionID, sh)
		})
	}
}

// Publish delivers to every current subscriber. A subscriber whose buffer is
// full is skipped, not waited on.
func (h *Hub) Publish(sessionID string, ev WireEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	for ch := range sh.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Begin claims the session's turn slot, reporting false when a turn is
// already running. Two clients posting at once must not interleave two turns
// into one transcript.
func (h *Hub) Begin(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh := h.get(sessionID)
	if sh.running {
		return false
	}
	sh.running = true
	return true
}

func (h *Hub) End(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		return
	}
	sh.running = false
	h.gc(sessionID, sh)
}

// gc drops the bookkeeping for a session with no subscribers and no turn, so
// a long-lived daemon does not accumulate one entry per session ever opened.
// Callers hold h.mu.
func (h *Hub) gc(sessionID string, sh *sessionHub) {
	if len(sh.subs) == 0 && !sh.running {
		delete(h.sessions, sessionID)
	}
}
