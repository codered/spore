package mcp

import (
	"context"
	"sync"
	"time"
)

// Supervise keeps every configured server connected for as long as ctx lives,
// and returns a function that blocks until all of its goroutines have
// stopped. Callers must call it: an unjoined supervisor outlives the daemon's
// shutdown and holds a child process with it.
//
// A server that will not start is a warning, never a fatal error. spore's own
// builtins and its web UI keep working when someone's MCP server does not.
func Supervise(ctx context.Context, h *Host) (wait func()) {
	var wg sync.WaitGroup
	for _, st := range h.servers {
		wg.Add(1)
		go func(st *serverState) {
			defer wg.Done()
			h.superviseOne(ctx, st)
		}(st)
	}
	return func() {
		wg.Wait()
		// superviseOne returns as soon as ctx is done, before it has torn
		// down the session it was watching: watch and the connect retry loop
		// both just stop selecting rather than close anything. Close is what
		// actually ends every session and kills every child, so wait must
		// call it or a cancelled Supervise leaves live connections behind.
		h.Close()
	}
}

func (h *Host) superviseOne(ctx context.Context, st *serverState) {
	backoff := h.backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		if err := h.connect(ctx, st); err != nil {
			h.log.Warn("mcp server did not start; retrying", "server", st.cfg.Name, "err", err, "in", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > h.backoffMax {
				backoff = h.backoffMax
			}
			continue
		}
		backoff = h.backoffMin
		h.log.Info("mcp server connected", "server", st.cfg.Name)

		if h.watch(ctx, st) {
			return // context cancelled: shutdown, not a drop
		}
		h.log.Warn("mcp server disconnected; redialling", "server", st.cfg.Name)
		h.markDown(st, errServerGone)
	}
}

// errServerGone is what Status reports for a server whose session ended
// without spore asking it to.
var errServerGone = errorString("the server closed the connection")

type errorString string

func (e errorString) Error() string { return string(e) }

// watch blocks until the session ends, re-listing whenever the server says
// its tool list changed. It reports true when the context was cancelled,
// meaning the caller should stop rather than redial.
func (h *Host) watch(ctx context.Context, st *serverState) (cancelled bool) {
	st.mu.RLock()
	session := st.session
	st.mu.RUnlock()
	if session == nil {
		return ctx.Err() != nil
	}

	// One goroutine turns "the session ended" into a channel receive. Wait
	// returns when the peer goes away, which for a stdio server is when the
	// process exits.
	gone := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(gone)
	}()

	for {
		select {
		case <-ctx.Done():
			return true
		case <-gone:
			return false
		case <-st.changed:
			if err := h.relist(ctx, st); err != nil {
				h.log.Warn("mcp re-list failed", "server", st.cfg.Name, "err", err)
			}
		}
	}
}
