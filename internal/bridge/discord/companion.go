package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/companion"
	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/store"
)

// ProposalAnswerer acts on a companion proposal's button. The daemon is one.
type ProposalAnswerer interface {
	AnswerProposal(ctx context.Context, outreachID int64, choice string) (string, error)
}

// CompanionCommander runs a /companion line. The daemon is one.
type CompanionCommander interface {
	CompanionCommand(ctx context.Context, args []string) (string, error)
}

const companionPrefix = "companion|"

// companionCustomID packs a proposal answer into a button's custom id.
func companionCustomID(outreachID int64, choice string) string {
	return companionPrefix + strconv.FormatInt(outreachID, 10) + "|" + choice
}

func isCompanionCustomID(s string) bool { return strings.HasPrefix(s, companionPrefix) }

// decodeCompanionCustomID parses a proposal button's custom id. It arrives
// from the network, so anything off-shape is an error, never a default.
func decodeCompanionCustomID(s string) (int64, string, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 3 || parts[0]+"|" != companionPrefix {
		return 0, "", fmt.Errorf("not a companion component id: %q", s)
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("bad outreach id in %q: %w", s, err)
	}
	switch parts[2] {
	case companion.ChoiceYes, companion.ChoiceNotNow, companion.ChoiceNever:
	default:
		return 0, "", fmt.Errorf("bad choice in %q", s)
	}
	return id, parts[2], nil
}

// proposalButtons are a proposal's three answers.
func proposalButtons(outreachID int64) []Button {
	return []Button{
		{CustomID: companionCustomID(outreachID, companion.ChoiceYes), Label: "✅ Yes, watch it"},
		{CustomID: companionCustomID(outreachID, companion.ChoiceNotNow), Label: "⏸ Not now"},
		{CustomID: companionCustomID(outreachID, companion.ChoiceNever), Label: "✖ Never", Danger: true},
	}
}

// answerProposal handles a proposal button press: answer it, then say how it
// went, privately, inside Discord's three-second window.
func (b *Bridge) answerProposal(i Interaction) {
	id, choice, err := decodeCompanionCustomID(i.CustomID)
	if err != nil {
		return // not ours, or malformed: silence
	}
	msg := "the companion is not enabled"
	if b.proposals != nil {
		if msg, err = b.proposals.AnswerProposal(b.ctx, id, choice); err != nil {
			msg = "could not record that: " + err.Error()
		}
	}
	if err := b.client.Respond(b.ctx, i.ID, i.Token, msg); err != nil {
		slog.Warn("discord companion response", "err", err)
	}
}

// CompanionChannel delivers the companion's outreach on Discord.
type CompanionChannel struct {
	b   *Bridge
	cfg config.CompanionDiscordConfig
}

// CompanionChannel returns the companion's Discord channel.
func (b *Bridge) CompanionChannel(cfg config.CompanionDiscordConfig) *CompanionChannel {
	return &CompanionChannel{b: b, cfg: cfg}
}

var _ companion.Channel = (*CompanionChannel)(nil)

// Name implements companion.Channel.
func (c *CompanionChannel) Name() string { return store.OutreachChannelDiscord }

// Send implements companion.Channel: it posts o where the companion session
// is bound, opening that place first if this is the session's first
// message. A proposal carries its three buttons.
func (c *CompanionChannel) Send(ctx context.Context, sessionID string, o store.Outreach) error {
	channelID, err := c.channelFor(ctx, sessionID)
	if err != nil {
		return err
	}
	m := Message{Content: o.Text, Quiet: true}
	if o.Kind == store.OutreachProposal {
		m.Buttons = proposalButtons(o.ID)
	}
	_, err = c.b.client.Send(ctx, channelID, m)
	return err
}

// channelFor is the session's bound channel, or a new one: a thread opened
// under the companion channel, or the first companion user's DM. It holds
// resolveMu so a reply racing the first outreach cannot bind twice.
func (c *CompanionChannel) channelFor(ctx context.Context, sessionID string) (string, error) {
	c.b.resolveMu.Lock()
	defer c.b.resolveMu.Unlock()
	if ch, ok := c.b.boundChannel(ctx, sessionID); ok {
		return ch, nil
	}
	if len(c.cfg.UserIDs) == 0 {
		return "", fmt.Errorf("companion.discord has no user to reach")
	}
	var channelID string
	switch c.cfg.Mode {
	case config.CompanionDiscordDM:
		dm, err := c.b.client.UserChannel(ctx, c.cfg.UserIDs[0])
		if err != nil {
			return "", err
		}
		channelID = dm
	default:
		anchor, err := c.b.client.Send(ctx, c.cfg.ChannelID, Message{Content: "spore: things I noticed and want to check with you", Quiet: true})
		if err != nil {
			return "", err
		}
		thread, err := c.b.client.CreateThread(ctx, c.cfg.ChannelID, anchor, "spore")
		if err != nil {
			return "", err
		}
		channelID = thread
	}
	if err := c.b.store.BindExternal(ctx, bridgeName, channelID, sessionID); err != nil {
		return "", err
	}
	return channelID, nil
}
