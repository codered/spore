package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/router"
	"github.com/codered/spore/internal/store"
)

const (
	// ScopeSession ops are chosen per session and stored on it.
	ScopeSession = "session"
	// ScopeGlobal ops are chosen for the whole daemon and stored in the
	// managed routing block.
	ScopeGlobal = "global"
)

// ErrInvalid marks a choice /model refuses: an unknown operation, a model
// that is not available, or a per-session op with no session.
var ErrInvalid = errors.New("invalid model choice")

// Op is one operation's state as every surface shows it: Selected gets
// "->", Default gets "*".
type Op struct {
	Op       string `json:"op"`
	Scope    string `json:"scope"`
	Selected string `json:"selected"`
	Default  string `json:"default"`
}

// View is GET /api/models: every op, and what can be chosen.
type View struct {
	Ops    []Op    `json:"ops"`
	Groups []Group `json:"groups"`
}

// Service reads and changes the per-operation choice.
type Service struct {
	Store      *store.Store
	Router     *router.Router
	Catalog    *Catalog
	ConfigPath string

	// mu serialises daemon-wide choices so the file and the router always agree.
	mu sync.Mutex
}

// View reports every op for a session ("" for none: chat and subagent then
// show their defaults).
func (s *Service) View(ctx context.Context, sessionID string, fresh bool) (View, error) {
	var sess store.Session
	if sessionID != "" {
		got, ok, err := s.Store.Session(ctx, sessionID)
		if err != nil {
			return View{}, err
		}
		if !ok {
			return View{}, fmt.Errorf("%w: no session %s", ErrInvalid, sessionID)
		}
		sess = got
	}
	v := View{Groups: s.Catalog.List(ctx, fresh)}
	chatDefault := s.Router.Model(router.SiteChat)
	chat := chatDefault
	if sess.ChatModel != "" {
		chat = sess.ChatModel
	}
	for _, site := range router.Sites {
		op := Op{Op: site}
		switch site {
		case router.SiteChat:
			op.Scope, op.Default, op.Selected = ScopeSession, chatDefault, chat
		case router.SiteSubagent:
			// A sub-agent inherits the session's own model unless told
			// otherwise, so that is its default.
			op.Scope, op.Default, op.Selected = ScopeSession, chat, chat
			if sess.SubagentModel != "" {
				op.Selected = sess.SubagentModel
			}
		default:
			op.Scope, op.Default, op.Selected = ScopeGlobal, s.Router.Default(site), s.Router.Model(site)
		}
		v.Ops = append(v.Ops, op)
	}
	return v, nil
}

// Set makes ref the model for op. Choosing the op's default clears the
// choice, so it follows the config (or, for subagent, the chat model) again.
// Any other ref must be in a fresh catalog listing.
func (s *Service) Set(ctx context.Context, sessionID, op, ref string) (View, error) {
	if !router.ValidSite(op) {
		return View{}, fmt.Errorf("%w: unknown operation %q", ErrInvalid, op)
	}
	global := router.IsGlobalSite(op)
	if !global && sessionID == "" {
		return View{}, fmt.Errorf("%w: %s is chosen per session", ErrInvalid, op)
	}
	cur, err := s.View(ctx, sessionID, false)
	if err != nil {
		return View{}, err
	}
	for _, o := range cur.Ops {
		if o.Op == op && o.Default == ref {
			ref = ""
		}
	}
	if ref != "" && !s.Catalog.Has(ctx, ref) {
		return View{}, fmt.Errorf("%w: %s is not available for %s", ErrInvalid, ref, op)
	}
	if global {
		s.mu.Lock()
		err := config.SetRoutingOverride(s.ConfigPath, op, ref)
		if err == nil {
			err = s.Router.SetOverride(op, ref)
		}
		s.mu.Unlock()
		if err != nil {
			return View{}, err
		}
	} else if err := s.Store.SetSessionModel(ctx, sessionID, op, ref); err != nil {
		return View{}, err
	}
	return s.View(ctx, sessionID, false)
}

// ChildModel picks the model for a sub-agent launched from parentID: the
// requested one if the launching agent named one, else the parent's
// sub-agent choice, else the parent's own model.
func (s *Service) ChildModel(ctx context.Context, parentID, requested string) (string, error) {
	if requested != "" {
		if !s.Catalog.Has(ctx, requested) {
			avail := s.Catalog.Refs(ctx)
			if len(avail) == 0 {
				return "", fmt.Errorf("model %q is not available, and no provider is listing any models right now", requested)
			}
			return "", fmt.Errorf("model %q is not available; choose one of: %s", requested, strings.Join(avail, ", "))
		}
		return requested, nil
	}
	sess, ok, err := s.Store.Session(ctx, parentID)
	if err != nil {
		return "", err
	}
	if ok && sess.SubagentModel != "" {
		return sess.SubagentModel, nil
	}
	if ok && sess.ChatModel != "" {
		return sess.ChatModel, nil
	}
	return s.Router.Model(router.SiteChat), nil
}
