package discord

import (
	"context"
	"log/slog"
	"strconv"
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

// defaultTypingEvery is how often the typing indicator is renewed. Discord
// expires it after about ten seconds, so this must stay comfortably under
// that or the indicator flickers off between turns of a long tool run.
const defaultTypingEvery = 8 * time.Second

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

	// buf is the text the model has written that has not been converted yet;
	// out is what conversion produced and the screen has not received. They
	// are separate so every byte passes through dial exactly once: text that
	// a failed Send leaves waiting in out must not be converted again.
	// msgID is the message out belongs to, empty when the next flush must
	// Send rather than Edit.
	buf      strings.Builder
	out      strings.Builder
	msgID    string
	onScreen int // characters already committed to msgID

	// dial rewrites the model's markdown into the dialect Discord renders.
	// It is per renderer because it carries stream state across flushes.
	dial dialect

	// currentContent tracks the full content of the current message, used when
	// editing to send the complete updated message.
	currentContent strings.Builder

	pendingCalls map[string]string // tool_use_id -> tool name
	approvalFn   func(daemon.WireEvent)

	// The tool calls of the turn in flight, collapsed onto one activity
	// message rather than two embeds per call. activityID is that message
	// (empty until the first tool call), tools is what it summarises, and
	// byUseID finds the entry a result belongs to. detailID names the
	// transcript in the bridge's details store, and recordTools publishes
	// it there — the goroutine running this renderer ends with the turn,
	// but the button on the activity message outlives it.
	activityID  string
	tools       []toolEntry
	byUseID     map[string]int
	detailID    string
	recordTools func(detailID string, entries []toolEntry)

	// typingFn is called when the turn starts and on every typingEvery tick
	// for as long as it runs: Discord expires the indicator after about ten
	// seconds, so it has to be renewed rather than set once.
	typingFn    func(context.Context)
	typingEvery time.Duration

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
		byUseID:      make(map[string]int),
		typingEvery:  defaultTypingEvery,
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
	// Typing starts at once rather than one tick in: the whole point is to
	// show something during the silence before the first token arrives.
	var typingTicker *time.Ticker
	var typingChan <-chan time.Time
	if r.typingFn != nil {
		r.typingFn(ctx)
		if r.typingEvery > 0 {
			typingTicker = time.NewTicker(r.typingEvery)
			typingChan = typingTicker.C
		}
	}
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
		if typingTicker != nil {
			typingTicker.Stop()
		}
		r.flush(ctx)
	}()

	for {
		select {
		case <-typingChan:
			r.typingFn(ctx)
		case ev, ok := <-events:
			if !ok {
				return
			}
			r.handleEvent(ctx, ev)
			if r.stopAfterTurn && (ev.Type == daemon.WireTurnDone || ev.Type == daemon.WireError || ev.Type == daemon.WireStopped) {
				return
			}
		case <-tickChan:
			r.flushPartial(ctx)
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
		//
		// Mid-turn this is always the partial flush: a table that is still
		// arriving has to be held back rather than half rewritten.
		if r.throttle <= 0 || r.onScreen+r.out.Len()+r.buf.Len() >= messageLimit {
			r.flushPartial(ctx)
		}

	case daemon.WireToolCall:
		// The prose written so far belongs above the activity message, so it
		// is flushed and closed before the activity message is touched.
		r.flush(ctx)
		r.pendingCalls[ev.ToolUseID] = ev.Tool
		r.byUseID[ev.ToolUseID] = len(r.tools)
		r.tools = append(r.tools, toolEntry{Name: ev.Tool, Args: ev.Args})
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()
		r.publishTools()
		r.writeActivity(ctx)

	case daemon.WireToolResult:
		delete(r.pendingCalls, ev.ToolUseID)
		// A result for a call this renderer never saw (a turn resumed mid
		// flight, say) still deserves a line rather than being dropped.
		i, ok := r.byUseID[ev.ToolUseID]
		if !ok {
			i = len(r.tools)
			r.byUseID[ev.ToolUseID] = i
			r.tools = append(r.tools, toolEntry{Name: "tool"})
		}
		r.tools[i].Result = ev.Content
		r.tools[i].IsError = ev.IsError
		r.publishTools()
		r.writeActivity(ctx)

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

	case daemon.WireTurnDone, daemon.WireStopped:
		r.flush(ctx)
		// Reset for next turn so each turn starts fresh
		r.msgID = ""
		r.onScreen = 0
		r.currentContent.Reset()
		clear(r.pendingCalls)
		r.resetActivity()

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
		r.resetActivity()
	}
}

// activityLine is the collapsed view of a turn's tool calls: the tools in
// call order, a failed one marked, and a count. It is the whole feed a turn's
// machinery gets — the arguments and results live behind the button.
func (r *renderer) activityLine() string {
	names := make([]string, 0, len(r.tools))
	for _, t := range r.tools {
		if t.IsError {
			names = append(names, "⚠ "+t.Name)
			continue
		}
		names = append(names, t.Name)
	}
	line := "⚙ " + strings.Join(names, " · ")
	unit := " tools)"
	if len(r.tools) == 1 {
		unit = " tool)"
	}
	line += "  (" + strconv.Itoa(len(r.tools)) + unit
	return truncate(line, messageLimit-200)
}

