// Package discord bridges spore to one Discord bot. It is a CLIENT of the
// daemon — it subscribes to the hub, starts turns through the server, and
// answers approvals through the broker and guard — so the session-ownership
// check that stops a remote session answering a local one's approval applies
// to it unchanged. It is deliberately not a policy.Approver.
//
// client.go is the only file in this package, and in the whole codebase,
// that imports discordgo. Every other file in this package — and every
// later task that consumes it — is written against the Client interface
// below and tested against fakeClient, so nothing here ever needs a live
// Discord connection to test.
package discord

import (
	"context"
	"fmt"

	"github.com/bwmarrin/discordgo"
)

// Inbound is one message the gateway delivered, flattened to what the bridge
// needs. Every field is a Discord snowflake or plain text; nothing here is a
// discordgo type, so the rest of the package never sees the library.
type Inbound struct {
	MessageID string
	UserID    string
	Bot       bool
	GuildID   string // "" for a direct message
	ChannelID string
	ParentID  string // set when ChannelID is a thread
	Content   string
}

// Interaction is one component (button) press.
type Interaction struct {
	ID        string
	Token     string
	UserID    string
	GuildID   string
	ChannelID string
	ParentID  string
	CustomID  string // the button's identity; see approve.go
}

// Button is one message component the bridge asks the client to render.
type Button struct {
	CustomID string
	Label    string
	Danger   bool
}

// Embed is a boxed block beside a message's text. Tool calls render as embeds
// so a long transcript stays skimmable — the prose and the machinery are
// visually separate.
type Embed struct {
	Title       string
	Description string
	// Error tints the embed red. A failed tool call must be obvious at a
	// glance on a phone.
	Error bool
}

// Message is everything the bridge can put on screen at once.
type Message struct {
	Content string
	Embeds  []Embed
	Buttons []Button
}

// embedDescriptionLimit is Discord's documented per-embed description cap.
// Exceeding it is a hard API error, not a truncation on Discord's side, so
// the adapter must enforce it before the request goes out.
const embedDescriptionLimit = 4096

// threadNameLimit is Discord's documented thread-name cap.
const threadNameLimit = 100

// buttonsPerRow and maxRows are Discord's component layout limits: at most
// five interactive components per action row, and at most five rows per
// message.
const (
	buttonsPerRow = 5
	maxRows       = 5
)

// embedColorDefault and embedColorError are the two colours the bridge ever
// sends. Blurple matches Discord's own brand accent so a normal embed reads
// as "part of the app"; the red is chosen to be unambiguous against both
// Discord's light and dark themes.
const (
	embedColorDefault = 0x5865F2
	embedColorError   = 0xCC3333
)

// Client is everything the bridge needs from Discord, expressed without any
// discordgo type. Every method above admission, rendering, approvals, and
// routing is written against this interface; production wires gatewayClient,
// tests wire fakeClient (fake_test.go), and neither the bridge nor its tests
// ever import discordgo.
type Client interface {
	// Open connects and starts delivering to the handlers, blocking until
	// the connection is established. It returns when connected, not when
	// closed.
	Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error
	Close() error
	Send(ctx context.Context, channelID string, m Message) (messageID string, err error)
	Edit(ctx context.Context, channelID, messageID string, m Message) error
	CreateThread(ctx context.Context, channelID, messageID, name string) (threadID string, err error)
	// Respond acknowledges an interaction with an ephemeral message. Discord
	// requires an acknowledgement within three seconds or the button shows
	// as failed, so this is called before any slow work.
	Respond(ctx context.Context, interactionID, token, content string) error
}

// gatewayClient is the real Client, over discordgo. It is the only place in
// spore that knows the library exists; everything above it is testable
// offline against a fake.
type gatewayClient struct {
	sess *discordgo.Session
}

