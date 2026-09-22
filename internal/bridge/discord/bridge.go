package discord

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
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

// Sessions creates the sessions this bridge binds threads to. It is an
// interface for the same reason Turns is: the bridge is tested without a
// daemon.
type Sessions interface {
	CreateSession(ctx context.Context, title, requested string, profile policy.Profile) (string, error)
}

// Options are the bridge's collaborators. Guard may be nil — a bridge built
// without one still answers approvals through the broker's live-waiter path,
// it just cannot recover a suspension whose turn is gone (see answer.go).
type Options struct {
	Cfg      config.DiscordConfig
	Client   Client
	Turns    Turns
	Sessions Sessions
	Store    *store.Store
	Broker   *daemon.Broker
	Guard    *policy.Guard
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
	sessions Sessions
	store    *store.Store
	answer   *answerer
	throttle time.Duration

	// details holds recent turns' tool transcripts for the "show details"
	// button. It lives here, not on the renderer, because the press arrives
	// after the renderer's goroutine has ended with its turn.
	details *details

	// ctx bounds the render goroutines. It is the BRIDGE's context, never a
	// message's: a turn outlives the message that started it, and so must
	// the goroutine rendering it.
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// resolveMu serialises resolveSession end to end, including the
	// CreateThread call. It protects the paths where one external id is
	// meant to map to exactly one session over time: a DM channel (one
	// rolling session per admitted user, per the design spec) and a thread
	// spore did not open itself. Two first DMs from the same user arriving
	// on separate discordgo dispatch goroutines would otherwise both miss
	// SessionForExternal, both CreateSession, and both BindExternal — two
	// sessions for what the spec calls one rolling conversation, with the
	// second write silently winning. It does NOT exist to stop two threads
	// from being opened in a guild channel: the spec is explicit that every
	// top-level channel message opens its OWN session and thread, so two
	// concurrent first messages in a channel correctly producing two
	// threads is not a race to fix. spore serves one person, so serialising
	// the whole function costs nothing worth avoiding — a per-key lock or
	// singleflight would be solving a throughput problem this bridge
	// doesn't have.
	resolveMu sync.Mutex

	// closeMu and closing rule out a race between Close's wg.Wait and a
	// concurrent startTurn's wg.Add. sync.WaitGroup requires that an Add
	// which takes the counter from zero happen before the Wait it is paired
	// with, and nothing about Client.Close documents that in-flight
	// handleMessage calls have returned by the time it returns — so timing
	// alone cannot be trusted to keep Add ahead of Wait. Close sets closing
	// under this lock before it ever calls Wait; startTurn checks closing
	// under the same lock before calling Add. A goroutine that observes
	// closing == false is therefore guaranteed to complete its Add before
	// Close can reach Wait, and one that observes true simply declines to
	// start new work — the race is impossible by construction, not by luck.
	closeMu sync.Mutex
	closing bool
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
	if o.Sessions == nil {
		return nil, errors.New("discord bridge: Sessions is required")
	}
	if o.Store == nil {
		return nil, errors.New("discord bridge: Store is required")
	}
	if o.Broker == nil {
		return nil, errors.New("discord bridge: Broker is required")
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
		sessions: o.Sessions,
		store:    o.Store,
		answer:   newAnswerer(o.Broker, o.Guard),
		throttle: throttle,
		details:  newDetails(),
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
// bridge started to finish. closing is set under closeMu first, and before
// anything else, so that no startTurn call still in flight can register a
// wg.Add after this reaches wg.Wait (see the field doc on closeMu). Only
// then does it cancel — before closing the client, rather than after, which
// is what lets a render goroutine mid-flush notice ctx.Done() and return
// instead of racing the client's own teardown.
func (b *Bridge) Close() error {
	b.closeMu.Lock()
	b.closing = true
	b.closeMu.Unlock()

	if b.cancel != nil {
		b.cancel()
	}
	err := b.client.Close()
	b.wg.Wait()
	return err
}

// cancelContext cancels the bridge's internal context. Called after a failed
// Start attempt to clean up the child context before retrying. This does not
// close the client or wait for goroutines — it only cancels the context,
// which is safe even if no goroutines were ever started.
func (b *Bridge) cancelContext() {
	if b.cancel != nil {
		b.cancel()
	}
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
	// Discord delivers a ping as a raw token — "<@1418> what time is it?" —
	// and nothing downstream renders it back into a name: the agent would
	// read an opaque id, and the thread name (a channel name, where Discord
	// renders nothing at all) would literally read "<@1418>". Drop the ping
	// that addressed the bot; mentions of OTHER people further into the
	// sentence are content and stay.
	content := stripAddressPing(strings.TrimSpace(in.Content))
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
	// Acknowledge before there is anything to say: the eyes go on the
	// message as soon as it is claimed, and are traded for a check when the
	// turn that answers it ends. After the dedupe, never before, so a
	// gateway resume does not re-react to a message already handled.
	b.react(in.ChannelID, in.MessageID, emojiEyes)

	if content == "/new" {
		b.handleNew(in)
		// /new answers immediately and starts no turn, so the goroutine that
		// settles an ordinary prompt's acknowledgement never runs for it.
		// Settle here or the eyes stay on a command that is already done.
		b.settle(in)
		return
	}

	sessionID, replyChannel, err := b.resolveSession(in)
	if err != nil {
		slog.Warn("discord session resolution failed", "err", err)
		return
	}
	b.startTurn(sessionID, replyChannel, in, content)
}

// handleNew starts a fresh session and rebinds the conversation to it,
// without running a turn. Rebinding an external id is legal (Store's
// BindExternal replaces the old target), which is what makes a rolling DM
// session possible: the same channel id, a new session behind it.
//
// That rebind is only safe on the surfaces resolveSession itself binds
// directly to a channel/thread id: a DM (no GuildID), or a reply inside a
// thread (ParentID set). A plain guild channel is the opposite case —
// resolveSession's own comment explains why it deliberately leaves the
// channel unbound: every top-level message there must open its own session
// and thread. Binding the channel here would write exactly what that
// function goes out of its way not to write, silently turning every later
// top-level message in the channel into a continuation of this one session
// instead of a new thread.
func (b *Bridge) handleNew(in Inbound) {
	if in.GuildID != "" && in.ParentID == "" {
		b.say(in.ChannelID, "/new only applies in a DM or inside a thread; a new message in this channel already starts a fresh session and thread")
		return
	}

	sessionID, err := b.sessions.CreateSession(b.ctx, "", "", policy.ProfileRemote)
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
//
// Per the design spec (§8 Bridges), a DM is one rolling session per admitted
// user, and a reply inside a thread spore did not open continues whatever
// session that thread is already bound to — both are cases where one
// external id must map to exactly one session over time. A top-level message
// in a guild channel is the opposite: it always opens its OWN session and
// thread, by design, so two such messages arriving together correctly
// produce two threads, not a race to prevent.
//
// The whole function still runs under resolveMu, including the CreateThread
// call, so it stays simple to reason about — but the lock is only load
// bearing for the DM and reopened-thread cases above: it is what stops two
// concurrent first DMs from the same user each creating their own session
// and each winning the same BindExternal write.
func (b *Bridge) resolveSession(in Inbound) (sessionID, replyChannel string, err error) {
	b.resolveMu.Lock()
	defer b.resolveMu.Unlock()

	if sid, found, err := b.store.SessionForExternal(b.ctx, bridgeName, in.ChannelID); err != nil {
		return "", "", err
	} else if found {
		return sid, in.ChannelID, nil
	}

	sid, err := b.sessions.CreateSession(b.ctx, "", "", policy.ProfileRemote)
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
	if threadErr != nil {
		// A bridge that cannot make threads must still work: fall back to
		// binding the channel itself, and say so rather than answering
		// silently in a mode the user didn't expect.
		slog.Warn("discord create thread failed, replying in channel", "err", threadErr)
		if err := b.store.BindExternal(b.ctx, bridgeName, in.ChannelID, sid); err != nil {
			return "", "", err
		}
		b.say(in.ChannelID, "could not open a thread for this, so I'll reply here instead")
		return sid, in.ChannelID, nil
	}

	// Bind only the thread, not the origin channel. The spec is explicit
	// that every top-level channel message opens its own session and
	// thread, so the channel id must stay unbound — binding it would make
	// the SECOND thing asked in this channel silently resolve to today's
	// session instead of starting a new one, which is exactly the DM
	// behaviour the spec reserves for DMs alone.
	if err := b.store.BindExternal(b.ctx, bridgeName, threadID, sid); err != nil {
		// The thread now exists and looks like an ordinary conversation to
		// the user, but the bind that points it at sid failed. MarkSeen
		// already claimed this message's id before resolveSession was ever
		// called, so there is no retry coming from gateway redelivery — the
		// prompt is gone unless we say something now, in the only place the
		// user is watching. The session row CreateSession made above is
		// left in place: Store has no delete-session call, and an unbound,
		// message-less row is harmless clutter, not a leak of anything that
		// matters.
		b.say(threadID, "something went wrong opening this conversation; please send your message again")
		return "", "", err
	}
	return sid, threadID, nil
}

// startTurn subscribes to the session's events before asking the daemon to
// run anything, then starts the turn. The order matters: turns.Subscribe
// must happen first, or the turn's first events are published to nobody and
// are lost forever — the hub does not buffer for a subscriber that has not
// yet attached.
func (b *Bridge) startTurn(sessionID, replyChannel string, in Inbound, text string) {
	events, cancel := b.turns.Subscribe(sessionID)

	r := newRenderer(b.client, replyChannel, b.throttle)
	// detailID names this turn's transcript before the turn runs, because
	// the button carrying it is rendered while the turn is still going.
	r.detailID = b.details.mintID()
	r.recordTools = b.details.put
	r.typingFn = func(ctx context.Context) {
		if err := b.client.Typing(ctx, replyChannel); err != nil {
			slog.Debug("discord typing failed", "err", err)
		}
	}
	// onApproval and stopAfterTurn must both be set before the Consume
	// goroutine starts: renderer.approvalFn has no synchronisation of its
	// own, so setting either concurrently with Consume's read of them is a
	// data race.
	r.onApproval(func(ev daemon.WireEvent) { b.postApproval(sessionID, replyChannel, ev) })
	r.stopAfterTurn = true

	// See closeMu's doc on Bridge: this check-then-Add, both under closeMu,
	// is what keeps a wg.Add from ever landing after Close has moved on to
	// wg.Wait. If Close already flipped closing, there is nothing left to
	// join a turn to — detach the subscription and give up on this message
	// rather than starting work the bridge is already shutting down.
	b.closeMu.Lock()
	if b.closing {
		b.closeMu.Unlock()
		cancel()
		return
	}
	b.wg.Add(1)
	b.closeMu.Unlock()

	go func() {
		defer b.wg.Done()
		defer cancel()
		// b.ctx, never a per-message context: the turn outlives the message
		// that started it, and so must the goroutine rendering it.
		r.Consume(b.ctx, events)
		// Consume returns when the turn ends, so this is the one place that
		// knows the prompt has been answered.
		b.settle(in)
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
	if isDetailsCustomID(i.CustomID) {
		b.showDetails(i)
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

// mentionToken matches a Discord mention as it arrives on the wire: a user
// (<@123>, or <@!123> from older clients), a role (<@&123>), or a channel
// (<#123>). Discord renders these as names only inside a message body.
var mentionToken = regexp.MustCompile(`<@[!&]?[0-9]+>|<#[0-9]+>`)

// stripAddressPing removes the leading mentions that address the bot, and
// the whitespace after them, leaving the rest of the prompt untouched. Only
// leading tokens go: a mention further in ("ask <@99> about the deploy") is
// part of what was said, not part of how the bot was summoned.
func stripAddressPing(s string) string {
	for {
		loc := mentionToken.FindStringIndex(s)
		if loc == nil || loc[0] != 0 {
			return s
		}
		s = strings.TrimLeft(s[loc[1]:], " \t")
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
	// Unlike a message body, a channel name renders no mentions, so any
	// token left here — including one naming a third person mid-sentence,
	// which the prompt itself keeps — would show as a bare "<@1418>".
	line = mentionToken.ReplaceAllString(line, " ")
	line = strings.Join(strings.Fields(line), " ")
	line = truncate(line, 90)
	if line == "" {
		return "spore session"
	}
	return line
}

// showDetails answers a "show details" press with the turn's tool
// transcript, ephemerally — only the person who pressed sees it, so the
// collapsed feed stays collapsed for everyone else in the channel.
//
// A miss is the normal end of a transcript's life, not an error: the store
// holds the most recent turns only, and a restart empties it while the old
// buttons stay on screen forever. Say so plainly rather than silently doing
// nothing, which would read as a broken button.
func (b *Bridge) showDetails(i Interaction) {
	content := "those details are no longer in memory — `spore trace` has the full turn"
	if entries, ok := b.details.get(detailsIDFrom(i.CustomID)); ok {
		content = renderDetails(entries)
	}
	if err := b.client.Respond(b.ctx, i.ID, i.Token, content); err != nil {
		slog.Warn("discord details response", "err", err)
	}
}

// settle marks a message as dealt with: the check goes on, the eyes come
// off. The order matters — if only one of the two calls can get through,
// "answered" is the more useful thing to be left on screen than nothing.
//
// Only the paths that actually finished call this. A turn that could not be
// started, or a session that could not be resolved, deliberately leaves the
// eyes in place: they are then the truth, not a stale indicator.
func (b *Bridge) settle(in Inbound) {
	b.react(in.ChannelID, in.MessageID, emojiDone)
	b.unreact(in.ChannelID, in.MessageID, emojiEyes)
}

// react and unreact are logged best-effort reaction calls. A reaction is an
// acknowledgement, never a correctness requirement: Discord refusing one
// (missing permission, a deleted message) must not disturb the turn.
func (b *Bridge) react(channelID, messageID, emoji string) {
	if err := b.client.React(b.ctx, channelID, messageID, emoji); err != nil {
		slog.Warn("discord react failed", "emoji", emoji, "err", err)
	}
}

func (b *Bridge) unreact(channelID, messageID, emoji string) {
	if err := b.client.Unreact(b.ctx, channelID, messageID, emoji); err != nil {
		slog.Warn("discord unreact failed", "emoji", emoji, "err", err)
	}
}
