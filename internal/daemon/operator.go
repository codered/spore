package daemon

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/codered/spore/internal/mcp"
	"github.com/codered/spore/internal/memory"
	"github.com/codered/spore/internal/policy"
	"github.com/codered/spore/internal/recall"
)

// FactIndexer keeps the recall index in step with a deleted fact file.
// *store.Store implements it.
type FactIndexer interface {
	UnindexFact(ctx context.Context, name string) error
}

// Operator is what the MCP, policy and memory views act on. Every field may
// be nil; the routes that need it then answer 503. None of it is ever handed
// to the Discord bridge: these are the local operator's controls.
type Operator struct {
	MCP       *mcp.Host
	Policy    *policy.Reloader
	Facts     *memory.Cache
	FactIndex FactIndexer
	Recall    recall.Recall
}

// AttachOperator supplies the operator's subsystems. Like AttachRefiner, it
// is called while the daemon is wired, before it serves.
func (s *Server) AttachOperator(o Operator) { s.op = o }

// audit logs one state-changing operator action. The actor is the client
// that asked, from the X-Spore-Client header, so the TUI and a later web view
// log as themselves.
func audit(r *http.Request, action, target string, kv ...any) {
	actor := r.Header.Get("X-Spore-Client")
	if actor == "" {
		actor = "http"
	}
	if len(actor) > 32 {
		actor = actor[:32]
	}
	args := append([]any{"actor", actor, "action", action, "target", target}, kv...)
	slog.Info("operator action", args...)
}