// NewGatewayClient dials nothing yet; Open does that. It is called during
// daemon startup, where a network round trip would make startup fail on a
// flaky link rather than on a bad token — discordgo.New only builds an
// unopened session, so that stays true here.
func NewGatewayClient(token string) (Client, error) {
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, fmt.Errorf("build discord session: %w", err)
	}
	// MessageContent is a privileged intent and must also be enabled in the
	// bot's settings in Discord's developer portal. Without it, message text
	// arrives empty in guilds and the bridge silently does nothing.
	s.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages |
		discordgo.IntentMessageContent
	return &gatewayClient{sess: s}, nil
}

// Open registers the two gateway callbacks the bridge needs, translates
// each event to this package's own types, and dials. discordgo delivers
// events on its own goroutines for the life of the connection, so onMessage
// and onInteraction must be safe to call concurrently — that obligation is
// documented on Client, not enforced here.
func (c *gatewayClient) Open(ctx context.Context, onMessage func(Inbound), onInteraction func(Interaction)) error {
	c.sess.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		if onMessage == nil || m.Message == nil {
			return
		}
		onMessage(c.inboundFrom(m.Message))
	})
	c.sess.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if onInteraction == nil || i.Interaction == nil {
			return
		}
		onInteraction(c.interactionFrom(i.Interaction))
	})
	if err := c.sess.Open(); err != nil {
		return fmt.Errorf("open discord session: %w", err)
	}
	return nil
}

// inboundFrom converts a discordgo message to Inbound, resolving ParentID by
// looking the channel up in the local state cache first and falling back to
// a REST call. Both can fail — the cache may be empty this early, and the
// call may hit the network — and neither is fatal: on failure ParentID is
// left empty and the message is delivered anyway, which is the safe
// direction (a thread whose parent can't be resolved is judged on its own
// channel id by admission, rather than wrongly admitted as some other
// channel's thread).
func (c *gatewayClient) inboundFrom(m *discordgo.Message) Inbound {
	in := Inbound{
		MessageID: m.ID,
		GuildID:   m.GuildID,
		ChannelID: m.ChannelID,
		Content:   m.Content,
	}
	if m.Author != nil {
		in.UserID = m.Author.ID
		in.Bot = m.Author.Bot
	}
	ch, err := c.sess.State.Channel(m.ChannelID)
	if err != nil || ch == nil {
		ch, err = c.sess.Channel(m.ChannelID)
	}
	if err == nil && ch != nil && ch.IsThread() {
		in.ParentID = ch.ParentID
	}
	return in
}

// interactionFrom converts a discordgo interaction to Interaction. UserID
// comes from Member.User in a guild and from User in a DM — discordgo fills
// exactly one of the two depending on where the interaction happened.
// ParentID is resolved the same way as inboundFrom's: state cache first,
// REST fallback second, empty (not fatal) if both fail.
func (c *gatewayClient) interactionFrom(i *discordgo.Interaction) Interaction {
	out := Interaction{
		ID:        i.ID,
		Token:     i.Token,
		GuildID:   i.GuildID,
		ChannelID: i.ChannelID,
	}
	if i.Member != nil && i.Member.User != nil {
		out.UserID = i.Member.User.ID
	} else if i.User != nil {
		out.UserID = i.User.ID
	}
	if i.Type == discordgo.InteractionMessageComponent {
		out.CustomID = i.MessageComponentData().CustomID
	}
	ch, err := c.sess.State.Channel(i.ChannelID)
	if err != nil || ch == nil {
		ch, err = c.sess.Channel(i.ChannelID)
	}
	if err == nil && ch != nil && ch.IsThread() {
		out.ParentID = ch.ParentID
	}
	return out
}

// Close disconnects from the gateway. Safe to call even if Open was never
// called successfully.
func (c *gatewayClient) Close() error {
	return c.sess.Close()
}

