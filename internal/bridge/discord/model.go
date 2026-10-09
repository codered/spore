package discord

import (
	"context"
	"log/slog"
	"strings"

	"github.com/codered/spore/internal/modelcmd"
	"github.com/codered/spore/internal/models"
	"github.com/codered/spore/internal/router"
)

// ModelChooser is /model's service as the bridge sees it. models.Service
// implements it.
type ModelChooser interface {
	View(ctx context.Context, sessionID string, fresh bool) (models.View, error)
	Set(ctx context.Context, sessionID, op, ref string) (models.View, error)
}

// modelPrefix marks a /model select's custom id: prefix, op, session.
const modelPrefix = "spore-model:"

// Discord's limits for a select menu.
const (
	maxSelectOptions = 25
	maxSelectValue   = 100
)

// modelGroups splits the selects across messages: Discord allows five rows
// per message, and there are six operations.
var modelGroups = [][]string{{"chat", "subagent", "compaction", "title"}, {"classify", "refinement"}}

func modelCustomID(sessionID, op string) string { return modelPrefix + op + ":" + sessionID }

func isModelCustomID(s string) bool { return strings.HasPrefix(s, modelPrefix) }

// decodeModelCustomID returns the session and op a select was drawn for.
func decodeModelCustomID(s string) (sessionID, op string) {
	op, sessionID, _ = strings.Cut(strings.TrimPrefix(s, modelPrefix), ":")
	return sessionID, op
}

// handleModel answers /model with the overview and a select per operation.
// Like /usage it never opens a session: outside a bound DM or thread there
// is no session, so only the daemon-wide operations are offered.
func (b *Bridge) handleModel(in Inbound) {
	if b.models == nil {
		b.say(in.ChannelID, "model selection is not available")
		return
	}
	sid, found, err := b.store.SessionForExternal(b.ctx, bridgeName, in.ChannelID)
	if err != nil {
		slog.Warn("discord /model: look up session", "err", err)
		return
	}
	if !found {
		sid = ""
	}
	v, err := b.models.View(b.ctx, sid, false)
	if err != nil {
		b.say(in.ChannelID, "could not list models: "+err.Error())
		return
	}
	for i, group := range modelGroups {
		var m Message
		if i == 0 {
			m.Content = "```\nModels per operation (-> selected, * default):\n" + modelcmd.Overview(v)
			for _, g := range v.Groups {
				if g.Error != "" {
					m.Content += "! " + g.Provider + ": " + g.Error + "\n"
				}
			}
			m.Content += "```"
			if sid == "" {
				m.Content += "\nchat and subagent are chosen per session: use /model inside a thread or DM."
			}
		}
		for _, op := range group {
			if sid == "" && !router.IsGlobalSite(op) {
				continue
			}
			if s, ok := modelSelect(v, sid, op); ok {
				m.Selects = append(m.Selects, s)
			}
		}
		if m.Content == "" && len(m.Selects) == 0 {
			continue
		}
		if _, err := b.client.Send(b.ctx, in.ChannelID, m); err != nil {
			slog.Warn("discord /model: send", "err", err)
		}
	}
}

// modelSelect is one operation's menu: the choosable options in the order
// every surface uses, capped at Discord's 25 (the selected model and the
// default come first, so they survive the cap).
func modelSelect(v models.View, sessionID, op string) (Select, bool) {
	var opts []SelectOption
	for _, o := range modelcmd.Options(v, op) {
		if !o.Choosable() || len(o.Ref) > maxSelectValue {
			continue
		}
		label := o.Ref
		if o.Selected {
			label = "-> " + label
		}
		if o.Default {
			label += " *"
		}
		opts = append(opts, SelectOption{Label: label, Value: o.Ref, Default: o.Selected})
		if len(opts) == maxSelectOptions {
			break
		}
	}
	if len(opts) == 0 {
		return Select{}, false
	}
	placeholder := op
	for _, o := range v.Ops {
		if o.Op == op {
			placeholder = op + ": -> " + o.Selected
		}
	}
	return Select{CustomID: modelCustomID(sessionID, op), Placeholder: placeholder, Options: opts}, true
}

// chooseModel applies a select. The catalog check can take seconds and
// Discord fails an interaction not acknowledged within three, so it
// acknowledges first and reports the outcome in the channel.
func (b *Bridge) chooseModel(i Interaction) {
	sessionID, op := decodeModelCustomID(i.CustomID)
	if b.models == nil || len(i.Values) != 1 {
		if err := b.client.Respond(b.ctx, i.ID, i.Token, "model selection is not available here"); err != nil {
			slog.Warn("discord /model: respond", "err", err)
		}
		return
	}
	ref := i.Values[0]
	if err := b.client.Respond(b.ctx, i.ID, i.Token, "choosing "+ref+" for "+op+"…"); err != nil {
		slog.Warn("discord /model: respond", "err", err)
	}
	v, err := b.models.Set(b.ctx, sessionID, op, ref)
	if err != nil {
		b.say(i.ChannelID, "could not choose "+ref+" for "+op+": "+err.Error())
		return
	}
	msg := strings.TrimSpace(modelcmd.Confirm(v, op))
	if router.IsGlobalSite(op) {
		msg += " — this changes " + op + " for every session, not just this thread"
	}
	b.say(i.ChannelID, msg)
}
