package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/codered/spore/internal/daemon"
	"github.com/codered/spore/internal/provider"
	"github.com/codered/spore/internal/store"
)

// sessionView is everything the interface knows about one session.
type sessionView struct {
	info   daemon.SessionJSON
	blocks []*block
	// loaded is true once the transcript has been fetched. Events for a
	// session that is not loaded still apply, so its sidebar state is live,
	// and the fetch replaces the blocks when it lands.
	loaded  bool
	working bool
	// approvals waiting to be answered through this session. A sub-agent's
	// approval lives on its root, with Origin naming the child.
	approvals []daemon.WireEvent
	model     string
	ctxTokens int
	cost      float64
	started   time.Time
}

func (sv *sessionView) last() *block {
	if len(sv.blocks) == 0 {
		return nil
	}
	return sv.blocks[len(sv.blocks)-1]
}

func (sv *sessionView) endStreaming() {
	if b := sv.last(); b != nil && b.kind == kindText && b.streaming {
		b.streaming = false
		b.touch()
	}
}

func (sv *sessionView) tool(id string) *block {
	for i := len(sv.blocks) - 1; i >= 0; i-- {
		if b := sv.blocks[i]; b.kind == kindTool && b.toolID == id {
			return b
		}
	}
	return nil
}

func (sv *sessionView) tools() []*block {
	var out []*block
	for _, b := range sv.blocks {
		if b.kind == kindTool {
			out = append(out, b)
		}
	}
	return out
}

func (sv *sessionView) add(kind blockKind, text string) {
	sv.endStreaming()
	sv.blocks = append(sv.blocks, &block{kind: kind, text: text})
}

func (sv *sessionView) addUser(text string) { sv.add(kindUser, text) }

// dropLastUser removes a user block that was shown optimistically for a send
// the daemon refused, so re-sending it does not show it twice.
func (sv *sessionView) dropLastUser(text string) {
	for i := len(sv.blocks) - 1; i >= 0; i-- {
		if b := sv.blocks[i]; b.kind == kindUser && b.text == text {
			sv.blocks = append(sv.blocks[:i], sv.blocks[i+1:]...)
			return
		}
	}
}

func (sv *sessionView) dropApproval(pendingID int64) {
	for i, a := range sv.approvals {
		if a.PendingID == pendingID {
			sv.approvals = append(sv.approvals[:i], sv.approvals[i+1:]...)
			return
		}
	}
}

func (sv *sessionView) dropApprovalsWhere(match func(daemon.WireEvent) bool) {
	kept := sv.approvals[:0]
	for _, a := range sv.approvals {
		if !match(a) {
			kept = append(kept, a)
		}
	}
	sv.approvals = kept
}

// cache holds every session the interface has heard of. Apply is its only
// writer of live state and performs no I/O, so every ordering question about
// the transcript is testable without Bubble Tea.
type cache struct {
	sessions map[string]*sessionView
	// answered holds approvals this terminal answered, so their `resolved`
	// echo is not reported as answered elsewhere.
	answered map[int64]bool
	now      func() time.Time
	showCost bool
	// jobsOpen is whether the sidebar's jobs folder is expanded.
	jobsOpen bool
}

func newCache(now func() time.Time) *cache {
	return &cache{sessions: map[string]*sessionView{}, answered: map[int64]bool{}, now: now}
}

func (c *cache) get(id string) *sessionView {
	sv, ok := c.sessions[id]
	if !ok {
		sv = &sessionView{info: daemon.SessionJSON{ID: id}}
		c.sessions[id] = sv
	}
	return sv
}

