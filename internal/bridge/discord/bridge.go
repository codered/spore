package discord

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/codered/spore/internal/config"
	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/store"
)

// bridgeName is the namespace used for every store binding and dedupe row.
// Every SessionForExternal/BindExternal/MarkSeen call in this file uses it,
// so a second bridge sharing the same store (a future Telegram bridge, say)
// cannot collide with Discord's bindings even if the external ids happen to
// overlap.
const bridgeName = "discord"

// Turns is what the bridge needs from the daemon. It is an interface rather
// than *daemon.Server so the bridge's tests need no HTTP server, and so the
// dependency is legible: the bridge starts turns and watches events, and can
// do nothing else to the daemon.
type Turns interface {
	StartTurn(sessionID, text, client string, profile policy.Profile) error
	Subscribe(sessionID string) (<-chan daemon.WireEvent, func())
}

// Options are the bridge's collaborators. Guard may be nil — a bridge built
// without one still answers approvals through the broker's live-waiter path,
// it just cannot recover a suspension whose turn is gone (see answer.go).
type Options struct {
	Cfg    config.DiscordConfig
	Client Client
	Turns  Turns
	Store  *store.Store
	Broker *daemon.Broker
	Guard  *policy.Guard
	// Throttle overrides the render throttle. Zero means defaultThrottle
	// (New substitutes it); a negative value means "flush on every event"
	// and is what tests pass so they never wait on a clock.
	Throttle time.Duration
}

// Bridge connects one Discord bot to spore. It owns no agent machinery: it
// resolves a Discord conversation to a session, asks the daemon to run a
// turn, and renders what comes back. Everything that decides whether a call
// may run stays in the policy engine, and everything that decides whether a
// person may be here stays in the Admitter.
type Bridge struct {
	cfg      config.DiscordConfig
	admit    Admitter
	client   Client
	turns    Turns
	store    *store.Store
	answer   *answerer
	throttle time.Duration

	// ctx bounds the render goroutines. It is the BRIDGE's context, never a
	// message's: a turn outlives the message that started it, and so must
	// the goroutine rendering it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New validates the required collaborators and wires the Admitter and
// answerer. It does nothing that touches the network — that is Start's job
// — so a bad Options value fails before any goroutine exists to clean up.
func New(o Options) (*Bridge, error) {
	if o.Client == nil {
		return nil, errors.New("discord bridge: Client is required")
	}
	if o.Turns == nil {
		return nil, errors.New("discord bridge: Turns is required")
	}
	if o.Store == nil {
		return nil, errors.New("discord bridge: Store is required")
	}
	throttle := o.Throttle
	if throttle == 0 {
		throttle = defaultThrottle
	}
	return &Bridge{
		cfg:      o.Cfg,
		admit:    NewAdmitter(o.Cfg),
		client:   o.Client,
		turns:    o.Turns,
		store:    o.Store,
		answer:   newAnswerer(o.Broker, o.Guard),
		throttle: throttle,
	}, nil
}

// Start opens the gateway and returns. Events arrive on the client's
// goroutines from then on. ctx is kept (as a cancellable child) for the
// lifetime of every render goroutine the bridge spawns afterward — never a
// per-message context, since a turn outlives the message that started it.
func (b *Bridge) Start(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)
	return b.client.Open(b.ctx, b.handleMessage, b.handleInteraction)
}

// Close stops accepting new work and waits for every render goroutine the
// bridge started to finish. Cancelling before closing the client, rather
// than after, is what lets a render goroutine mid-flush notice ctx.Done()
// and return instead of racing the client's own teardown.
func (b *Bridge) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	err := b.client.Close()
	b.wg.Wait()
	return err
}

// handleMessage is the Discord client's onMessage callback. It runs on the
// client's own goroutine (discordgo's, in production; the fake's deliver, in
// tests), so everything it touches — the store, the turns interface, the
// client — must already be safe for concurrent use, and it must never block
// on the turn it starts.
func (b *Bridge) handleMessage(in Inbound) {
	// Admission first, before anything is written, logged at debug only.
	// A dropped message produces no reply of any kind: answering would
	// confirm the bot exists to whoever probed it.
	if !b.admit.AdmitMessage(in) {
		slog.Debug("discord message not admitted", "channel", in.ChannelID, "user", in.UserID)
		return
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return
	}
	// The gateway redelivers on resume, so the claim comes before the work:
	// MarkSeen is the dedupe, and it must run before any session or thread
	// is created for this message.
	fresh, err := b.store.MarkSeen(b.ctx, bridgeName, in.MessageID)
	if err != nil {
		slog.Warn("discord dedupe failed", "err", err)
		return // Failing closed: better a dropped prompt than a repeated one.
	}
	if !fresh {
		return
	}

	if content == "/new" {
		b.handleNew(in)
		return
	}

	sessionID, replyChannel, err := b.resolveSession(in)
	if err != nil {
		slog.Warn("discord session resolution failed", "err", err)
		return
	}
	b.startTurn(sessionID, replyChannel, content)
}

// handleNew starts a fresh session and rebinds the conversation to it,
// without running a turn. Rebinding an external id is legal (Store's
// BindExternal replaces the old target), which is what makes a rolling DM
// session possible: the same channel id, a new session behind it.
func (b *Bridge) handleNew(in Inbound) {
	sessionID, err := b.store.CreateSession(b.ctx, "")
	if err != nil {
		slog.Warn("discord /new: create session", "err", err)
		return
	}
	if err := b.store.BindExternal(b.ctx, bridgeName, in.ChannelID, sessionID); err != nil {
		slog.Warn("discord /new: bind session", "err", err)
		return
	}
	b.say(in.ChannelID, "started a fresh session")
}