// Send posts a new message and returns its id, which callers hold onto for
// later Edit or CreateThread calls.
func (c *gatewayClient) Send(ctx context.Context, channelID string, m Message) (string, error) {
	msg, err := c.sess.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    m.Content,
		Embeds:     embedsFor(m.Embeds),
		Components: componentsFor(m.Buttons),
	}, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("send discord message: %w", err)
	}
	return msg.ID, nil
}

// Edit replaces a message's content, embeds, and buttons in place. discordgo
// distinguishes "field omitted" from "field cleared" with pointers, so a nil
// component list here would leave existing buttons untouched rather than
// remove them — the bridge always wants the new Message to fully describe
// the message's state, so all three fields are always set.
func (c *gatewayClient) Edit(ctx context.Context, channelID, messageID string, m Message) error {
	content := m.Content
	embeds := embedsFor(m.Embeds)
	comps := componentsFor(m.Buttons)
	_, err := c.sess.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    channelID,
		ID:         messageID,
		Content:    &content,
		Embeds:     &embeds,
		Components: &comps,
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("edit discord message: %w", err)
	}
	return nil
}

// CreateThread starts a public thread off an existing message. Discord
// rejects thread names over 100 characters, so the name is truncated here
// rather than surfacing an API error to the caller.
func (c *gatewayClient) CreateThread(ctx context.Context, channelID, messageID, name string) (string, error) {
	ch, err := c.sess.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{
		Name:                truncate(name, threadNameLimit),
		AutoArchiveDuration: 1440, // 24h; the shortest option Discord considers "won't vanish mid-conversation"
		Type:                discordgo.ChannelTypeGuildPublicThread,
	}, discordgo.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("create discord thread: %w", err)
	}
	return ch.ID, nil
}

// Respond acknowledges an interaction with an ephemeral message, visible
// only to the user who pressed the button. discordgo's InteractionRespond
// reads only ID and Token off the interaction, so those two fields are all
// this needs from the caller.
func (c *gatewayClient) Respond(ctx context.Context, interactionID, token, content string) error {
	err := c.sess.InteractionRespond(&discordgo.Interaction{ID: interactionID, Token: token}, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}, discordgo.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("respond to discord interaction: %w", err)
	}
	return nil
}

// embedsFor translates the bridge's Embed to discordgo's, truncating
// Description to Discord's documented limit and choosing colour from Error.
func embedsFor(embeds []Embed) []*discordgo.MessageEmbed {
	if len(embeds) == 0 {
		return nil
	}
	out := make([]*discordgo.MessageEmbed, len(embeds))
	for i, e := range embeds {
		color := embedColorDefault
		if e.Error {
			color = embedColorError
		}
		out[i] = &discordgo.MessageEmbed{
			Title:       e.Title,
			Description: truncate(e.Description, embedDescriptionLimit),
			Color:       color,
		}
	}
	return out
}

// componentsFor translates the bridge's Buttons into discordgo action rows,
// splitting into rows of five and capping at five rows (25 buttons) per
// Discord's layout limits. Nothing in this plan produces more than a
// handful of buttons, so silently dropping any excess is acceptable.
func componentsFor(buttons []Button) []discordgo.MessageComponent {
	if len(buttons) == 0 {
		return nil
	}
	var rows []discordgo.MessageComponent
	for i := 0; i < len(buttons) && len(rows) < maxRows; i += buttonsPerRow {
		end := i + buttonsPerRow
		if end > len(buttons) {
			end = len(buttons)
		}
		var comps []discordgo.MessageComponent
		for _, b := range buttons[i:end] {
			style := discordgo.SecondaryButton
			if b.Danger {
				style = discordgo.DangerButton
			}
			comps = append(comps, discordgo.Button{
				CustomID: b.CustomID,
				Label:    b.Label,
				Style:    style,
			})
		}
		rows = append(rows, discordgo.ActionsRow{Components: comps})
	}
	return rows
}

// truncate cuts s to at most max runes, leaving it unchanged if it already
// fits. Discord's documented limits are character counts, not bytes, so
// truncation is done on runes to avoid splitting multi-byte characters.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}
