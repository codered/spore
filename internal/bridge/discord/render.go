package discord

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/codered/spore/internal/daemon"
)

// messageLimit is Discord's hard cap on a message's content.
const messageLimit = 2000

// defaultThrottle bounds how often a streaming turn edits its message.
// Discord's rate limits are per channel and unforgiving; a turn that emits a
// hundred deltas a second must still only edit a few times a second.
const defaultThrottle = 1500 * time.Millisecond

// renderer turns one session's event stream into Discord messages. A turn
// emits many small text deltas, so it accumulates them and edits a single
// message on a throttle rather than posting per delta — which would be both
// unreadable and instantly rate limited.
//
// Every Discord call's error is logged and swallowed. Discord is a network:
// a failed edit must never stop the goroutine draining the hub, because a
// stalled drain means the session silently stops updating.
type renderer struct {
	client    Client
	channelID string
	throttle  time.Duration

	// buf is the text not yet on screen; msgID is the message it belongs to,
	// empty when the next flush must Send rather than Edit.
	buf      strings.Builder
	msgID    string
	onScreen int // characters already committed to msgID

	// currentContent tracks the full content of the current message, used when
	// editing to send the complete updated message.
	currentContent strings.Builder

	pendingCalls map[string]string // tool_use_id -> tool name
	approvalFn   func(daemon.WireEvent)

	// stopAfterTurn makes Consume return once the turn it was started for
	// ends. The bridge sets it: one goroutine per turn, ending with the turn,
	// so a long-lived session does not accumulate one renderer per prompt.
	stopAfterTurn bool
}

// newRenderer creates a renderer for one session's events.
// channelID is where messages will be sent.
// throttle is how long to wait between edits; zero means flush on every event.
func newRenderer(c Client, channelID string, throttle time.Duration) *renderer {
	return &renderer{
		client:       c,
		channelID:    channelID,
		throttle:     throttle,
		pendingCalls: make(map[string]string),
	}
}

