package discord

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// detailsPrefix namespaces the "show details" button. It is deliberately a
// different shape from the approval custom id decodeCustomID parses (five
// pipe-separated fields), so handleInteraction can tell the two kinds apart
// by prefix alone and neither decoder ever sees the other's ids.
const detailsPrefix = customIDPrefix + "|tools|"

// detailsKept bounds how many turns' transcripts are held in memory. A
// transcript is a convenience, not a record — the store already has the real
// one, and `spore trace` reads it — so the oldest is dropped rather than
// letting a long-lived daemon grow without limit.
const detailsKept = 50

// toolEntry is one tool call and what it returned, as the details view shows
// it. It is the renderer's to fill: Result and IsError arrive on a separate
// event from Name and Args.
type toolEntry struct {
	Name    string
	Args    string
	Result  string
	IsError bool
}

// details holds the recent turns' tool transcripts, keyed by the id carried
// in the button's custom id. The renderer that recorded a transcript is gone
// by the time the button is pressed — its goroutine ends with the turn — so
// the transcripts outlive it here, on the Bridge.
//
// Ids are minted with a per-process random prefix. After a restart the map is
// empty, and a button on a message from the previous process must not collide
// with a recycled id and hand somebody a different turn's transcript: an
// unknown prefix simply misses, and the press is answered with "expired".
type details struct {
	mu    sync.Mutex
	run   string
	next  int
	byID  map[string][]toolEntry
	order []string
}

// newDetails seeds the per-process id prefix. A failed read from crypto/rand
// is not fatal: the prefix only has to be unlikely to repeat across restarts,
// and a fixed fallback degrades the expiry check, not correctness of anything
// that can be acted on — the worst case is showing a stale transcript for a
// turn number that happens to match.
func newDetails() *details {
	var b [4]byte
	run := "0"
	if _, err := rand.Read(b[:]); err == nil {
		run = hex.EncodeToString(b[:])
	}
	return &details{run: run, byID: make(map[string][]toolEntry)}
}

// mintID returns a fresh id for one turn's transcript.
func (d *details) mintID() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	return fmt.Sprintf("%s-%d", d.run, d.next)
}

// put records (or replaces) a turn's transcript, evicting the oldest once
// detailsKept turns are held.
func (d *details) put(id string, entries []toolEntry) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, seen := d.byID[id]; !seen {
		d.order = append(d.order, id)
		for len(d.order) > detailsKept {
			delete(d.byID, d.order[0])
			d.order = d.order[1:]
		}
	}
	d.byID[id] = append([]toolEntry(nil), entries...)
}

// get returns a turn's transcript and whether it is still held.
func (d *details) get(id string) ([]toolEntry, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.byID[id]
	return e, ok
}

// detailsCustomID is the button id carrying a transcript id.
func detailsCustomID(id string) string { return detailsPrefix + id }

// isDetailsCustomID reports whether a press is a "show details" press.
func isDetailsCustomID(s string) bool { return strings.HasPrefix(s, detailsPrefix) }

// detailsIDFrom extracts the transcript id from a button id.
func detailsIDFrom(s string) string { return strings.TrimPrefix(s, detailsPrefix) }

// renderDetails formats a transcript for the ephemeral reply a press gets.
// Discord caps a message at 2000 characters and this one cannot be split
// across several — an interaction gets one response — so it is built newest
// last and cut from the FRONT when it overflows, keeping the most recent
// calls, with a line saying where the whole thing lives.
func renderDetails(entries []toolEntry) string {
	if len(entries) == 0 {
		return "no tool calls in that turn"
	}
	blocks := make([]string, 0, len(entries))
	for _, e := range entries {
		var b strings.Builder
		mark := "⚙"
		if e.IsError {
			mark = "⚠"
		}
		b.WriteString(mark + " **" + e.Name + "**\n")
		if args := strings.TrimSpace(e.Args); args != "" {
			b.WriteString("```\n" + truncate(args, 500) + "\n```\n")
		}
		if res := strings.TrimSpace(e.Result); res != "" {
			b.WriteString("```\n" + truncate(res, 500) + "\n```\n")
		}
		blocks = append(blocks, b.String())
	}

	const note = "_earlier calls trimmed; `spore trace` has the whole turn_\n"
	out := strings.Join(blocks, "")
	if len(out) <= messageLimit {
		return out
	}
	for i := 1; i < len(blocks); i++ {
		out = note + strings.Join(blocks[i:], "")
		if len(out) <= messageLimit {
			return out
		}
	}
	// Even the newest call alone overflows: cut it, rune-safely.
	head, _ := splitAt(note+blocks[len(blocks)-1], messageLimit)
	return head
}