// resolveSession maps an inbound message to the session it belongs to,
// creating one (and, for a plain guild channel message, a thread) if this is
// the conversation's first message. replyChannel is where the turn's output
// should go: the existing binding's channel, the thread just opened, or the
// original channel if opening one failed.
func (b *Bridge) resolveSession(in Inbound) (sessionID, replyChannel string, err error) {
	if sid, found, err := b.store.SessionForExternal(b.ctx, bridgeName, in.ChannelID); err != nil {
		return "", "", err
	} else if found {
		return sid, in.ChannelID, nil
	}

	sid, err := b.store.CreateSession(b.ctx, "")
	if err != nil {
		return "", "", err
	}

	// A message in a thread spore did not open (ParentID set, no existing
	// binding) or a direct message (no guild, so no threads exist at all) is
	// bound directly to the channel it arrived on. Only a first message in a
	// plain guild channel gets a thread opened for it.
	if in.ParentID != "" || in.GuildID == "" {
		if err := b.store.BindExternal(b.ctx, bridgeName, in.ChannelID, sid); err != nil {
			return "", "", err
		}
		return sid, in.ChannelID, nil
	}

	threadID, threadErr := b.client.CreateThread(b.ctx, in.ChannelID, in.MessageID, threadName(in.Content))
	bindTo, reply := in.ChannelID, in.ChannelID
	if threadErr == nil {
		bindTo, reply = threadID, threadID
	} else {
		// A bridge that cannot make threads must still work: fall back to
		// binding the channel itself, and say so rather than answering
		// silently in a mode the user didn't expect.
		slog.Warn("discord create thread failed, replying in channel", "err", threadErr)
	}
	if err := b.store.BindExternal(b.ctx, bridgeName, bindTo, sid); err != nil {
		return "", "", err
	}
	if threadErr != nil {
		b.say(reply, "could not open a thread for this, so I'll reply here instead")
	}
	return sid, reply, nil
}

// startTurn subscribes to the session's events before asking the daemon to
// run anything, then starts the turn. The order matters: turns.Subscribe
// must happen first, or the turn's first events are published to nobody and
// are lost forever — the hub does not buffer for a subscriber that has not
// yet attached.
func (b *Bridge) startTurn(sessionID, replyChannel, text string) {
	events, cancel := b.turns.Subscribe(sessionID)

	r := newRenderer(b.client, replyChannel, b.throttle)
	// onApproval and stopAfterTurn must both be set before the Consume
	// goroutine starts: renderer.approvalFn has no synchronisation of its
	// own, so setting either concurrently with Consume's read of them is a
	// data race.
	r.onApproval(func(ev daemon.WireEvent) { b.postApproval(sessionID, replyChannel, ev) })
	r.stopAfterTurn = true

	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer cancel()
		// b.ctx, never a per-message context: the turn outlives the message
		// that started it, and so must the goroutine rendering it.
		r.Consume(b.ctx, events)
	}()

	if err := b.turns.StartTurn(sessionID, text, bridgeName, policy.ProfileRemote); err != nil {
		// The subscription was for a turn that never ran; detach it. cancel
		// is idempotent (Hub.Subscribe's is a sync.Once), so this races
		// nothing against the goroutine's own deferred cancel.
		cancel()
		if errors.Is(err, daemon.ErrTurnRunning) {
			b.say(replyChannel, "that session is already running a turn; wait for it to finish")
			return
		}
		b.say(replyChannel, "could not start the turn: "+err.Error())
		return
	}
}

// postApproval renders one approval request as a message with buttons. It is
// the renderer's approvalFn for every turn the bridge starts.
func (b *Bridge) postApproval(sessionID, channelID string, ev daemon.WireEvent) {
	if _, err := b.client.Send(b.ctx, channelID, approvalMessage(sessionID, ev)); err != nil {
		slog.Warn("discord approval post failed", "err", err)
	}
}

// handleInteraction is the Discord client's onInteraction callback, for
// button presses.
func (b *Bridge) handleInteraction(i Interaction) {
	// The same admission rules as a message. A button press is a second
	// entrance to the same house.
	if !b.admit.AdmitInteraction(i) {
		slog.Debug("discord interaction not admitted", "channel", i.ChannelID, "user", i.UserID)
		return
	}
	sessionID, pendingID, ans, err := decodeCustomID(i.CustomID)
	if err != nil {
		return // Not one of ours, or malformed. Silence again.
	}
	// Discord fails the interaction visibly if it is not acknowledged within
	// three seconds, so answer, then report.
	msg, err := b.answer.answer(b.ctx, sessionID, pendingID, ans)
	if err != nil {
		msg = "could not record that: " + err.Error()
	}
	if err := b.client.Respond(b.ctx, i.ID, i.Token, msg); err != nil {
		slog.Warn("discord interaction response", "err", err)
	}
}

// say is a logged best-effort Send, for the bridge's own status messages
// (confirmations, errors) rather than turn output.
func (b *Bridge) say(channelID, text string) {
	if _, err := b.client.Send(b.ctx, channelID, Message{Content: text}); err != nil {
		slog.Warn("discord send failed", "err", err)
	}
}

// threadName derives a Discord thread name from a prompt: its first line,
// whitespace collapsed, truncated to 90 runes (Discord's own cap is 100;
// this leaves room for an ellipsis a future truncation might add), falling
// back to a fixed name when the prompt has no visible content.
func threadName(prompt string) string {
	line := prompt
	if i := strings.IndexAny(line, "\r\n"); i >= 0 {
		line = line[:i]
	}
	line = strings.Join(strings.Fields(line), " ")
	line = truncate(line, 90)
	if line == "" {
		return "spore session"
	}
	return line
}
