package discord

import (
	"github.com/codered/spore/internal/config"
)

// Admitter decides who may reach the agent through Discord. It is the whole
// trust boundary of the bridge, which is why it is one small pure type with
// no I/O: it can be read in full and table-tested exhaustively.
//
// Guild membership is the outer boundary — a Discord bot exists only in
// servers it has been invited to — and the user allowlist is the inner one.
// Both apply to every surface. Anything not admitted is dropped without a
// reply: answering would confirm the bot exists to whoever probed it.
type Admitter struct {
	guildID  string
	channels map[string]struct{}
	users    map[string]struct{}
	allowDMs bool
}

// NewAdmitter creates an Admitter that enforces the given configuration.
func NewAdmitter(cfg config.DiscordConfig) Admitter {
	a := Admitter{
		guildID:  cfg.GuildID,
		channels: make(map[string]struct{}, len(cfg.ChannelIDs)),
		users:    make(map[string]struct{}, len(cfg.UserIDs)),
		allowDMs: cfg.AllowDMs,
	}
	for _, c := range cfg.ChannelIDs {
		a.channels[c] = struct{}{}
	}
	for _, u := range cfg.UserIDs {
		a.users[u] = struct{}{}
	}
	return a
}

// AdmitMessage reports whether an inbound message may start or continue a
// session.
func (a Admitter) AdmitMessage(in Inbound) bool {
	// A bot's own messages come back over the gateway. Replying to them is
	// how a bridge talks to itself until the rate limiter stops it.
	if in.Bot {
		return false
	}
	return a.admit(in.UserID, in.GuildID, in.ChannelID, in.ParentID)
}

// AdmitInteraction reports whether a button press may be acted on. It applies
// exactly the rules AdmitMessage applies: a press is a second entrance to the
// same house, and an approval answered by someone who could not have sent the
// prompt would be the worst kind of hole.
func (a Admitter) AdmitInteraction(i Interaction) bool {
	return a.admit(i.UserID, i.GuildID, i.ChannelID, i.ParentID)
}

func (a Admitter) admit(userID, guildID, channelID, parentID string) bool {
	if _, ok := a.users[userID]; !ok {
		return false
	}
	if guildID == "" {
		return a.allowDMs
	}
	if a.guildID == "" || guildID != a.guildID {
		return false
	}
	// A thread is admitted on its parent, so threads the bridge opens itself
	// need no entry in the config.
	key := channelID
	if parentID != "" {
		key = parentID
	}
	_, ok := a.channels[key]
	return ok
}