// Apply folds one event from the global feed into the cache and returns the
// session it changed.
func (c *cache) Apply(ev daemon.WireEvent) string {
	id := ev.Session
	sv := c.get(id)
	switch ev.Type {
	case daemon.WireTurnStarted:
		sv.working = true
		sv.started = c.now()
		sv.info.UpdatedAt = c.now()
	case daemon.WireText:
		sv.working = true
		if b := sv.last(); b != nil && b.kind == kindText && b.streaming {
			b.text += ev.Text
			b.touch()
		} else {
			sv.blocks = append(sv.blocks, &block{kind: kindText, text: ev.Text, streaming: true})
		}
	case daemon.WireToolCall:
		sv.endStreaming()
		sv.blocks = append(sv.blocks, &block{kind: kindTool, toolID: ev.ToolUseID, tool: ev.Tool, args: ev.Args})
	case daemon.WireToolResult:
		b := sv.tool(ev.ToolUseID)
		if b == nil {
			b = &block{kind: kindTool, toolID: ev.ToolUseID, tool: "tool"}
			sv.blocks = append(sv.blocks, b)
		}
		b.result, b.isError, b.truncated, b.done = ev.Content, ev.IsError, ev.Truncated, true
		b.touch()
	case daemon.WireApproval:
		for _, a := range sv.approvals {
			if a.PendingID == ev.PendingID {
				return id
			}
		}
		sv.approvals = append(sv.approvals, ev)
	case daemon.WireResolved:
		sv.dropApproval(ev.PendingID)
		if !c.answered[ev.PendingID] {
			sv.add(kindNotice, fmt.Sprintf("approval for %s answered elsewhere: %s", ev.Tool, ev.Decision))
		}
		delete(c.answered, ev.PendingID)
	case daemon.WireTurnDone:
		sv.endStreaming()
		sv.working = false
		if sv.info.Source == "job" {
			// Unread until someone opens it; the model clears it at once
			// when the run is the session on screen.
			sv.info.Unread = true
		}
		sv.model = ev.Model
		sv.ctxTokens = ev.TokensIn + ev.TokensCacheRead + ev.TokensCacheWrite
		sv.cost += ev.CostUSD
		sv.blocks = append(sv.blocks, &block{kind: kindFooter, text: c.footer(sv, ev)})
	case daemon.WireStopped:
		sv.working = false
		sv.dropApprovalsWhere(func(a daemon.WireEvent) bool { return a.Origin == "" })
		sv.add(kindNotice, "stopped")
	case daemon.WireError:
		sv.working = false
		sv.dropApprovalsWhere(func(a daemon.WireEvent) bool { return a.Origin == "" })
		sv.add(kindError, "turn failed: "+ev.Error)
	case daemon.WireJobNote:
		sv.add(kindNotice, ev.Text)
	case daemon.WireSession:
		sv.info.Title, sv.info.Workspace = ev.Title, ev.Workspace
		sv.info.Source, sv.info.ParentID = ev.Source, ev.ParentID
		sv.info.UpdatedAt = c.now()
	case daemon.WireAgentState:
		sv.working = ev.State == "running"
		if !sv.working {
			sv.endStreaming()
			// A settled child can no longer be waiting on anyone.
			for _, other := range c.sessions {
				other.dropApprovalsWhere(func(a daemon.WireEvent) bool { return a.Origin == id })
			}
		}
	}
	return id
}

func (c *cache) footer(sv *sessionView, ev daemon.WireEvent) string {
	s := fmt.Sprintf("%s · ctx %s · %d out", ev.Model, humanTokens(sv.ctxTokens), ev.TokensOut)
	if c.showCost {
		s += fmt.Sprintf(" · $%.4f", ev.CostUSD)
	}
	if !sv.started.IsZero() {
		s += fmt.Sprintf(" · %.1fs", c.now().Sub(sv.started).Seconds())
	}
	return s
}

// SetSessions records what the daemon reports about each session, keeping
// the transcripts already loaded.
func (c *cache) SetSessions(list []daemon.SessionJSON) {
	for _, s := range list {
		sv := c.get(s.ID)
		sv.info = s
		sv.working = s.State == daemon.SessionWorking || s.State == daemon.SessionBlocked
	}
}

// SetTranscript replaces a session's blocks with what the daemon persisted.
// Text that was mid-stream and not yet persisted is lost from view until the
// daemon writes it; events after the fetch continue from there.
func (c *cache) SetTranscript(t daemon.TranscriptJSON) {
	sv := c.get(t.Session.ID)
	state, pending := sv.info.State, sv.info.Pending
	sv.info = t.Session
	sv.info.State, sv.info.Pending = state, pending
	sv.blocks = blocksFromMessages(t.Messages)
	sv.loaded = true
	sv.working = t.Running
	sv.cost = 0
	for _, m := range t.Messages {
		sv.cost += m.CostUSD
		// Only a message that carries usage says what the context was.
		if m.Role == string(provider.RoleAssistant) && m.Model != "" {
			sv.model = m.Model
			sv.ctxTokens = m.TokensIn + m.TokensCacheRead + m.TokensCacheWrite
		}
	}
}