// writeActivity sends the turn's activity message the first time and edits it
// in place afterwards, so a turn's tool calls stay one line in the feed
// however many of them there are. It bypasses the text message's state
// entirely (msgID, currentContent): the two messages interleave on screen and
// must not share a cursor.
func (r *renderer) writeActivity(ctx context.Context) {
	m := Message{Content: r.activityLine()}
	// No transcript id means nothing to show: a renderer built without one
	// (a test, or a caller that did not wire the details store) renders the
	// line alone rather than a button that can only ever say "expired".
	if r.detailID != "" {
		m.Buttons = []Button{{CustomID: detailsCustomID(r.detailID), Label: "Show details"}}
	}
	if r.activityID == "" {
		id, err := r.client.Send(ctx, r.channelID, m)
		if err != nil {
			slog.Warn("discord activity send", "err", err)
			return
		}
		r.activityID = id
		return
	}
	if err := r.client.Edit(ctx, r.channelID, r.activityID, m); err != nil {
		slog.Warn("discord activity edit", "err", err)
	}
}

// publishTools hands the transcript so far to the bridge, which outlives this
// renderer and answers the button press.
func (r *renderer) publishTools() {
	if r.recordTools != nil && r.detailID != "" {
		r.recordTools(r.detailID, r.tools)
	}
}

// resetActivity ends the turn's activity message. The transcript stays in the
// bridge's details store, so the button on the message still works; only this
// renderer's cursor is cleared, and a following turn starts its own line.
func (r *renderer) resetActivity() {
	r.activityID = ""
	r.tools = nil
	clear(r.byUseID)
}

// flush puts everything written so far on screen, converting it to Discord's
// dialect first. It is the end-of-turn form: nothing is held back, so a turn
// that stops inside a table still shows its rows.
func (r *renderer) flush(ctx context.Context) {
	r.convert(true)
	r.drain(ctx)
}

// flushPartial is the mid-turn form. It withholds the tail of a construct
// that is still arriving — a table whose rows have not stopped coming — so
// the screen never receives half a rewrite, which no later edit could repair.
func (r *renderer) flushPartial(ctx context.Context) {
	r.convert(false)
	r.drain(ctx)
}

// convert moves what it can from buf into out, rewriting it on the way.
func (r *renderer) convert(final bool) {
	if r.buf.Len() == 0 {
		if final {
			r.dial.convert("", true)
		}
		return
	}
	src := r.buf.String()
	take := src
	if !final {
		take = src[:len(src)-r.dial.hold(src)]
	}
	r.buf.Reset()
	r.buf.WriteString(src[len(take):])
	r.out.WriteString(r.dial.convert(take, final))
}

// drain puts the converted text on screen, splitting at the message limit.
// Splitting prefers the last newline in the overflowing chunk so a code block
// or paragraph is not cut mid-line when there is a reasonable place to cut.
func (r *renderer) drain(ctx context.Context) {
	for r.out.Len() > 0 {
		room := messageLimit
		sending := r.msgID == ""
		if !sending {
			// When appending to an open message, derive room from actual content
			// length to account for any failed writes. Failed edits don't update
			// onScreen, but currentContent grows anyway, so we use its length to
			// compute room conservatively — splitting earlier if needed to stay safe.
			room = messageLimit - r.currentContent.Len()
		}
		text := r.out.String()
		if len(text) <= room {
			ok := r.write(ctx, text, nil)
			if sending && !ok {
				// A failed Send never reached Discord, so unlike a failed Edit
				// nothing captured this text anywhere else (currentContent for
				// an unsent message is not self-healing — the next Send just
				// overwrites it). Leave out intact so the same text is retried
				// as a new Send on the next flush, and stop here rather than
				// spinning on the same failure within this call.
				return
			}
			r.out.Reset()
			return
		}
		head, tail := splitAt(text, room)
		ok := r.write(ctx, head, nil)
		if !ok {
			if sending {
				// A failed Send never reached Discord, so nothing captured
				// this text anywhere else. out still holds the whole,
				// untouched text (head+tail) — retry it whole on the next
				// flush.
				return
			}
			// A failed Edit is different: write already appended head into
			// currentContent before the Edit call (its self-healing
			// convention — see write's comment), so the live message will
			// pick up head the next time an edit to it succeeds. Do NOT
			// widen this to "retry head+tail whole" the way the Send case
			// does — head is already accounted for in currentContent, and
			// requeuing it into out would send it a second time once a
			// later edit succeeds, duplicating it on screen. So keep msgID
			// and currentContent exactly as they are (do not treat this
			// message as closed) and only requeue tail, the part nothing
			// has captured yet.
			r.out.Reset()
			r.out.WriteString(tail)
			return
		}
		// Whatever did not fit belongs to a new message.
		r.msgID, r.onScreen = "", 0
		r.currentContent.Reset()
		r.out.Reset()
		r.out.WriteString(tail)
	}
}

// write sends when msgID is empty and edits otherwise, recording the new msgID.
// It reports whether the call succeeded. On error it logs and returns false
// without otherwise changing state. Failed edits leave currentContent intact
// for recovery on the next successful flush (self-healing); a failed Send has
// no such recovery path — nothing server-side captured the content — so the
// caller (flush) is responsible for retrying rather than discarding it.
func (r *renderer) write(ctx context.Context, content string, embeds []Embed) bool {
	if r.msgID == "" {
		// Send a new message
		r.currentContent.Reset()
		r.currentContent.WriteString(content)
		m := Message{Content: content, Embeds: embeds}
		id, err := r.client.Send(ctx, r.channelID, m)
		if err != nil {
			slog.Warn("discord render", "err", err)
			return false
		}
		r.msgID = id
		r.onScreen = r.currentContent.Len()
		return true
	}
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
		return false
	}
	r.onScreen = r.currentContent.Len()
	return true
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
