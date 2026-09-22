package daemon

import (
	"github.com/codered/spore/internal/agent"
	"github.com/codered/spore/internal/store"
)

// childPublisher puts sub-agents on the hub under their own session ids, so
// the global feed streams a child exactly as it streams any other session.
type childPublisher struct{ s *Server }

func (p childPublisher) ChildStarted(parentID, childID, prompt string) {
	ws := ""
	if sess, ok, err := p.s.store.Session(p.s.base, childID); err == nil && ok {
		ws = sess.Workspace
	}
	p.s.hub.Publish(childID, WireEvent{
		Type: WireSession, Title: prompt, Workspace: ws,
		Source: store.SourceSubagent, ParentID: parentID,
	})
	p.s.hub.Publish(childID, WireEvent{Type: WireTurnStarted})
}

func (p childPublisher) ChildEvent(childID string, ev agent.Event) {
	p.s.hub.Publish(childID, FromAgent(ev))
}

func (p childPublisher) ChildSettled(childID, state string) {
	p.s.hub.Publish(childID, WireEvent{Type: WireAgentState, State: state})
}
