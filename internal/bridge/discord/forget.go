package discord

import (
	"context"
	"fmt"
	"time"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/store"
)

var _ daemon.Cleaner = (*Bridge)(nil)

// ChannelKind is what a bound channel is, which decides what deleting its
// session can do there.
type ChannelKind int

const (
	ChannelOther ChannelKind = iota
	// ChannelThread is a guild thread spore opened for a session: deleting
	// it removes every message in it, the user's included.
	ChannelThread
	// ChannelDM is a direct-message channel. Discord lets a bot delete only
	// its own messages there, never the user's.
	ChannelDM
)

// dmSpanSlack widens a DM session's time span at the end: the last reply is
// streamed by edits and can settle a little after the session's last write.
const dmSpanSlack = 2 * time.Minute

// ForgetSessions implements daemon.Cleaner: it deletes Discord's copy of
// sessions that are about to be deleted, and says what it did. It must run
// before the rows go, while the bindings still name the channels.
func (b *Bridge) ForgetSessions(ctx context.Context, sessions []store.Session) []string {
	byID := map[string]store.Session{}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		byID[s.ID] = s
		ids = append(ids, s.ID)
	}
	bindings, err := b.store.BindingsForSessions(ctx, bridgeName, ids)
	if err != nil {
		return []string{"Discord was not touched: " + err.Error()}
	}
	var notes []string
	for _, bd := range bindings {
		kind, err := b.client.ChannelKind(ctx, bd.ExternalID)
		if err != nil {
			notes = append(notes, fmt.Sprintf("could not look up Discord channel %s: %v", bd.ExternalID, err))
			continue
		}
		switch kind {
		case ChannelThread:
			if err := b.client.DeleteChannel(ctx, bd.ExternalID); err != nil {
				notes = append(notes, fmt.Sprintf("could not delete the Discord thread %s (the bot needs Manage Threads): %v", bd.ExternalID, err))
				continue
			}
			notes = append(notes, fmt.Sprintf("deleted the Discord thread %s", bd.ExternalID))
		case ChannelDM:
			notes = append(notes, b.forgetDM(ctx, bd.ExternalID, byID[bd.SessionID]))
		default:
			notes = append(notes, fmt.Sprintf("left Discord channel %s alone: it is not a thread spore opened or a DM", bd.ExternalID))
		}
	}
	return notes
}

// forgetDM deletes the bot's own messages in the DM that fall within the
// session's life. The user's messages cannot be deleted by a bot.
func (b *Bridge) forgetDM(ctx context.Context, channelID string, sess store.Session) string {
	after := sess.CreatedAt.Add(-time.Second)
	before := sess.UpdatedAt.Add(dmSpanSlack)
	msgs, err := b.client.OwnMessages(ctx, channelID, after, before)
	if err != nil {
		return fmt.Sprintf("could not read spore's messages in your DM: %v", err)
	}
	deleted := 0
	for _, id := range msgs {
		if err := b.client.DeleteMessage(ctx, channelID, id); err == nil {
			deleted++
		}
	}
	note := fmt.Sprintf("deleted %d of spore's messages in your DM; yours stay, because Discord does not let a bot delete them", deleted)
	if deleted < len(msgs) {
		note += fmt.Sprintf(" (%d could not be deleted)", len(msgs)-deleted)
	}
	return note
}
