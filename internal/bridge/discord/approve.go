package discord

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/policy"
)

// customIDPrefix namespaces our components. Discord hands the custom id back
// verbatim on a press and nothing else, so it is the entire message from the
// button to the handler — and it arrives from the network, so it is parsed as
// untrusted input rather than trusted because we wrote it.
const customIDPrefix = "spore"

// encodeCustomID packs the answer into Discord's 100-character custom id.
func encodeCustomID(sessionID string, pendingID int64, allow bool, scope policy.Scope) string {
	verdict := "deny"
	if allow {
		verdict = "allow"
	}
	return strings.Join([]string{customIDPrefix, sessionID, strconv.FormatInt(pendingID, 10), verdict, string(scope)}, "|")
}

// decodeCustomID parses a custom id from a button press. An unknown prefix,
// a bad number, or an unknown scope is an error, never a default: a scope
// that silently became "once" would be confusing, and one that silently
// became "pattern" would be a hole.
func decodeCustomID(s string) (string, int64, policy.Answer, error) {
	parts := strings.Split(s, "|")
	if len(parts) != 5 || parts[0] != customIDPrefix {
		return "", 0, policy.Answer{}, fmt.Errorf("not a spore component id: %q", s)
	}
	pendingID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", 0, policy.Answer{}, fmt.Errorf("bad pending id in %q: %w", s, err)
	}
	var allow bool
	switch parts[3] {
	case "allow":
		allow = true
	case "deny":
	default:
		return "", 0, policy.Answer{}, fmt.Errorf("bad verdict in %q", s)
	}
	scope := policy.Scope(parts[4])
	switch scope {
	case policy.ScopeOnce, policy.ScopeSession, policy.ScopePattern:
	default:
		return "", 0, policy.Answer{}, fmt.Errorf("bad scope in %q", s)
	}
	if parts[1] == "" {
		return "", 0, policy.Answer{}, fmt.Errorf("empty session in %q", s)
	}
	return parts[1], pendingID, policy.Answer{Allow: allow, Scope: scope}, nil
}

// truncateLabel caps a label at Discord's 80-character button-label limit,
// with an ellipsis if truncated.
func truncateLabel(s string) string {
	const limit = 80
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-3]) + "..."
}

// verdictText renders a verdict as a short human-readable string for an
// ephemeral reply to the button press.
func verdictText(ans policy.Answer) string {
	if !ans.Allow {
		return "denied"
	}
	switch ans.Scope {
	case policy.ScopeOnce:
		return "allowed once"
	case policy.ScopeSession:
		return "allowed for this session"
	case policy.ScopePattern:
		return "added to policy"
	default:
		return "allowed"
	}
}

// approvalMessage renders an approval request as a message with buttons.
func approvalMessage(sessionID string, ev daemon.WireEvent) Message {
	// Build content
	content := "**spore wants to run " + ev.Tool + "**"
	if ev.Rule != "" {
		content += "\nmatched policy rule: " + ev.Rule
	}

	// Build embed with arguments
	embeds := []Embed{{
		Title:       "Arguments",
		Description: "```\n" + ev.Args + "\n```",
	}}

	// Build buttons
	buttons := []Button{
		{CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopeOnce), Label: "allow once"},
		{CustomID: encodeCustomID(sessionID, ev.PendingID, false, policy.ScopeOnce), Label: "deny", Danger: true},
		// Say what this actually does. It approves the TOOL for the rest of
		// the session, not these arguments.
		{CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopeSession),
			Label: truncateLabel("allow " + ev.Tool + " for this session")},
	}
	// An empty pattern is the guard saying there is nothing to generalise to.
	// Offering the button anyway would put a one-tap blanket allow for the
	// whole tool on a phone screen.
	if ev.Pattern != "" {
		buttons = append(buttons, Button{
			CustomID: encodeCustomID(sessionID, ev.PendingID, true, policy.ScopePattern),
			Label:    truncateLabel("always allow " + ev.Pattern),
		})
	}

	return Message{
		Content: content,
		Embeds:  embeds,
		Buttons: buttons,
	}
}

// answerer resolves an approval on behalf of a Discord button press.
type answerer struct {
	broker *daemon.Broker
	guard  *policy.Guard
}

// newAnswerer constructs an answerer. A nil guard is legal and means "no
// out-of-band path" — if no live waiter exists, an error is returned.
func newAnswerer(broker *daemon.Broker, guard *policy.Guard) *answerer {
	return &answerer{broker: broker, guard: guard}
}

// answer delivers a Discord button press to the suspended turn. It takes the
// same two paths the HTTP handler takes and in the same order: a live waiter
// is the normal case, and Guard.Resolve is only for a suspension whose turn
// is gone. Taking both would write two audit rows for one decision.
//
// The sessionID comes from the button, and both Broker.Answer and
// Guard.Resolve verify it against the suspension's own session — which is
// what stops one session answering another's approvals.
func (a *answerer) answer(ctx context.Context, sessionID string, pendingID int64, ans policy.Answer) (string, error) {
	if a.broker.Answer(sessionID, pendingID, ans) {
		return verdictText(ans), nil
	}
	if a.broker.AlreadyAnswered(pendingID) {
		return "", fmt.Errorf("that approval was already answered")
	}
	if a.guard == nil {
		return "", fmt.Errorf("no approval %d is waiting", pendingID)
	}
	if err := a.guard.Resolve(ctx, sessionID, pendingID, ans); err != nil {
		return "", err
	}
	return verdictText(ans) + " (recorded after a restart)", nil
}
