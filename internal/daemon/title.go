package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
	"github.com/codered/spore/internal/title"
)

// titleTimeout bounds the naming call. A local model can be slow; a session
// that stays "chat" a little longer costs nothing.
const titleTimeout = 90 * time.Second

// Titler names a conversation from its opening message.
type Titler interface {
	Title(ctx context.Context, message string) (string, error)
}

// wantsTitle reports whether a session should be named: a conversation a
// person started (a job is named by its prompt, a sub-agent by its task)
// that still carries the placeholder it was created with.
func wantsTitle(sess store.Session) bool {
	if sess.ParentID != "" || !title.Placeholder(sess.Title) {
		return false
	}
	switch sess.Source {
	case store.SourceChat, store.SourceDiscord, store.SourceUnknown:
		return true
	}
	return false
}

// nameAfter wraps a turn's end hook so the session is named once the turn
// ends. Naming waits for the reply rather than racing it: on a local model
// the two calls would share one GPU and the reply would arrive later.
func (s *Server) nameAfter(sess store.Session, text string, then func(WireEvent)) func(WireEvent) {
	return func(end WireEvent) {
		if then != nil {
			then(end)
		}
		go s.nameSession(sess, text)
	}
}

// nameSession asks the titler for a name, falls back to the first line of
// the opening message, and announces the name on the feed.
func (s *Server) nameSession(sess store.Session, text string) {
	if _, busy := s.naming.LoadOrStore(sess.ID, true); busy {
		return
	}
	defer s.naming.Delete(sess.ID)
	defer func() {
		if r := recover(); r != nil {
			slog.Error("panic naming session", "session", sess.ID, "panic", r)
		}
	}()

	ctx, cancel := context.WithTimeout(s.base, titleTimeout)
	defer cancel()
	opening := s.openingMessage(ctx, sess.ID, text)
	name := ""
	if s.titler != nil {
		var err error
		if name, err = s.titler.Title(ctx, opening); err != nil {
			slog.Warn("naming session failed; using its first line", "session", sess.ID, "err", err)
			name = ""
		}
	}
	if name == "" {
		name = title.Fallback(opening)
	}
	if name == "" {
		return
	}
	changed, err := s.store.RenameSessionFrom(ctx, sess.ID, sess.Title, name)
	if err != nil {
		slog.Warn("could not store the session name", "session", sess.ID, "err", err)
		return
	}
	if !changed {
		return // renamed meanwhile, or deleted
	}
	// Every field is sent: clients replace the session's details with the
	// event's, so a missing workspace would blank theirs.
	s.hub.Publish(sess.ID, WireEvent{
		Type: WireSession, Title: name, Workspace: sess.Workspace,
		Source: sess.Source, ParentID: sess.ParentID,
	})
}

// openingMessage is the first thing the person said in the session, which is
// what names it. A session renamed late (one created before naming existed)
// is still named for how it began, not for its latest message.
func (s *Server) openingMessage(ctx context.Context, sessionID, fallback string) string {
	msgs, err := s.store.Messages(ctx, sessionID)
	if err != nil {
		return fallback
	}
	for _, m := range msgs {
		if m.Role != string(provider.RoleUser) {
			continue
		}
		var blocks []provider.Block
		if json.Unmarshal(m.BlocksJSON, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == provider.BlockText && strings.TrimSpace(b.Text) != "" {
				return b.Text
			}
		}
	}
	return fallback
}