func blocksFromMessages(msgs []daemon.MessageJSON) []*block {
	var out []*block
	byID := map[string]*block{}
	for _, m := range msgs {
		for _, b := range m.Blocks {
			switch {
			case b.Type == provider.BlockText && m.Role == store.RoleNote:
				out = append(out, &block{kind: kindNotice, text: b.Text})
			case b.Type == provider.BlockText && m.Role == string(provider.RoleUser) && strings.HasPrefix(b.Text, daemon.SchedulerTag):
				out = append(out, &block{kind: kindNotice, text: schedulerNotice(b.Text)})
			case b.Type == provider.BlockText && m.Role == string(provider.RoleUser):
				out = append(out, &block{kind: kindUser, text: b.Text})
			case b.Type == provider.BlockText:
				out = append(out, &block{kind: kindText, text: b.Text})
			case b.Type == provider.BlockToolUse:
				tb := &block{kind: kindTool, toolID: b.ID, tool: b.Name, args: string(b.Input)}
				byID[b.ID] = tb
				out = append(out, tb)
			case b.Type == provider.BlockToolResult:
				if tb, ok := byID[b.ID]; ok {
					tb.result, tb.isError, tb.truncated, tb.done = b.Content, b.IsError, b.Truncated, true
				}
			}
		}
	}
	return out
}

// schedulerNotice is how a message the scheduler wrote as the user reads in
// the transcript: its first sentence, which says what happened. The rest is
// instructions to the model, not to the person.
func schedulerNotice(text string) string {
	text = strings.TrimSpace(strings.TrimPrefix(text, daemon.SchedulerTag))
	for _, cut := range []string{" Its reply was:", " Tell the user"} {
		if i := strings.Index(text, cut); i >= 0 {
			text = text[:i]
		}
	}
	return "⏰ " + text
}

// State is blocked when an approval waits on the session (its own, or one
// its sub-agent asked through the root), working while a turn runs, and idle
// otherwise.
func (c *cache) State(id string) string {
	sv, ok := c.sessions[id]
	if !ok {
		return daemon.SessionIdle
	}
	if len(sv.approvals) > 0 {
		return daemon.SessionBlocked
	}
	for _, other := range c.sessions {
		for _, a := range other.approvals {
			if a.Origin == id {
				return daemon.SessionBlocked
			}
		}
	}
	if sv.working {
		return daemon.SessionWorking
	}
	return daemon.SessionIdle
}

// approvalFor is the approval to show while id is selected, and the session
// it must be answered through.
func (c *cache) approvalFor(id string) (root string, ev daemon.WireEvent, ok bool) {
	if sv, found := c.sessions[id]; found && len(sv.approvals) > 0 {
		return id, sv.approvals[0], true
	}
	for rid, sv := range c.sessions {
		for _, a := range sv.approvals {
			if a.Origin == id {
				return rid, a, true
			}
		}
	}
	return "", daemon.WireEvent{}, false
}

// BlockedCount is the number of approvals waiting anywhere.
func (c *cache) BlockedCount() int {
	n := 0
	for _, sv := range c.sessions {
		n += len(sv.approvals)
	}
	return n
}

// ClearApprovals forgets every approval. The global feed replays the live
// ones on connect, so a reconnect calls this first.
func (c *cache) ClearApprovals() {
	for _, sv := range c.sessions {
		sv.approvals = nil
	}
}

func (c *cache) markAnswered(root string, pendingID int64) {
	c.answered[pendingID] = true
	c.get(root).dropApproval(pendingID)
}

// pending reports whether the approval is still waiting under root.
func (c *cache) pending(root string, pendingID int64) bool {
	sv, ok := c.sessions[root]
	if !ok || c.answered[pendingID] {
		return false
	}
	for _, a := range sv.approvals {
		if a.PendingID == pendingID {
			return true
		}
	}
	return false
}

// restoreApproval puts back an approval whose answer failed to send, so the
// overlay stays up and the human can try again.
func (c *cache) restoreApproval(root string, ev daemon.WireEvent) {
	delete(c.answered, ev.PendingID)
	sv := c.get(root)
	sv.approvals = append([]daemon.WireEvent{ev}, sv.approvals...)
}

// remove forgets a deleted session.
func (c *cache) remove(id string) { delete(c.sessions, id) }

// descendants counts the sessions below id: its sub-agents, theirs, and so on.
func (c *cache) descendants(id string) int {
	n := 0
	for _, sv := range c.sessions {
		for p := sv.info.ParentID; p != ""; p = c.sessions[p].info.ParentID {
			if p == id {
				n++
				break
			}
			if c.sessions[p] == nil {
				break
			}
		}
	}
	return n
}
