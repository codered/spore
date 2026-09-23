package daemon

import (
	"context"
	"sync"
)

// subscriberBuffer is how far a client may fall behind before it starts
// missing events. A browser tab that is not reading must never block the
// turn, so a full buffer drops rather than waits.
const subscriberBuffer = 256

// globalBuffer is how far a global subscriber may fall behind before it is
// disconnected. A global reader tracks state -- which sessions are working
// -- so a skipped event would leave it wrong with no way to notice. Closing
// the stream forces it down its reconnect-and-resync path instead.
const globalBuffer = 1024

type sessionHub struct {
	subs    map[chan WireEvent]struct{}
	running bool
	// cancel stops the running turn. It is set by startTurn once the turn's
	// context exists and cleared by End; a slot claimed by /clear or
	// /compact never has one.
	cancel context.CancelCauseFunc
	// queued is work waiting for the slot. End hands the slot straight to
	// the first of it, so nothing a client posts can slip in between.
	queued []func()
}

// Hub fans one turn's events out to every client attached to a session, and
// tracks which sessions have a turn in flight. It is the whole of what makes
// a session "a row, not a process": the turn publishes here and never holds
// a reference to any client.
type Hub struct {
	mu       sync.Mutex
	sessions map[string]*sessionHub
	global   map[chan WireEvent]struct{}
}

func NewHub() *Hub {
	return &Hub{sessions: map[string]*sessionHub{}, global: map[chan WireEvent]struct{}{}}
}

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

// SubscribeAll attaches a client to every session. Events arrive tagged with
// their session. The returned function detaches it; it is safe to call more
// than once, and safe after the hub has closed the channel on overflow.
func (h *Hub) SubscribeAll() (<-chan WireEvent, func()) {
	ch := make(chan WireEvent, globalBuffer)
	h.mu.Lock()
	h.global[ch] = struct{}{}
	h.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if _, still := h.global[ch]; still {
				delete(h.global, ch)
				close(ch)
			}
		})
	}
}

// Publish delivers to every current subscriber of the session, and to every
// global subscriber tagged with the session. A per-session subscriber whose
// buffer is full is skipped; a global one is disconnected (see globalBuffer).
func (h *Hub) Publish(sessionID string, ev WireEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sh, ok := h.sessions[sessionID]; ok {
		for ch := range sh.subs {
			select {
			case ch <- ev:
			default:
			}
		}
	}
	if len(h.global) == 0 {
		return
	}
	tagged := ev
	tagged.Session = sessionID
	for ch := range h.global {
		select {
		case ch <- tagged:
		default:
			// Closing under the lock is safe for the same reason it is in
			// Subscribe's detach: nothing can be mid-send on this channel.
			delete(h.global, ch)
			close(ch)
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

// End releases the session's turn slot. When work is queued for the slot it
// is handed over instead: the session stays running and the first queued
// function runs on its own goroutine, holding the slot.
func (h *Hub) End(sessionID string) {
	h.mu.Lock()
	sh, ok := h.sessions[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	sh.cancel = nil
	if len(sh.queued) > 0 {
		next := sh.queued[0]
		sh.queued = sh.queued[1:]
		h.mu.Unlock()
		go next()
		return
	}
	sh.running = false
	h.gc(sessionID, sh)
	h.mu.Unlock()
}

// Enqueue runs fn holding the session's turn slot: now, on the caller's
// goroutine, when the slot is free, or when the running turn ends. fn owns
// the slot and must release it with End (startTurn's pump does), including
// when it decides to do nothing.
func (h *Hub) Enqueue(sessionID string, fn func()) {
	h.mu.Lock()
	sh := h.get(sessionID)
	if sh.running {
		sh.queued = append(sh.queued, fn)
		h.mu.Unlock()
		return
	}
	sh.running = true
	h.mu.Unlock()
	fn()
}

// Running reports whether a turn is in flight for the session.
func (h *Hub) Running(sessionID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	return ok && sh.running
}

// SetCancel records how to stop the session's running turn. It does nothing
// when no turn is running.
func (h *Hub) SetCancel(sessionID string, cancel context.CancelCauseFunc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	sh, ok := h.sessions[sessionID]
	if !ok || !sh.running {
		return
	}
	sh.cancel = cancel
}

// Stop cancels the session's running turn with cause, reporting whether
// there was one to cancel. The cancel runs outside the lock: it wakes the
// turn, which will call End.
func (h *Hub) Stop(sessionID string, cause error) bool {
	h.mu.Lock()
	var cancel context.CancelCauseFunc
	if sh, ok := h.sessions[sessionID]; ok && sh.running {
		cancel = sh.cancel
	}
	h.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel(cause)
	return true
}

// gc drops the bookkeeping for a session with no subscribers and no turn, so
// a long-lived daemon does not accumulate one entry per session ever opened.
// Callers hold h.mu.
func (h *Hub) gc(sessionID string, sh *sessionHub) {
	if len(sh.subs) == 0 && !sh.running {
		delete(h.sessions, sessionID)
	}
}