// Consume drains events until the channel closes or ctx is done, then flushes
// whatever is left. It is the renderer's whole public surface.
func (r *renderer) Consume(ctx context.Context, events <-chan daemon.WireEvent) {
	var ticker *time.Ticker
	var tickChan <-chan time.Time
	if r.throttle > 0 {
		ticker = time.NewTicker(r.throttle)
		tickChan = ticker.C
	}
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		r.flush(ctx)
	}()

	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			r.handleEvent(ctx, ev)
			if r.stopAfterTurn && (ev.Type == daemon.WireTurnDone || ev.Type == daemon.WireError) {
				return
			}
		case <-tickChan:
			r.flush(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// handleEvent processes one event, accumulating text or sending embeds as needed.
func (r *renderer) handleEvent(ctx context.Context, ev daemon.WireEvent) {
	switch ev.Type {
	case daemon.WireText:
		r.buf.WriteString(ev.Text)
		// With throttle=0, tests don't wait on a clock; flush on every event
		// so the test loop runs to completion immediately. If buffered text would
		// exceed the message limit, split now to stay under Discord's 2000-char cap.
		if r.throttle <= 0 || r.onScreen+r.buf.Len() >= messageLimit {
			r.flush(ctx)
		}

	case daemon.WireToolCall:
		r.flush(ctx)
		r.pendingCalls[ev.ToolUseID] = ev.Tool

		// Send embed for the tool call. Each embed is a boundary in the
		// transcript, so the embed gets its own message and any following text
		// starts a fresh one.
		desc := truncate(ev.Args, 1000)
		// Wrap in code fence
		desc = "```\n" + desc + "\n```"
		r.sendEmbed(ctx, Embed{
			Title:       "⚙ " + ev.Tool,
			Description: desc,
		})
		// Reset so the next embed gets its own message, not an edit.
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()

	case daemon.WireToolResult:
		// Get the tool name from pending calls
		toolName := r.pendingCalls[ev.ToolUseID]
		delete(r.pendingCalls, ev.ToolUseID)

		// Send result embed as its own message.
		desc := truncate(ev.Content, 1000)
		r.sendEmbed(ctx, Embed{
			Title:       "↳ " + toolName,
			Description: desc,
			Error:       ev.IsError,
		})
		// Reset so any following text starts a fresh message.
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()

	case daemon.WireApproval:
		r.flush(ctx)
		if r.approvalFn != nil {
			r.approvalFn(ev)
		} else {
			// Render as plain text if no approval handler
			r.buf.WriteString("⚠ Approval needed: ")
			r.buf.WriteString(ev.Rule)
			if ev.Pattern != "" {
				r.buf.WriteString(" (")
				r.buf.WriteString(ev.Pattern)
				r.buf.WriteString(")")
			}
			r.buf.WriteString("\n")
			r.flush(ctx)
		}

	case daemon.WireResolved:
		r.buf.WriteString("→ ")
		r.buf.WriteString(ev.Decision)
		r.buf.WriteString("\n")
		if r.throttle <= 0 {
			r.flush(ctx)
		}

	case daemon.WireTurnDone:
		r.flush(ctx)
		// Reset for next turn so each turn starts fresh
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()
		clear(r.pendingCalls)

	case daemon.WireError:
		r.flush(ctx)
		// Send error as both content and embed so it's visible in all contexts
		content := "**Error:** " + ev.Error
		embed := Embed{
			Title:       "turn failed",
			Description: ev.Error,
			Error:       true,
		}
		r.write(ctx, content, []Embed{embed})
		// Reset for next turn (error ends the turn)
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()
		clear(r.pendingCalls)
	}
}

// sendEmbed sends a message containing only an embed.
func (r *renderer) sendEmbed(ctx context.Context, e Embed) {
	r.write(ctx, "", []Embed{e})
}

// flush puts the buffered text on screen, splitting at the message limit.
// Splitting prefers the last newline in the overflowing chunk so a code block
// or paragraph is not cut mid-line when there is a reasonable place to cut.
func (r *renderer) flush(ctx context.Context) {
	for r.buf.Len() > 0 {
		room := messageLimit
		if r.msgID != "" {
			// When appending to an open message, derive room from actual content
			// length to account for any failed writes. Failed edits don't update
			// onScreen, but currentContent grows anyway, so we use its length to
			// compute room conservatively — splitting earlier if needed to stay safe.
			room = messageLimit - r.currentContent.Len()
		}
		text := r.buf.String()
		if len(text) <= room {
			r.write(ctx, text, nil)
			r.buf.Reset()
			return
		}
		head, tail := splitAt(text, room)
		r.write(ctx, head, nil)
		// Whatever did not fit belongs to a new message.
		r.msgID, r.onScreen = "", 0
		r.currentContent.Reset()
		r.buf.Reset()
		r.buf.WriteString(tail)
	}
}

// write sends when msgID is empty and edits otherwise, recording the new msgID.
// On error it logs and returns without changing state. Failed edits leave
// currentContent intact for recovery on the next successful flush (self-healing).
func (r *renderer) write(ctx context.Context, content string, embeds []Embed) {
	if r.msgID == "" {
		// Send a new message
		r.currentContent.Reset()
		r.currentContent.WriteString(content)
		m := Message{Content: content, Embeds: embeds}
		id, err := r.client.Send(ctx, r.channelID, m)
		if err != nil {
			slog.Warn("discord render", "err", err)
			return
		}
		r.msgID = id
		r.onScreen = r.currentContent.Len()
	} else {
		// Edit the existing message with appended content. Append to currentContent
		// before attempting the edit, so failed edits still have the full delta for
		// recovery. Only update onScreen on success, since the server saw nothing.
		// The next flush derives room from currentContent.Len(), making it
		// conservative when a message hasn't been edited successfully yet.
		r.currentContent.WriteString(content)
		fullContent := r.currentContent.String()
		m := Message{Content: fullContent, Embeds: embeds}
		err := r.client.Edit(ctx, r.channelID, r.msgID, m)
		if err != nil {
			slog.Warn("discord render", "err", err)
			return
		}
		r.onScreen = r.currentContent.Len()
	}
}

// onApproval sets the handler for approval events. Left nil, an
// approval event is rendered as plain text.
func (r *renderer) onApproval(fn func(daemon.WireEvent)) {
	r.approvalFn = fn
}

// splitAt cuts s at n, preferring the last '\n' at or before n when one exists
// past n/2, else exactly at n. It must be rune-safe — never split inside a
// multi-byte rune.
func splitAt(s string, n int) (head, tail string) {
	// If the string fits in n, return it all as head
	if len(s) <= n {
		return s, ""
	}

	// Look for the last newline between n/2 and n
	preferredCut := -1
	if n >= 2 {
		searchStart := n / 2
		for i := n; i > searchStart; i-- {
			if i <= len(s) && s[i-1] == '\n' {
				preferredCut = i
				break
			}
		}
	}

	// If we found a good newline, use it
	if preferredCut > 0 && preferredCut <= len(s) {
		return s[:preferredCut], s[preferredCut:]
	}

	// Otherwise, find the last rune boundary at or before n
	// by walking backwards from n, skipping continuation bytes
	cutPoint := n
	if cutPoint > len(s) {
		cutPoint = len(s)
	}

	// Walk backwards to find a valid rune boundary
	for cutPoint > 0 && cutPoint < len(s) {
		// Check if the byte at cutPoint is the start of a rune
		if (s[cutPoint] & 0xC0) != 0x80 {
			break
		}
		cutPoint--
	}

	return s[:cutPoint], s[cutPoint:]
}
